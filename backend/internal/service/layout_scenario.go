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
	"gorm.io/gorm"
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
		response := dto.DecodeScenario(item)
		if item.ScenarioStatus == constants.ScenarioPendingReview {
			report, err := s.driftForScenario(ctx, item)
			if err != nil {
				return nil, 0, err
			}
			response.InputDrift = report
		}
		responses = append(responses, response)
	}
	return responses, total, nil
}

func (s *LayoutScenarioService) Get(ctx context.Context, id uint) (dto.ScenarioResponse, error) {
	item, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	response := dto.DecodeScenario(item)
	if item.ScenarioStatus == constants.ScenarioPendingReview {
		report, err := s.driftForScenario(ctx, item)
		if err != nil {
			return dto.ScenarioResponse{}, err
		}
		response.InputDrift = report
	}
	return response, nil
}

func (s *LayoutScenarioService) Create(ctx context.Context, req dto.CreateLayoutScenarioRequest, actor audit.Entry) (dto.ScenarioResponse, error) {
	if err := req.ValidateBusiness(); err != nil {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO", err.Error(), err)
	}
	loads, err := s.loads.FindByIDs(ctx, req.LoadIDs)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	for _, load := range loads {
		if !load.IsPlannable() {
			return dto.ScenarioResponse{}, web.Unprocessable("LOAD_NOT_READY", fmt.Sprintf("load %d is not ready for planning", load.ID), nil)
		}
	}
	zones, err := s.zones.All(ctx)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	racks, err := s.racks.All(ctx)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	frozenAt := time.Now()
	snapshot, err := encodeFrozenInput(req.LoadIDs, zones, racks, loads, planner.AlgorithmVersion, frozenAt)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(err)
	}
	item := model.LayoutScenario{
		Name: strings.TrimSpace(req.Name), ScenarioStatus: constants.ScenarioDraft,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: snapshot, ZoneResultsJSON: "[]",
		ConstraintViolationsJSON: "[]", AlgorithmVersion: planner.AlgorithmVersion,
		Version: 1, FrozenAt: &frozenAt, CreatedBy: actor.ActorID,
	}
	actor.Action = "layout_scenario.create"
	actor.EntityType = "layout_scenario"
	actor.AfterSummary = fmt.Sprintf("draft loads=%v algorithm=%s frozen_at=%s", req.LoadIDs, planner.AlgorithmVersion, frozenAt.UTC().Format(time.RFC3339))
	if err := s.scenarios.Create(ctx, &item, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, item.ID)
}

func (s *LayoutScenarioService) Evaluate(ctx context.Context, id, version uint, actor audit.Entry) (dto.ScenarioResponse, error) {
	current, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	input, complete := dto.DecodeFrozenInput(current.InputSnapshotJSON)
	var refreshedSnapshot string
	var refreshedFrozenAt *time.Time
	if !complete {
		// Legacy draft created before full input freezing: freeze the current
		// inputs once at evaluation time so the result stays reproducible.
		liveLoads, loadErr := s.loads.FindByIDs(ctx, legacyLoadIDs(current.InputSnapshotJSON))
		if loadErr != nil {
			return dto.ScenarioResponse{}, loadErr
		}
		liveZones, zoneErr := s.zones.All(ctx)
		if zoneErr != nil {
			return dto.ScenarioResponse{}, zoneErr
		}
		liveRacks, rackErr := s.racks.All(ctx)
		if rackErr != nil {
			return dto.ScenarioResponse{}, rackErr
		}
		frozenAt := time.Now()
		refreshed, encodeErr := encodeFrozenInput(input.LoadIDs, liveZones, liveRacks, liveLoads, planner.AlgorithmVersion, frozenAt)
		if encodeErr != nil {
			return dto.ScenarioResponse{}, web.Internal(encodeErr)
		}
		refreshedSnapshot = refreshed
		refreshedFrozenAt = &frozenAt
		input, _ = dto.DecodeFrozenInput(refreshed)
	}
	if len(input.LoadIDs) == 0 || len(input.Loads) == 0 {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO_SNAPSHOT", "scenario input snapshot cannot be evaluated", nil)
	}
	// The planner always consumes the frozen snapshot, never the live tables.
	zones, racks, loads := frozenInputToModels(input)

	actor.Action = "layout_scenario.evaluate.start"
	actor.EntityType = "layout_scenario"
	evaluating, err := s.scenarios.BeginEvaluation(ctx, id, version, refreshedSnapshot, refreshedFrozenAt, actor)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	result := s.engine.Evaluate(zones, racks, loads)
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
	update := repository.EvaluationUpdate{
		AssignmentsJSON: string(assignments),
		ZoneResultsJSON: string(zoneResults), ViolationsJSON: string(violations),
		TotalPowerKW: result.TotalPower, PeakTempC: result.PeakTemp, Score: result.Score,
	}
	finishActor := audit.Entry{RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
		Action: "layout_scenario.evaluate.finish", EntityType: "layout_scenario",
		BeforeSummary: fmt.Sprintf("algorithm=%s frozen_loads=%d frozen_at=%s", input.AlgorithmVersion, len(input.Loads), input.FrozenAt.UTC().Format(time.RFC3339)),
		AfterSummary:  fmt.Sprintf("score=%.2f assignments=%d violations=%d", result.Score, len(result.Assignments), len(result.Violations)),
	}
	if err := s.scenarios.FinishEvaluation(ctx, evaluating, update, finishActor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, id)
}

// legacyLoadIDs extracts the selected load ids from a pre-freeze snapshot.
func legacyLoadIDs(raw string) []uint {
	var minimal struct {
		LoadIDs []uint `json:"load_ids"`
	}
	_ = json.Unmarshal([]byte(raw), &minimal)
	return minimal.LoadIDs
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
	decoded := dto.DecodeScenario(current)
	if req.TargetStatus == constants.ScenarioApproved {
		if decoded.HasCriticalViolation {
			return dto.ScenarioResponse{}, web.Unprocessable("CRITICAL_VIOLATIONS", "scenario cannot be approved while critical violations remain", nil)
		}
		if err := s.approve(ctx, current, actor); err != nil {
			return dto.ScenarioResponse{}, err
		}
		return s.Get(ctx, id)
	}
	actor.Action = "layout_scenario.transition"
	actor.EntityType = "layout_scenario"
	actor.BeforeSummary = string(current.ScenarioStatus)
	actor.AfterSummary = fmt.Sprintf("%s reason=%s", req.TargetStatus, strings.TrimSpace(req.Reason))
	if err := s.scenarios.Transition(ctx, current, req.TargetStatus, actor.ActorID, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, id)
}

// approve runs the authoritative frozen-versus-current input check inside a
// locked transaction and only approves when the scenario is still consistent.
func (s *LayoutScenarioService) approve(ctx context.Context, current model.LayoutScenario, actor audit.Entry) error {
	frozen, ok := dto.DecodeFrozenInput(current.InputSnapshotJSON)
	if !ok {
		return web.Unprocessable("INVALID_SCENARIO_SNAPSHOT", "scenario lacks a frozen input snapshot and cannot be approved", nil)
	}
	verify := func(verifyCtx context.Context, tx *gorm.DB) error {
		liveZones, err := s.zones.AllLockedTx(verifyCtx, tx)
		if err != nil {
			return err
		}
		liveRacks, err := s.racks.AllLockedTx(verifyCtx, tx)
		if err != nil {
			return err
		}
		liveLoads, err := s.loads.FindByIDsLockedTx(verifyCtx, tx, frozen.LoadIDs)
		if err != nil {
			return err
		}
		report := CompareFrozenInput(frozen, liveZones, liveRacks, liveLoads)
		if report.HasDrift {
			return web.Unprocessable("INPUT_DRIFT_BLOCKED", report.BlockingReason, nil)
		}
		return nil
	}
	approveActor := audit.Entry{RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
		Action: "layout_scenario.approve", EntityType: "layout_scenario",
		AfterSummary: fmt.Sprintf("approved reason=%s", strings.TrimSpace(actor.AfterSummary)),
	}
	return s.scenarios.Approve(ctx, current, verify, actor.ActorID, approveActor)
}

// Rebuild discards an obsolete pending-review result, rebuilds the frozen
// snapshot from the latest live data, re-runs the planner and returns the new
// pending-review scenario. The source scenario is archived in the same
// transaction and stays queryable. Concurrent rebuilds serialize; exactly one
// can succeed, and failures leave both the original scenario and approval
// state untouched.
func (s *LayoutScenarioService) Rebuild(ctx context.Context, id, version uint, actor audit.Entry) (dto.ScenarioResponse, error) {
	build := func(buildCtx context.Context, tx *gorm.DB, source model.LayoutScenario) (model.LayoutScenario, []audit.Entry, error) {
		frozen, ok := dto.DecodeFrozenInput(source.InputSnapshotJSON)
		if !ok || len(frozen.LoadIDs) == 0 {
			return model.LayoutScenario{}, nil, web.Unprocessable("INVALID_SCENARIO_SNAPSHOT", "scenario lacks a frozen input snapshot and cannot be rebuilt", nil)
		}
		liveZones, err := s.zones.AllTx(buildCtx, tx)
		if err != nil {
			return model.LayoutScenario{}, nil, err
		}
		liveRacks, err := s.racks.AllTx(buildCtx, tx)
		if err != nil {
			return model.LayoutScenario{}, nil, err
		}
		liveLoads, err := s.loads.FindByIDsTx(buildCtx, tx, frozen.LoadIDs)
		if err != nil {
			return model.LayoutScenario{}, nil, err
		}
		for _, load := range liveLoads {
			if !load.IsPlannable() {
				return model.LayoutScenario{}, nil, web.Unprocessable("LOAD_NOT_READY", fmt.Sprintf("selected load %d is no longer ready for planning; create a new scenario", load.ID), nil)
			}
		}
		frozenAt := time.Now()
		snapshot, err := encodeFrozenInput(frozen.LoadIDs, liveZones, liveRacks, liveLoads, planner.AlgorithmVersion, frozenAt)
		if err != nil {
			return model.LayoutScenario{}, nil, web.Internal(err)
		}
		plannerZones, plannerRacks, plannerLoads := frozenInputToModels(dto.FrozenInputFromModels(frozen.LoadIDs, liveZones, liveRacks, liveLoads, planner.AlgorithmVersion, frozenAt))
		result := s.engine.Evaluate(plannerZones, plannerRacks, plannerLoads)
		assignments, err := json.Marshal(result.Assignments)
		if err != nil {
			return model.LayoutScenario{}, nil, web.Internal(fmt.Errorf("encode assignments: %w", err))
		}
		zoneResults, err := json.Marshal(result.ZoneResults)
		if err != nil {
			return model.LayoutScenario{}, nil, web.Internal(fmt.Errorf("encode zone results: %w", err))
		}
		violations, err := json.Marshal(result.Violations)
		if err != nil {
			return model.LayoutScenario{}, nil, web.Internal(fmt.Errorf("encode violations: %w", err))
		}
		newScenario := model.LayoutScenario{
			Name: rebuiltName(source.Name, frozenAt), ScenarioStatus: constants.ScenarioPendingReview,
			RackAssignmentsJSON: string(assignments), InputSnapshotJSON: snapshot,
			ZoneResultsJSON: string(zoneResults), ConstraintViolationsJSON: string(violations),
			TotalPowerKW: result.TotalPower, PeakTempC: result.PeakTemp, Score: result.Score,
			AlgorithmVersion: planner.AlgorithmVersion, Version: 1, FrozenAt: &frozenAt,
			SourceScenarioID: &source.ID, CreatedBy: actor.ActorID,
		}
		entry := audit.Entry{RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
			Action: "layout_scenario.rebuild", EntityType: "layout_scenario",
			AfterSummary: fmt.Sprintf("rebuilt from #%d score=%.2f assignments=%d violations=%d frozen_at=%s", source.ID, result.Score, len(result.Assignments), len(result.Violations), frozenAt.UTC().Format(time.RFC3339)),
		}
		return newScenario, []audit.Entry{entry}, nil
	}
	archiveEntry := audit.Entry{RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
		Action: "layout_scenario.rebuild.archive", EntityType: "layout_scenario"}
	rebuilt, err := s.scenarios.Rebuild(ctx, id, version, build, archiveEntry)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, rebuilt.ID)
}

// rebuiltName produces a unique, length-safe name for the replacement scenario.
func rebuiltName(source string, at time.Time) string {
	suffix := " rebuilt " + at.UTC().Format("20060102T150405")
	name := strings.TrimSpace(source)
	limit := 120 - len(suffix)
	if len(name) > limit {
		name = name[:limit]
	}
	return name + suffix
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

// driftForScenario compares a pending scenario's frozen inputs against the
// current live zones, racks and selected loads.
func (s *LayoutScenarioService) driftForScenario(ctx context.Context, scenario model.LayoutScenario) (*dto.InputDriftReport, error) {
	frozen, ok := dto.DecodeFrozenInput(scenario.InputSnapshotJSON)
	if !ok {
		return &dto.InputDriftReport{Entries: []dto.InputDriftEntry{}}, nil
	}
	liveZones, err := s.zones.All(ctx)
	if err != nil {
		return nil, err
	}
	liveRacks, err := s.racks.All(ctx)
	if err != nil {
		return nil, err
	}
	liveLoads, err := s.loads.FindByIDs(ctx, frozen.LoadIDs)
	if err != nil {
		// A deleted selected load is itself a drift, not a failure to report.
		allLoads, loadErr := s.loads.All(ctx)
		if loadErr != nil {
			return nil, loadErr
		}
		report := CompareFrozenInput(frozen, liveZones, liveRacks, allLoads)
		return &report, nil
	}
	report := CompareFrozenInput(frozen, liveZones, liveRacks, liveLoads)
	return &report, nil
}
