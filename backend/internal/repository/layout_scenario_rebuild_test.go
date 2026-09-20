package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/audit"
	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newScenarioTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:scenario-test-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ThermalZone{}, &model.Rack{}, &model.EquipmentLoad{}, &model.LayoutScenario{}, &audit.Event{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedScenarioInputs(t *testing.T, db *gorm.DB) ([]model.ThermalZone, []model.Rack, []model.EquipmentLoad) {
	t.Helper()
	zones := []model.ThermalZone{
		{ZoneCode: "TZ-A", Name: "A", CoolingCapacityKW: 100, SupplyTempC: 18, MaxReturnTempC: 32, AdjacencyJSON: `{}`, ZoneStatus: "active"},
		{ZoneCode: "TZ-B", Name: "B", CoolingCapacityKW: 100, SupplyTempC: 18, MaxReturnTempC: 32, AdjacencyJSON: `{}`, ZoneStatus: "active"},
	}
	if err := db.Create(&zones).Error; err != nil {
		t.Fatalf("seed zones: %v", err)
	}
	racks := []model.Rack{
		{ZoneID: zones[0].ID, RackCode: "A-01", RowIndex: 1, ColumnIndex: 1, PowerLimitKW: 24, AirflowLimitCFM: 7000, RackUnits: 42, RackStatus: constants.RackAvailable, Version: 1},
		{ZoneID: zones[1].ID, RackCode: "B-01", RowIndex: 2, ColumnIndex: 1, PowerLimitKW: 24, AirflowLimitCFM: 7000, RackUnits: 42, RackStatus: constants.RackAvailable, Version: 1},
	}
	if err := db.Create(&racks).Error; err != nil {
		t.Fatalf("seed racks: %v", err)
	}
	loads := []model.EquipmentLoad{
		{Name: "node-a", PowerKW: 10, HeatKW: 9, AirflowCFM: 2000, RackUnits: 8, RedundancyGroup: "RG", LoadStatus: "ready"},
	}
	if err := db.Create(&loads).Error; err != nil {
		t.Fatalf("seed loads: %v", err)
	}
	return zones, racks, loads
}

func testEntry(action string) audit.Entry {
	return audit.Entry{RequestID: "req-test", ActorID: 1, ActorUsername: "planner", Action: action, EntityType: "layout_scenario"}
}

// TestRebuildConcurrentOnlyOneSucceeds starts several rebuilds against the
// same pending scenario. Exactly one must insert a replacement and archive the
// source; the others must fail without changing either state.
func TestRebuildConcurrentOnlyOneSucceeds(t *testing.T) {
	db := newScenarioTestDB(t)
	auditRepo := audit.NewRepository(db)
	scenarioRepo := NewLayoutScenarioRepository(db, auditRepo)
	ctx := context.Background()

	_, _, _ = seedScenarioInputs(t, db)
	frozenAt := time.Now()
	source := model.LayoutScenario{
		Name: "source scenario", ScenarioStatus: constants.ScenarioPendingReview,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: `{"load_ids":[]}`,
		ZoneResultsJSON: "[]", ConstraintViolationsJSON: "[]",
		AlgorithmVersion: "thermal-v1", Version: 3, FrozenAt: &frozenAt, CreatedBy: 1,
	}
	if err := scenarioRepo.Create(ctx, &source, testEntry("layout_scenario.create")); err != nil {
		t.Fatalf("create source: %v", err)
	}

	build := func(_ context.Context, tx *gorm.DB, src model.LayoutScenario) (model.LayoutScenario, []audit.Entry, error) {
		at := time.Now()
		return model.LayoutScenario{
			Name: fmt.Sprintf("rebuilt %d", at.UnixNano()), ScenarioStatus: constants.ScenarioPendingReview,
			RackAssignmentsJSON: "[]", InputSnapshotJSON: `{"load_ids":[]}`,
			ZoneResultsJSON: "[]", ConstraintViolationsJSON: "[]",
			AlgorithmVersion: "thermal-v1", Version: 1, FrozenAt: &at,
			SourceScenarioID: &src.ID, CreatedBy: 1,
		}, []audit.Entry{testEntry("layout_scenario.rebuild")}, nil
	}

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := scenarioRepo.Rebuild(ctx, source.ID, source.Version, build, testEntry("layout_scenario.rebuild.archive"))
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		default:
			conflicts++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one successful rebuild, got %d (conflicts=%d)", successes, conflicts)
	}
	if conflicts != workers-1 {
		t.Fatalf("expected %d losing rebuilds, got %d", workers-1, conflicts)
	}

	var archived model.LayoutScenario
	if err := db.First(&archived, source.ID).Error; err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if archived.ScenarioStatus != constants.ScenarioArchived {
		t.Fatalf("source should be archived, got %s", archived.ScenarioStatus)
	}
	var rebuiltCount int64
	if err := db.Model(&model.LayoutScenario{}).Where("source_scenario_id = ?", source.ID).Count(&rebuiltCount).Error; err != nil {
		t.Fatalf("count rebuilt: %v", err)
	}
	if rebuiltCount != 1 {
		t.Fatalf("expected one replacement scenario, got %d", rebuiltCount)
	}
}

// TestRebuildFailureLeavesSourceUntouched ensures a builder error rolls the
// whole transaction back: the source stays pending and unversioned.
func TestRebuildFailureLeavesSourceUntouched(t *testing.T) {
	db := newScenarioTestDB(t)
	auditRepo := audit.NewRepository(db)
	scenarioRepo := NewLayoutScenarioRepository(db, auditRepo)
	ctx := context.Background()

	frozenAt := time.Now()
	source := model.LayoutScenario{
		Name: "kept scenario", ScenarioStatus: constants.ScenarioPendingReview,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: `{"load_ids":[]}`,
		ZoneResultsJSON: "[]", ConstraintViolationsJSON: "[]",
		AlgorithmVersion: "thermal-v1", Version: 2, FrozenAt: &frozenAt, CreatedBy: 1,
	}
	if err := scenarioRepo.Create(ctx, &source, testEntry("layout_scenario.create")); err != nil {
		t.Fatalf("create source: %v", err)
	}

	boom := fmt.Errorf("planner failed")
	build := func(_ context.Context, _ *gorm.DB, _ model.LayoutScenario) (model.LayoutScenario, []audit.Entry, error) {
		return model.LayoutScenario{}, nil, boom
	}
	if _, err := scenarioRepo.Rebuild(ctx, source.ID, source.Version, build, testEntry("layout_scenario.rebuild.archive")); err == nil {
		t.Fatal("expected rebuild to fail")
	}

	var reloaded model.LayoutScenario
	if err := db.First(&reloaded, source.ID).Error; err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if reloaded.ScenarioStatus != constants.ScenarioPendingReview {
		t.Fatalf("source status changed to %s after failed rebuild", reloaded.ScenarioStatus)
	}
	if reloaded.Version != source.Version {
		t.Fatalf("source version changed from %d to %d after failed rebuild", source.Version, reloaded.Version)
	}
	var count int64
	if err := db.Model(&model.LayoutScenario{}).Count(&count).Error; err != nil {
		t.Fatalf("count scenarios: %v", err)
	}
	if count != 1 {
		t.Fatalf("failed rebuild must not insert a scenario, found %d", count)
	}
}

// TestApproveRejectsConcurrentVersionChange ensures approval is guarded by the
// in-transaction optimistic lock.
func TestApproveRejectsConcurrentVersionChange(t *testing.T) {
	db := newScenarioTestDB(t)
	auditRepo := audit.NewRepository(db)
	scenarioRepo := NewLayoutScenarioRepository(db, auditRepo)
	ctx := context.Background()

	frozenAt := time.Now()
	source := model.LayoutScenario{
		Name: "approve target", ScenarioStatus: constants.ScenarioPendingReview,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: `{"load_ids":[]}`,
		ZoneResultsJSON: "[]", ConstraintViolationsJSON: "[]",
		AlgorithmVersion: "thermal-v1", Version: 1, FrozenAt: &frozenAt, CreatedBy: 1,
	}
	if err := scenarioRepo.Create(ctx, &source, testEntry("layout_scenario.create")); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Simulate another actor moving the scenario back to draft before approval.
	if err := db.Model(&model.LayoutScenario{}).Where("id = ?", source.ID).
		Updates(map[string]any{"scenario_status": constants.ScenarioDraft, "version": gorm.Expr("version + 1")}).Error; err != nil {
		t.Fatalf("simulate concurrent change: %v", err)
	}

	verify := func(_ context.Context, _ *gorm.DB) error { return nil }
	err := scenarioRepo.Approve(ctx, source, verify, 2, testEntry("layout_scenario.approve"))
	if err == nil {
		t.Fatal("expected approval to fail after concurrent version change")
	}
	var reloaded model.LayoutScenario
	if err := db.First(&reloaded, source.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ScenarioStatus != constants.ScenarioDraft || reloaded.ApprovedBy != nil {
		t.Fatalf("failed approval mutated state: status=%s approved_by=%v", reloaded.ScenarioStatus, reloaded.ApprovedBy)
	}
}
