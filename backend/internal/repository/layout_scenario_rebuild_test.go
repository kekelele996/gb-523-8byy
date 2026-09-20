package repository

import (
	"context"
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
	open, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := open.AutoMigrate(&model.LayoutScenario{}, &audit.Event{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := open.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return open
}

func TestRebuildConcurrencyAndDiscard(t *testing.T) {
	db := newScenarioTestDB(t)
	repo := NewLayoutScenarioRepository(db, audit.NewRepository(db))
	ctx := context.Background()
	frozenAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	source := model.LayoutScenario{
		Name: "concurrent source", ScenarioStatus: constants.ScenarioPendingReview,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: `{"load_ids":[1],"frozen_at":"2026-09-01T08:00:00Z"}`,
		InputFrozenAt: &frozenAt, InputDiffJSON: "{}", ZoneResultsJSON: "[]",
		ConstraintViolationsJSON: "[]", AlgorithmVersion: "thermal-v1", Version: 3, CreatedBy: 1,
	}
	if err := repo.Create(ctx, &source, audit.Entry{Action: "layout_scenario.create", EntityType: "layout_scenario", ActorUsername: "planner"}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	first := model.LayoutScenario{
		Name: "source (rebuilt)", ScenarioStatus: constants.ScenarioEvaluating,
		InputSnapshotJSON: `{"load_ids":[1]}`, InputDiffJSON: "{}", AlgorithmVersion: "thermal-v1",
		Version: 1, CreatedBy: 1, RebuiltFromID: &source.ID,
	}
	begin := audit.Entry{Action: "layout_scenario.rebuild.start", EntityType: "layout_scenario", ActorUsername: "planner"}
	supersede := audit.Entry{Action: "layout_scenario.rebuild.supersede", EntityType: "layout_scenario", ActorUsername: "planner"}
	if err := repo.BeginRebuild(ctx, source.ID, 3, &first, begin, supersede); err != nil {
		t.Fatalf("first rebuild must succeed: %v", err)
	}

	second := model.LayoutScenario{
		Name: "source (rebuilt) duplicate", ScenarioStatus: constants.ScenarioEvaluating,
		InputSnapshotJSON: `{"load_ids":[1]}`, AlgorithmVersion: "thermal-v1",
		Version: 1, CreatedBy: 1, RebuiltFromID: &source.ID,
	}
	if err := repo.BeginRebuild(ctx, source.ID, 3, &second, begin, supersede); err == nil {
		t.Fatalf("concurrent rebuild must be rejected")
	}

	stored, err := repo.Get(ctx, source.ID)
	if err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if stored.ScenarioStatus != constants.ScenarioPendingReview {
		t.Fatalf("source status changed to %s", stored.ScenarioStatus)
	}
	if stored.Version != 3 {
		t.Fatalf("source version changed to %d", stored.Version)
	}
	if stored.SupersededByID == nil || *stored.SupersededByID != first.ID {
		t.Fatalf("source must point to the winning rebuild, got %v", stored.SupersededByID)
	}

	// Engine failure: discard the evaluating child and restore the source so
	// reviewers keep a pending-review scenario with unchanged approval state.
	discard := audit.Entry{Action: "layout_scenario.rebuild.discard", EntityType: "layout_scenario", ActorUsername: "planner"}
	if err := repo.DiscardRebuild(ctx, first.ID, source.ID, discard); err != nil {
		t.Fatalf("discard failed: %v", err)
	}
	restored, err := repo.Get(ctx, source.ID)
	if err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if restored.SupersededByID != nil || restored.ScenarioStatus != constants.ScenarioPendingReview || restored.Version != 3 {
		t.Fatalf("source not restored after discard: %+v", restored)
	}
	if _, err := repo.Get(ctx, first.ID); err == nil {
		t.Fatalf("discarded rebuild row must no longer exist")
	}

	// After rollback a fresh rebuild is allowed and succeeds.
	retry := model.LayoutScenario{
		Name: "source (rebuilt) retry", ScenarioStatus: constants.ScenarioEvaluating,
		InputSnapshotJSON: `{"load_ids":[1]}`, AlgorithmVersion: "thermal-v1",
		Version: 1, CreatedBy: 1, RebuiltFromID: &source.ID,
	}
	if err := repo.BeginRebuild(ctx, source.ID, 3, &retry, begin, supersede); err != nil {
		t.Fatalf("retry rebuild must succeed after discard: %v", err)
	}
}

func TestRefreshInputDiffGuardsStatus(t *testing.T) {
	db := newScenarioTestDB(t)
	repo := NewLayoutScenarioRepository(db, audit.NewRepository(db))
	ctx := context.Background()
	frozenAt := time.Now().UTC()
	draft := model.LayoutScenario{
		Name: "draft without review", ScenarioStatus: constants.ScenarioDraft,
		InputSnapshotJSON: `{}`, InputFrozenAt: &frozenAt, AlgorithmVersion: "thermal-v1",
		Version: 1, CreatedBy: 1,
	}
	if err := repo.Create(ctx, &draft, audit.Entry{Action: "layout_scenario.create", EntityType: "layout_scenario", ActorUsername: "planner"}); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	entry := audit.Entry{Action: "layout_scenario.input_drift.refresh", EntityType: "layout_scenario", ActorUsername: "reviewer"}
	if err := repo.RefreshInputDiff(ctx, draft.ID, `{"changed":[]}`, entry); err == nil {
		t.Fatalf("refreshing drift on a draft must be rejected")
	}
}
