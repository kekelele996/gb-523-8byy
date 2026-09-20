package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/audit"
	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/model"
	"datacenter-thermal-capacity-planner/backend/internal/web"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type LayoutScenarioRepository struct {
	db    *gorm.DB
	audit *audit.Repository
}

func NewLayoutScenarioRepository(db *gorm.DB, auditRepo *audit.Repository) *LayoutScenarioRepository {
	return &LayoutScenarioRepository{db: db, audit: auditRepo}
}

func (r *LayoutScenarioRepository) List(ctx context.Context, search, status string, page, size int) ([]model.LayoutScenario, int64, error) {
	query := r.db.WithContext(ctx).Model(&model.LayoutScenario{})
	if search != "" {
		query = query.Where("LOWER(name) LIKE ?", "%"+strings.ToLower(search)+"%")
	}
	if status != "" {
		query = query.Where("scenario_status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count layout scenarios: %w", err)
	}
	var scenarios []model.LayoutScenario
	err := query.Order("updated_at DESC, id DESC").Offset((page - 1) * size).Limit(size).Find(&scenarios).Error
	if err != nil {
		return nil, 0, fmt.Errorf("list layout scenarios: %w", err)
	}
	return scenarios, total, nil
}

func (r *LayoutScenarioRepository) Get(ctx context.Context, id uint) (model.LayoutScenario, error) {
	var scenario model.LayoutScenario
	if err := r.db.WithContext(ctx).First(&scenario, id).Error; err != nil {
		return model.LayoutScenario{}, fmt.Errorf("get layout scenario %d: %w", id, err)
	}
	return scenario, nil
}

func (r *LayoutScenarioRepository) Create(ctx context.Context, scenario *model.LayoutScenario, entry audit.Entry) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(scenario).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return web.Conflict("SCENARIO_NAME_EXISTS", "scenario name already exists", err)
			}
			return fmt.Errorf("create layout scenario: %w", err)
		}
		entry.EntityID = scenario.ID
		return r.audit.RecordWithDB(ctx, tx, entry)
	})
}

// BeginEvaluation flips a draft to evaluating inside an optimistic-lock
// guarded transaction. A refreshed frozen snapshot can be supplied for legacy
// drafts that were created before full input freezing.
func (r *LayoutScenarioRepository) BeginEvaluation(ctx context.Context, id, expectedVersion uint, refreshedSnapshot string, refreshedFrozenAt *time.Time, entry audit.Entry) (model.LayoutScenario, error) {
	var scenario model.LayoutScenario
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&scenario, id).Error; err != nil {
			return web.NotFound("layout scenario")
		}
		if scenario.Version != expectedVersion || scenario.ScenarioStatus != constants.ScenarioDraft {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario must be an unchanged draft before evaluation", nil)
		}
		updates := map[string]any{"scenario_status": constants.ScenarioEvaluating, "version": gorm.Expr("version + 1")}
		if refreshedSnapshot != "" {
			updates["input_snapshot_json"] = refreshedSnapshot
		}
		if refreshedFrozenAt != nil {
			updates["frozen_at"] = refreshedFrozenAt
		}
		result := tx.Model(&model.LayoutScenario{}).Where("id = ? AND version = ? AND scenario_status = ?", id, expectedVersion, constants.ScenarioDraft).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("begin scenario evaluation: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario changed while evaluation was starting", nil)
		}
		entry.EntityID = id
		entry.BeforeSummary = string(constants.ScenarioDraft)
		entry.AfterSummary = string(constants.ScenarioEvaluating)
		return r.audit.RecordWithDB(ctx, tx, entry)
	})
	if err != nil {
		return model.LayoutScenario{}, err
	}
	scenario.ScenarioStatus = constants.ScenarioEvaluating
	scenario.Version++
	return scenario, nil
}

type EvaluationUpdate struct {
	AssignmentsJSON string
	ZoneResultsJSON string
	ViolationsJSON  string
	TotalPowerKW    float64
	PeakTempC       float64
	Score           float64
}

// FinishEvaluation stores evaluation results and moves the scenario to
// pending_review. The frozen input snapshot is intentionally untouched so the
// evaluated inputs remain exactly what the planner ran against.
func (r *LayoutScenarioRepository) FinishEvaluation(ctx context.Context, scenario model.LayoutScenario, update EvaluationUpdate, entry audit.Entry) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.LayoutScenario{}).
			Where("id = ? AND version = ? AND scenario_status = ?", scenario.ID, scenario.Version, constants.ScenarioEvaluating).
			Updates(map[string]any{
				"rack_assignments_json":      update.AssignmentsJSON,
				"zone_results_json":          update.ZoneResultsJSON,
				"constraint_violations_json": update.ViolationsJSON,
				"total_power_kw":             update.TotalPowerKW,
				"peak_temp_c":                update.PeakTempC,
				"score":                      update.Score,
				"scenario_status":            constants.ScenarioPendingReview,
				"version":                    gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return fmt.Errorf("finish scenario evaluation: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario changed during evaluation", nil)
		}
		entry.EntityID = scenario.ID
		entry.BeforeSummary = string(constants.ScenarioEvaluating)
		entry.AfterSummary = fmt.Sprintf("%s score=%.1f", constants.ScenarioPendingReview, update.Score)
		return r.audit.RecordWithDB(ctx, tx, entry)
	})
}

func (r *LayoutScenarioRepository) Transition(ctx context.Context, scenario model.LayoutScenario, target constants.ScenarioStatus, actorID uint, entry audit.Entry) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"scenario_status": target, "version": gorm.Expr("version + 1")}
		if target == constants.ScenarioApproved {
			updates["approved_by"] = actorID
		}
		result := tx.Model(&model.LayoutScenario{}).
			Where("id = ? AND version = ? AND scenario_status = ?", scenario.ID, scenario.Version, scenario.ScenarioStatus).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("transition layout scenario: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario changed before transition", nil)
		}
		entry.EntityID = scenario.ID
		entry.BeforeSummary = string(scenario.ScenarioStatus)
		entry.AfterSummary = string(target)
		return r.audit.RecordWithDB(ctx, tx, entry)
	})
}

// LockedScenario re-reads a scenario row with a row lock when running on
// PostgreSQL. SQLite serializes writes and treats the locking clause as a
// no-op through GORM.
func LockedScenario(ctx context.Context, tx *gorm.DB, id uint) (model.LayoutScenario, error) {
	var scenario model.LayoutScenario
	query := tx.WithContext(ctx)
	if tx.Dialector.Name() == "postgres" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.First(&scenario, id).Error; err != nil {
		return model.LayoutScenario{}, web.NotFound("layout scenario")
	}
	return scenario, nil
}

// InputVerifier re-reads live boundary inputs inside an approval transaction
// and returns an *web.AppError when frozen inputs have drifted.
type InputVerifier func(ctx context.Context, tx *gorm.DB) error

// Approve performs the authoritative approval consistency check: the scenario
// row is locked, live inputs are re-read under lock, verify runs the frozen
// versus current comparison, and only then is the status flipped. Any drift or
// concurrent change aborts the whole transaction without mutating the
// scenario or its approval state.
func (r *LayoutScenarioRepository) Approve(ctx context.Context, scenario model.LayoutScenario, verify InputVerifier, actorID uint, entry audit.Entry) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := LockedScenario(ctx, tx, scenario.ID)
		if err != nil {
			return err
		}
		if locked.Version != scenario.Version || locked.ScenarioStatus != constants.ScenarioPendingReview {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario changed before approval", nil)
		}
		if err := verify(ctx, tx); err != nil {
			return err
		}
		result := tx.Model(&model.LayoutScenario{}).
			Where("id = ? AND version = ? AND scenario_status = ?", locked.ID, locked.Version, constants.ScenarioPendingReview).
			Updates(map[string]any{"scenario_status": constants.ScenarioApproved, "approved_by": actorID, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return fmt.Errorf("approve layout scenario: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario changed during approval", nil)
		}
		entry.EntityID = locked.ID
		entry.BeforeSummary = string(constants.ScenarioPendingReview)
		entry.AfterSummary = string(constants.ScenarioApproved)
		return r.audit.RecordWithDB(ctx, tx, entry)
	})
}

// RebuildBuilder reads live inputs from the locked transaction, rebuilds the
// frozen snapshot and evaluation result, and returns the new scenario (with
// ID zero so it gets inserted) together with any extra audit entries.
type RebuildBuilder func(ctx context.Context, tx *gorm.DB, source model.LayoutScenario) (model.LayoutScenario, []audit.Entry, error)

// Rebuild atomically replaces an obsolete pending scenario: it locks the
// source scenario, lets the builder read current data and plan the rebuild,
// inserts the freshly evaluated scenario and archives the source. Concurrent
// rebuilds serialize on the source row; only one can pass the version and
// status guard. Any failure rolls back so the original scenario and its
// approval state stay untouched.
func (r *LayoutScenarioRepository) Rebuild(ctx context.Context, sourceID, expectedVersion uint, build RebuildBuilder, archiveEntry audit.Entry) (model.LayoutScenario, error) {
	var rebuilt model.LayoutScenario
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		source, err := LockedScenario(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if source.Version != expectedVersion || source.ScenarioStatus != constants.ScenarioPendingReview {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "only an unchanged pending review scenario can be rebuilt", nil)
		}
		newScenario, entries, err := build(ctx, tx, source)
		if err != nil {
			return err
		}
		if err := tx.Create(&newScenario).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return web.Conflict("SCENARIO_NAME_EXISTS", "rebuilt scenario name already exists", err)
			}
			return fmt.Errorf("create rebuilt layout scenario: %w", err)
		}
		result := tx.Model(&model.LayoutScenario{}).
			Where("id = ? AND version = ? AND scenario_status = ?", source.ID, source.Version, constants.ScenarioPendingReview).
			Updates(map[string]any{"scenario_status": constants.ScenarioArchived, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return fmt.Errorf("archive rebuilt scenario source: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario changed while rebuilding", nil)
		}
		for _, entry := range entries {
			if err := r.audit.RecordWithDB(ctx, tx, entry); err != nil {
				return err
			}
		}
		archiveEntry.EntityID = sourceID
		archiveEntry.BeforeSummary = fmt.Sprintf("%s v%d", constants.ScenarioPendingReview, source.Version)
		archiveEntry.AfterSummary = fmt.Sprintf("%s replaced_by=#%d", constants.ScenarioArchived, newScenario.ID)
		if err := r.audit.RecordWithDB(ctx, tx, archiveEntry); err != nil {
			return err
		}
		rebuilt = newScenario
		return nil
	})
	if err != nil {
		return model.LayoutScenario{}, err
	}
	return rebuilt, nil
}
