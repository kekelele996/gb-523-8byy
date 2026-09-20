package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/audit"
	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
	"datacenter-thermal-capacity-planner/backend/internal/planner"
	"datacenter-thermal-capacity-planner/backend/internal/repository"
	"datacenter-thermal-capacity-planner/backend/internal/web"
)

type LayoutScenarioService struct {
	scenarios *repository.LayoutScenarioRepository
	zones     *repository.ThermalZoneRepository
	racks     *repository.RackRepository
	loads     *repository.EquipmentLoadRepository
	engine    *planner.Engine
}

func NewLayoutScenarioService(scenarios *repository.LayoutScenarioRepository, zones *repository.ThermalZoneRepository, racks *repository.RackRepository, loads *repository.EquipmentLoadRepository, engine *planner.Engine) *LayoutScenarioService {
	return &LayoutScenarioService{scenarios: scenarios, zones: zones, racks: racks, loads: loads, engine: engine}
}

func (s *LayoutScenarioService) List(ctx context.Context, search, status string, page, size int) ([]dto.ScenarioResponse, int64, error) {
	items, total, err := s.scenarios.List(ctx, search, status, page, size)
	if err != nil {
		return nil, 0, err
	}
	responses := make([]dto.ScenarioResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, dto.DecodeScenario(item))
	}
	return responses, total, nil
}

func (s *LayoutScenarioService) Get(ctx context.Context, id uint) (dto.ScenarioResponse, error) {
	item, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	return dto.DecodeScenario(item), nil
}

func (s *LayoutScenarioService) Create(ctx context.Context, req dto.CreateLayoutScenarioRequest, actor audit.Entry) (dto.ScenarioResponse, error) {
	if err := req.ValidateBusiness(); err != nil {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO", err.Error(), err)
	}
	frozen, err := s.captureFrozenInput(ctx, req.LoadIDs, time.Now().UTC())
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	snapshot, err := dto.EncodeFrozenInput(frozen)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode scenario input: %w", err))
	}
	frozenAt := frozen.FrozenAt
	item := model.LayoutScenario{
		Name: strings.TrimSpace(req.Name), ScenarioStatus: constants.ScenarioDraft,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: snapshot, InputFrozenAt: &frozenAt,
		InputDiffJSON: "{}", ZoneResultsJSON: "[]",
		ConstraintViolationsJSON: "[]", AlgorithmVersion: planner.AlgorithmVersion,
		Version: 1, CreatedBy: actor.ActorID,
	}
	actor.Action = "layout_scenario.create"
	actor.EntityType = "layout_scenario"
	actor.AfterSummary = fmt.Sprintf("draft loads=%v algorithm=%s frozen_at=%s", req.LoadIDs, planner.AlgorithmVersion, frozenAt.Format(time.RFC3339))
	if err := s.scenarios.Create(ctx, &item, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return dto.DecodeScenario(item), nil
}

func (s *LayoutScenarioService) Evaluate(ctx context.Context, id, version uint, actor audit.Entry) (dto.ScenarioResponse, error) {
	current, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	frozen, err := dto.DecodeFrozenInput(current.InputSnapshotJSON)
	if err != nil || len(frozen.LoadIDs) == 0 || len(frozen.Zones) == 0 || frozen.FrozenAt.IsZero() {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO_SNAPSHOT", "scenario is missing a frozen input and must be recreated", err)
	}
	actor.Action = "layout_scenario.evaluate.start"
	actor.EntityType = "layout_scenario"
	evaluating, err := s.scenarios.BeginEvaluation(ctx, id, version, actor)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	response, err := s.runEvaluation(ctx, evaluating, frozen, actor)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	return response, nil
}

// runEvaluation executes the deterministic engine against the frozen input and
// records the resulting pending-review scenario together with a fresh drift
// report. It is shared by draft evaluation and rebuild.
func (s *LayoutScenarioService) runEvaluation(ctx context.Context, evaluating model.LayoutScenario, frozen dto.FrozenScenarioInput, actor audit.Entry) (dto.ScenarioResponse, error) {
	result := s.engine.Evaluate(frozen.Zones, frozen.Racks, frozen.Loads)
	assignments, err := json.Marshal(result.Assignments)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode assignments: %w", err))
	}
	zoneResults, err := json.Marshal(result.ZoneResults)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode zone results: %w", err))
	}
	violations, err := json.Marshal(result.Violations)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode violations: %w", err))
	}
	snapshot, err := dto.EncodeFrozenInput(frozen)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode evaluation snapshot: %w", err))
	}
	liveZones, liveRacks, liveLoads, err := s.liveInputs(ctx, frozen.LoadIDs)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	inputDiff := BuildInputDiff(frozen, liveZones, liveRacks, liveLoads, time.Now().UTC())
	diffJSON, err := dto.EncodeScenarioInputDiff(inputDiff)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode input diff: %w", err))
	}
	update := repository.EvaluationUpdate{
		AssignmentsJSON: string(assignments), SnapshotJSON: snapshot, DiffJSON: diffJSON,
		ZoneResultsJSON: string(zoneResults), ViolationsJSON: string(violations),
		TotalPowerKW: result.TotalPower, PeakTempC: result.PeakTemp, Score: result.Score,
	}
	actor.Action = "layout_scenario.evaluate.finish"
	actor.EntityType = "layout_scenario"
	actor.BeforeSummary = fmt.Sprintf("algorithm=%s frozen_loads=%d frozen_at=%s", frozen.AlgorithmVersion, len(frozen.Loads), frozen.FrozenAt.Format(time.RFC3339))
	actor.AfterSummary = fmt.Sprintf("score=%.2f assignments=%d violations=%d drift=%s", result.Score, len(result.Assignments), len(result.Violations), inputDiff.Summary)
	if err := s.scenarios.FinishEvaluation(ctx, evaluating, update, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, evaluating.ID)
}

// Rebuild creates a replacement scenario frozen from the latest data and
// evaluates it immediately. The source stays pending review (and queryable)
// until the replacement row is committed; an engine failure rolls everything
// back so the source and its approval state are unchanged.
func (s *LayoutScenarioService) Rebuild(ctx context.Context, id, version uint, actor audit.Entry) (dto.ScenarioResponse, error) {
	source, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	previous, err := dto.DecodeFrozenInput(source.InputSnapshotJSON)
	if err != nil || len(previous.LoadIDs) == 0 {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO_SNAPSHOT", "scenario has no frozen load selection to rebuild", err)
	}
	frozen, err := s.captureFrozenInput(ctx, previous.LoadIDs, time.Now().UTC())
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	snapshot, err := dto.EncodeFrozenInput(frozen)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode rebuilt input: %w", err))
	}
	frozenAt := frozen.FrozenAt
	rebuilt := model.LayoutScenario{
		Name: s.rebuiltName(source.Name), ScenarioStatus: constants.ScenarioEvaluating,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: snapshot, InputFrozenAt: &frozenAt,
		InputDiffJSON: "{}", ZoneResultsJSON: "[]", ConstraintViolationsJSON: "[]",
		AlgorithmVersion: planner.AlgorithmVersion, Version: 1, CreatedBy: actor.ActorID,
		RebuiltFromID: &source.ID,
	}
	beginEntry := audit.Entry{
		RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
		Action: "layout_scenario.rebuild.start", EntityType: "layout_scenario",
		BeforeSummary: fmt.Sprintf("source=%d version=%d frozen_at=%s", source.ID, source.Version, formatFrozenAt(previous.FrozenAt)),
		AfterSummary:  fmt.Sprintf("rebuild of scenario %d with fresh inputs frozen_at=%s", source.ID, frozenAt.Format(time.RFC3339)),
	}
	supersedeEntry := audit.Entry{
		RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
		Action: "layout_scenario.rebuild.supersede", EntityType: "layout_scenario",
		BeforeSummary: string(source.ScenarioStatus),
		AfterSummary:  fmt.Sprintf("superseded by rebuild scenario (frozen_at=%s)", frozenAt.Format(time.RFC3339)),
	}
	if err := s.scenarios.BeginRebuild(ctx, id, version, &rebuilt, beginEntry, supersedeEntry); err != nil {
		return dto.ScenarioResponse{}, err
	}
	finishActor := audit.Entry{
		RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
	}
	response, err := s.runEvaluation(ctx, rebuilt, frozen, finishActor)
	if err != nil {
		discardEntry := audit.Entry{
			RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
			Action: "layout_scenario.rebuild.discard", EntityType: "layout_scenario",
			BeforeSummary: fmt.Sprintf("rebuild of scenario %d failed", source.ID),
			AfterSummary:  "source scenario restored unchanged",
		}
		_ = s.scenarios.DiscardRebuild(ctx, rebuilt.ID, source.ID, discardEntry)
		return dto.ScenarioResponse{}, err
	}
	return response, nil
}

func (s *LayoutScenarioService) Transition(ctx context.Context, id uint, req dto.TransitionScenarioRequest, actor audit.Entry) (dto.ScenarioResponse, error) {
	current, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	if current.Version != req.Version {
		return dto.ScenarioResponse{}, web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario was changed by another user", nil)
	}
	if !constants.ValidScenarioStatus(req.TargetStatus) || !constants.CanTransitionScenario(current.ScenarioStatus, req.TargetStatus) {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO_TRANSITION", fmt.Sprintf("cannot transition scenario from %s to %s", current.ScenarioStatus, req.TargetStatus), nil)
	}
	if req.TargetStatus == constants.ScenarioApproved {
		blocked, reason, err := s.approvalGuard(ctx, current, actor)
		if err != nil {
			return dto.ScenarioResponse{}, err
		}
		if blocked {
			return dto.ScenarioResponse{}, web.Unprocessable("SCENARIO_APPROVAL_BLOCKED", reason, nil)
		}
	}
	actor.Action = "layout_scenario.transition"
	if req.TargetStatus == constants.ScenarioApproved {
		actor.Action = "layout_scenario.approve"
	}
	actor.EntityType = "layout_scenario"
	actor.BeforeSummary = string(current.ScenarioStatus)
	actor.AfterSummary = fmt.Sprintf("%s reason=%s", req.TargetStatus, strings.TrimSpace(req.Reason))
	if err := s.scenarios.Transition(ctx, current, req.TargetStatus, actor.ActorID, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, id)
}

// approvalGuard recomputes drift against live data, refreshes the stored
// evidence (without touching status or version), and decides whether approval
// is blocked. A drift block can only be cleared by rebuilding.
func (s *LayoutScenarioService) approvalGuard(ctx context.Context, current model.LayoutScenario, actor audit.Entry) (bool, string, error) {
	if current.SupersededByID != nil {
		return true, fmt.Sprintf("scenario was superseded by rebuild #%d and can no longer be approved", *current.SupersededByID), nil
	}
	frozen, err := dto.DecodeFrozenInput(current.InputSnapshotJSON)
	if err != nil || len(frozen.LoadIDs) == 0 || frozen.FrozenAt.IsZero() {
		return true, "frozen input evidence is missing; rebuild the scenario to recapture inputs before approval", nil
	}
	liveZones, liveRacks, liveLoads, err := s.liveInputs(ctx, frozen.LoadIDs)
	if err != nil {
		return false, "", err
	}
	diff := BuildInputDiff(frozen, liveZones, liveRacks, liveLoads, time.Now().UTC())
	diffJSON, err := dto.EncodeScenarioInputDiff(diff)
	if err != nil {
		return false, "", web.Internal(fmt.Errorf("encode input diff: %w", err))
	}
	stored := dto.DecodeScenarioInputDiff(current.InputDiffJSON)
	if diffJSON != current.InputDiffJSON && (diff.HasChanges() || stored.HasChanges()) {
		entry := audit.Entry{
			RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
			Action: "layout_scenario.input_drift.refresh", EntityType: "layout_scenario",
			BeforeSummary: stored.Summary, AfterSummary: diff.Summary,
		}
		if refreshErr := s.scenarios.RefreshInputDiff(ctx, current.ID, diffJSON, entry); refreshErr != nil {
			return false, "", refreshErr
		}
	}
	if diff.HasChanges() {
		return true, "frozen inputs differ from current data (" + diff.Summary + "); rebuild the scenario from the latest data and re-evaluate before approval", nil
	}
	decoded := dto.DecodeScenario(current)
	if decoded.HasCriticalViolation {
		return true, "scenario cannot be approved while critical violations remain", nil
	}
	return false, "", nil
}

func (s *LayoutScenarioService) Compare(ctx context.Context, leftID, rightID uint) (dto.ScenarioComparison, error) {
	left, err := s.Get(ctx, leftID)
	if err != nil {
		return dto.ScenarioComparison{}, err
	}
	right, err := s.Get(ctx, rightID)
	if err != nil {
		return dto.ScenarioComparison{}, err
	}
	summary := []string{
		fmt.Sprintf("Score changed by %.2f points", right.Score-left.Score),
		fmt.Sprintf("Peak return temperature changed by %.2f C", right.PeakTempC-left.PeakTempC),
		fmt.Sprintf("Critical flag changed from %t to %t", left.HasCriticalViolation, right.HasCriticalViolation),
	}
	return dto.ScenarioComparison{
		Left: left, Right: right, ScoreDelta: right.Score - left.Score,
		PowerDeltaKW:  right.TotalPowerKW - left.TotalPowerKW,
		PeakTempDelta: right.PeakTempC - left.PeakTempC, Summary: summary,
	}, nil
}

func (s *LayoutScenarioService) captureFrozenInput(ctx context.Context, loadIDs []uint, frozenAt time.Time) (dto.FrozenScenarioInput, error) {
	loads, err := s.loads.FindByIDs(ctx, loadIDs)
	if err != nil {
		return dto.FrozenScenarioInput{}, err
	}
	if len(loads) != len(loadIDs) {
		return dto.FrozenScenarioInput{}, web.Unprocessable("LOAD_NOT_FOUND", "one or more selected loads no longer exist", nil)
	}
	for _, load := range loads {
		if !load.IsPlannable() {
			return dto.FrozenScenarioInput{}, web.Unprocessable("LOAD_NOT_READY", fmt.Sprintf("load %d is not ready for planning", load.ID), nil)
		}
	}
	zones, racks, loads, err := s.liveInputs(ctx, loadIDs)
	if err != nil {
		return dto.FrozenScenarioInput{}, err
	}
	return dto.FrozenScenarioInput{
		LoadIDs: append([]uint(nil), loadIDs...), AlgorithmVersion: planner.AlgorithmVersion,
		Zones: zones, Racks: racks, Loads: loads, FrozenAt: frozenAt,
	}, nil
}

func (s *LayoutScenarioService) liveInputs(ctx context.Context, loadIDs []uint) ([]model.ThermalZone, []model.Rack, []model.EquipmentLoad, error) {
	zones, err := s.zones.All(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	racks, err := s.racks.All(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	loads, err := s.loads.FindByIDs(ctx, loadIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	return zones, racks, loads, nil
}

func (s *LayoutScenarioService) rebuiltName(base string) string {
	const suffix = " (rebuilt)"
	name := strings.TrimSpace(base)
	if len(name)+len(suffix) > 120 {
		name = strings.TrimSpace(name[:120-len(suffix)])
	}
	return name + suffix
}

func formatFrozenAt(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.Format(time.RFC3339)
}
