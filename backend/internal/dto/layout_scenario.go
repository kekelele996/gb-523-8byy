package dto

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

type CreateLayoutScenarioRequest struct {
	Name    string `json:"name" binding:"required,min=3,max=120"`
	LoadIDs []uint `json:"load_ids" binding:"required,min=1"`
}

type EvaluateScenarioRequest struct {
	Version uint `json:"version" binding:"required"`
}

type RebuildScenarioRequest struct {
	Version uint `json:"version" binding:"required"`
}

type TransitionScenarioRequest struct {
	TargetStatus constants.ScenarioStatus `json:"target_status" binding:"required"`
	Version      uint                     `json:"version" binding:"required"`
	Reason       string                   `json:"reason" binding:"max=500"`
}

type ConstraintViolation struct {
	Code       string  `json:"code"`
	Severity   string  `json:"severity"`
	EntityType string  `json:"entity_type"`
	EntityID   uint    `json:"entity_id"`
	Message    string  `json:"message"`
	Actual     float64 `json:"actual"`
	Limit      float64 `json:"limit"`
}

type RackAssignment struct {
	LoadID         uint     `json:"load_id"`
	LoadName       string   `json:"load_name"`
	RackID         uint     `json:"rack_id"`
	RackCode       string   `json:"rack_code"`
	ZoneID         uint     `json:"zone_id"`
	ZoneCode       string   `json:"zone_code"`
	PowerKW        float64  `json:"power_kw"`
	HeatKW         float64  `json:"heat_kw"`
	AirflowCFM     float64  `json:"airflow_cfm"`
	RackUnits      int      `json:"rack_units"`
	PlacementScore float64  `json:"placement_score"`
	Explanation    []string `json:"explanation"`
}

type ZoneThermalResult struct {
	ZoneID             uint    `json:"zone_id"`
	ZoneCode           string  `json:"zone_code"`
	AssignedHeatKW     float64 `json:"assigned_heat_kw"`
	NeighborHeatKW     float64 `json:"neighbor_heat_kw"`
	EstimatedReturnC   float64 `json:"estimated_return_c"`
	TemperatureMarginC float64 `json:"temperature_margin_c"`
	CoolingMarginKW    float64 `json:"cooling_margin_kw"`
}

type ScenarioResponse struct {
	ID                   uint                     `json:"id"`
	Name                 string                   `json:"name"`
	ScenarioStatus       constants.ScenarioStatus `json:"scenario_status"`
	Assignments          []RackAssignment         `json:"assignments"`
	ZoneResults          []ZoneThermalResult      `json:"zone_results"`
	Violations           []ConstraintViolation    `json:"violations"`
	TotalPowerKW         float64                  `json:"total_power_kw"`
	PeakTempC            float64                  `json:"peak_temp_c"`
	Score                float64                  `json:"score"`
	Version              uint                     `json:"version"`
	AlgorithmVersion     string                   `json:"algorithm_version"`
	CreatedBy            uint                     `json:"created_by"`
	ApprovedBy           *uint                    `json:"approved_by"`
	HasCriticalViolation bool                     `json:"has_critical_violation"`
	InputFrozenAt        *time.Time               `json:"input_frozen_at"`
	InputDiff            ScenarioInputDiff        `json:"input_diff"`
	RebuiltFromID        *uint                    `json:"rebuilt_from_id"`
	SupersededByID       *uint                    `json:"superseded_by_id"`
	IsSuperseded         bool                     `json:"is_superseded"`
	ApprovalBlocked      bool                     `json:"approval_blocked"`
	ApprovalBlockReason  string                   `json:"approval_block_reason"`
}

type ScenarioComparison struct {
	Left          ScenarioResponse `json:"left"`
	Right         ScenarioResponse `json:"right"`
	ScoreDelta    float64          `json:"score_delta"`
	PowerDeltaKW  float64          `json:"power_delta_kw"`
	PeakTempDelta float64          `json:"peak_temp_delta_c"`
	Summary       []string         `json:"summary"`
}

func (r CreateLayoutScenarioRequest) ValidateBusiness() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("scenario name is required")
	}
	seen := map[uint]bool{}
	for _, id := range r.LoadIDs {
		if id == 0 {
			return errors.New("load ids must be positive")
		}
		if seen[id] {
			return errors.New("load ids must not contain duplicates")
		}
		seen[id] = true
	}
	return nil
}

func DecodeScenario(value model.LayoutScenario) ScenarioResponse {
	response := ScenarioResponse{
		ID: value.ID, Name: value.Name, ScenarioStatus: value.ScenarioStatus,
		TotalPowerKW: value.TotalPowerKW, PeakTempC: value.PeakTempC,
		Score: value.Score, Version: value.Version, AlgorithmVersion: value.AlgorithmVersion,
		CreatedBy: value.CreatedBy, ApprovedBy: value.ApprovedBy,
		InputFrozenAt: value.InputFrozenAt, RebuiltFromID: value.RebuiltFromID,
		SupersededByID: value.SupersededByID, IsSuperseded: value.IsSuperseded(),
		Assignments: []RackAssignment{}, ZoneResults: []ZoneThermalResult{}, Violations: []ConstraintViolation{},
		InputDiff: DecodeScenarioInputDiff(value.InputDiffJSON),
	}
	_ = json.Unmarshal([]byte(value.RackAssignmentsJSON), &response.Assignments)
	_ = json.Unmarshal([]byte(value.ZoneResultsJSON), &response.ZoneResults)
	_ = json.Unmarshal([]byte(value.ConstraintViolationsJSON), &response.Violations)
	for _, violation := range response.Violations {
		if violation.Severity == "critical" {
			response.HasCriticalViolation = true
			break
		}
	}
	response.ApprovalBlocked, response.ApprovalBlockReason = ApprovalBlockers(response)
	return response
}

// ApprovalBlockers derives why a pending review scenario cannot be approved.
// Drift against frozen inputs blocks first because the stored result no longer
// represents current data; the only remedy is a rebuild and re-evaluation.
func ApprovalBlockers(response ScenarioResponse) (bool, string) {
	if response.ScenarioStatus != constants.ScenarioPendingReview {
		return false, ""
	}
	if response.InputDiff.HasChanges() {
		return true, "frozen inputs differ from current data (" + response.InputDiff.Summary + "); rebuild the scenario from the latest data and re-evaluate before approval"
	}
	if response.InputDiff.ComputedAt.IsZero() || response.InputFrozenAt == nil {
		return true, "frozen input evidence is missing; rebuild the scenario to recapture inputs before approval"
	}
	if response.IsSuperseded {
		return true, "this scenario was superseded by a rebuild and can no longer be approved"
	}
	if response.HasCriticalViolation {
		return true, "critical constraint violations remain"
	}
	return false, ""
}
