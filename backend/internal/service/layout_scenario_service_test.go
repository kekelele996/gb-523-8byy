package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/audit"
	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
	"datacenter-thermal-capacity-planner/backend/internal/planner"
	"datacenter-thermal-capacity-planner/backend/internal/repository"
	"datacenter-thermal-capacity-planner/backend/internal/web"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type scenarioHarness struct {
	db        *gorm.DB
	service   *LayoutScenarioService
	scenarios *repository.LayoutScenarioRepository
	zones     *repository.ThermalZoneRepository
	racks     *repository.RackRepository
	loads     *repository.EquipmentLoadRepository
	zoneIDs   []uint
	rackIDs   []uint
	loadIDs   []uint
}

func newScenarioHarness(t *testing.T) *scenarioHarness {
	t.Helper()
	dsn := fmt.Sprintf("file:service-scenario-test-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ThermalZone{}, &model.Rack{}, &model.EquipmentLoad{}, &model.LayoutScenario{}, &audit.Event{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	auditRepo := audit.NewRepository(db)
	zoneRepo := repository.NewThermalZoneRepository(db, auditRepo)
	rackRepo := repository.NewRackRepository(db, auditRepo)
	loadRepo := repository.NewEquipmentLoadRepository(db, auditRepo)
	scenarioRepo := repository.NewLayoutScenarioRepository(db, auditRepo)
	h := &scenarioHarness{
		db:        db,
		scenarios: scenarioRepo,
		zones:     zoneRepo,
		racks:     rackRepo,
		loads:     loadRepo,
		service:   NewLayoutScenarioService(scenarioRepo, zoneRepo, rackRepo, loadRepo, planner.NewEngine(500)),
	}
	h.seed(t)
	return h
}

func (h *scenarioHarness) seed(t *testing.T) {
	t.Helper()
	zones := []model.ThermalZone{
		{ZoneCode: "TZ-A", Name: "A", CoolingCapacityKW: 120, SupplyTempC: 18, MaxReturnTempC: 32, AdjacencyJSON: `{}`, ZoneStatus: "active"},
		{ZoneCode: "TZ-B", Name: "B", CoolingCapacityKW: 120, SupplyTempC: 18, MaxReturnTempC: 32, AdjacencyJSON: `{}`, ZoneStatus: "active"},
	}
	if err := h.db.Create(&zones).Error; err != nil {
		t.Fatalf("seed zones: %v", err)
	}
	racks := []model.Rack{
		{ZoneID: zones[0].ID, RackCode: "A-01", RowIndex: 1, ColumnIndex: 1, PowerLimitKW: 24, AirflowLimitCFM: 7000, RackUnits: 42, RackStatus: constants.RackAvailable, Version: 1},
		{ZoneID: zones[1].ID, RackCode: "B-01", RowIndex: 2, ColumnIndex: 1, PowerLimitKW: 24, AirflowLimitCFM: 7000, RackUnits: 42, RackStatus: constants.RackAvailable, Version: 1},
	}
	if err := h.db.Create(&racks).Error; err != nil {
		t.Fatalf("seed racks: %v", err)
	}
	loads := []model.EquipmentLoad{
		{Name: "node-a", PowerKW: 10, HeatKW: 9, AirflowCFM: 2000, RackUnits: 8, RedundancyGroup: "RG-A", LoadStatus: "ready"},
		{Name: "node-b", PowerKW: 8, HeatKW: 7, AirflowCFM: 1600, RackUnits: 6, RedundancyGroup: "RG-B", LoadStatus: "ready"},
	}
	if err := h.db.Create(&loads).Error; err != nil {
		t.Fatalf("seed loads: %v", err)
	}
	for _, zone := range zones {
		h.zoneIDs = append(h.zoneIDs, zone.ID)
	}
	for _, rack := range racks {
		h.rackIDs = append(h.rackIDs, rack.ID)
	}
	for _, load := range loads {
		h.loadIDs = append(h.loadIDs, load.ID)
	}
}

func plannerActor() audit.Entry {
	return audit.Entry{RequestID: "req-1", ActorID: 1, ActorUsername: "planner"}
}

func reviewerActor() audit.Entry {
	return audit.Entry{RequestID: "req-2", ActorID: 2, ActorUsername: "reviewer", AfterSummary: "reviewed"}
}

func TestFrozenEvaluationIgnoresExternalEdits(t *testing.T) {
	h := newScenarioHarness(t)
	ctx := context.Background()

	draft, err := h.service.Create(ctx, dto.CreateLayoutScenarioRequest{Name: "freeze check", LoadIDs: h.loadIDs}, plannerActor())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if draft.FrozenAt == nil {
		t.Fatal("draft creation must record the freeze time")
	}

	// Shrink a rack after freezing. Evaluation must still use the frozen 24 kW.
	if err := h.db.Model(&model.Rack{}).Where("id = ?", h.rackIDs[0]).
		Updates(map[string]any{"power_limit_kw": 1, "version": gorm.Expr("version + 1")}).Error; err != nil {
		t.Fatalf("shrink rack: %v", err)
	}

	evaluated, err := h.service.Evaluate(ctx, draft.ID, draft.Version, plannerActor())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluated.ScenarioStatus != constants.ScenarioPendingReview {
		t.Fatalf("status=%s, want pending_review", evaluated.ScenarioStatus)
	}
	assignments := 0
	for _, assignment := range evaluated.Assignments {
		if assignment.RackCode == "A-01" {
			assignments++
		}
	}
	if assignments == 0 {
		t.Fatalf("frozen evaluation must still place loads on rack A-01 even though the live rack shrank: %+v", evaluated.Assignments)
	}
}

func TestApprovalBlockedByDriftThenRebuildAllowsApproval(t *testing.T) {
	h := newScenarioHarness(t)
	ctx := context.Background()

	draft, err := h.service.Create(ctx, dto.CreateLayoutScenarioRequest{Name: "drift workflow", LoadIDs: h.loadIDs}, plannerActor())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	evaluated, err := h.service.Evaluate(ctx, draft.ID, draft.Version, plannerActor())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// External edit after entering pending review.
	if err := h.db.Model(&model.ThermalZone{}).Where("id = ?", h.zoneIDs[0]).
		Update("cooling_capacity_kw", 40).Error; err != nil {
		t.Fatalf("edit zone: %v", err)
	}

	pending, err := h.service.Get(ctx, evaluated.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pending.InputDrift == nil || !pending.InputDrift.HasDrift || pending.InputDrift.ChangedCount != 1 {
		t.Fatalf("expected one changed value in drift report, got %+v", pending.InputDrift)
	}
	if pending.InputDrift.BlockingReason == "" {
		t.Fatal("drift report must include a blocking reason")
	}

	// Approval must be refused while drift exists.
	_, err = h.service.Transition(ctx, evaluated.ID, dto.TransitionScenarioRequest{TargetStatus: constants.ScenarioApproved, Version: pending.Version}, reviewerActor())
	if appErr, ok := err.(*web.AppError); !ok || appErr.Code != "INPUT_DRIFT_BLOCKED" {
		t.Fatalf("expected INPUT_DRIFT_BLOCKED, got %v", err)
	}

	// The blocked attempt must not change status or version.
	unchanged, err := h.scenarios.Get(ctx, evaluated.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if unchanged.ScenarioStatus != constants.ScenarioPendingReview || unchanged.ApprovedBy != nil || unchanged.Version != pending.Version {
		t.Fatalf("blocked approval mutated scenario: status=%s version=%d approved_by=%v", unchanged.ScenarioStatus, unchanged.Version, unchanged.ApprovedBy)
	}

	// Rebuild from latest data and re-evaluate.
	rebuilt, err := h.service.Rebuild(ctx, evaluated.ID, pending.Version, plannerActor())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rebuilt.ScenarioStatus != constants.ScenarioPendingReview {
		t.Fatalf("rebuilt scenario must enter pending review, got %s", rebuilt.ScenarioStatus)
	}
	if rebuilt.FrozenAt == nil || rebuilt.SourceScenarioID == nil || *rebuilt.SourceScenarioID != evaluated.ID {
		t.Fatal("rebuilt scenario must record freeze time and source scenario id")
	}
	if rebuilt.InputDrift == nil || rebuilt.InputDrift.HasDrift {
		t.Fatalf("freshly rebuilt scenario must have no drift, got %+v", rebuilt.InputDrift)
	}

	// Old scenario is archived but still queryable.
	oldView, err := h.service.Get(ctx, evaluated.ID)
	if err != nil {
		t.Fatalf("old scenario must remain queryable: %v", err)
	}
	if oldView.ScenarioStatus != constants.ScenarioArchived {
		t.Fatalf("old scenario status=%s, want archived", oldView.ScenarioStatus)
	}

	// Approval now succeeds.
	approved, err := h.service.Transition(ctx, rebuilt.ID, dto.TransitionScenarioRequest{TargetStatus: constants.ScenarioApproved, Version: rebuilt.Version}, reviewerActor())
	if err != nil {
		t.Fatalf("approve rebuilt: %v", err)
	}
	if approved.ScenarioStatus != constants.ScenarioApproved || approved.ApprovedBy == nil {
		t.Fatalf("rebuilt scenario not approved: %+v", approved)
	}
}

func TestRebuildStaleVersionRejected(t *testing.T) {
	h := newScenarioHarness(t)
	ctx := context.Background()

	draft, err := h.service.Create(ctx, dto.CreateLayoutScenarioRequest{Name: "stale rebuild", LoadIDs: h.loadIDs}, plannerActor())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	evaluated, err := h.service.Evaluate(ctx, draft.ID, draft.Version, plannerActor())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// Someone else rebuilds (or otherwise moves) the scenario first.
	first, err := h.service.Rebuild(ctx, evaluated.ID, evaluated.Version, plannerActor())
	if err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	_ = first

	_, err = h.service.Rebuild(ctx, evaluated.ID, evaluated.Version, plannerActor())
	if appErr, ok := err.(*web.AppError); !ok || appErr.Code != "SCENARIO_VERSION_CONFLICT" {
		t.Fatalf("expected stale rebuild conflict, got %v", err)
	}
}
