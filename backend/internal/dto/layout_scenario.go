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
	FrozenAt             *time.Time               `json:"frozen_at"`
	SourceScenarioID     *uint                    `json:"source_scenario_id"`
	InputDrift           *InputDriftReport        `json:"input_drift"`
	CreatedBy            uint                     `json:"created_by"`
	ApprovedBy           *uint                    `json:"approved_by"`
	HasCriticalViolation bool                     `json:"has_critical_violation"`
}

// InputDriftEntry describes one difference between the frozen scenario inputs
// and the current live boundary data. Kind is one of added, removed, changed.
type InputDriftEntry struct {
	Kind       string  `json:"kind"`
	EntityType string  `json:"entity_type"`
	EntityID   uint    `json:"entity_id"`
	Identifier string  `json:"identifier"`
	Field      string  `json:"field,omitempty"`
	Frozen     float64 `json:"frozen,omitempty"`
	Current    float64 `json:"current,omitempty"`
	Detail     string  `json:"detail,omitempty"`
}

// InputDriftReport is computed server side by comparing a frozen input
// snapshot with the current live zones, racks and selected loads.
type InputDriftReport struct {
	AddedCount     int               `json:"added_count"`
	RemovedCount   int               `json:"removed_count"`
	ChangedCount   int               `json:"changed_count"`
	TotalCount     int               `json:"total_count"`
	HasDrift       bool              `json:"has_drift"`
	BlockingReason string            `json:"blocking_reason,omitempty"`
	Entries        []InputDriftEntry `json:"entries"`
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
		FrozenAt: value.FrozenAt, SourceScenarioID: value.SourceScenarioID,
		CreatedBy: value.CreatedBy, ApprovedBy: value.ApprovedBy,
		Assignments: []RackAssignment{}, ZoneResults: []ZoneThermalResult{}, Violations: []ConstraintViolation{},
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
	return response
}

// FrozenInput is the full input snapshot captured when a draft is created.
// External edits to zones, racks or the selected loads never mutate a stored
// snapshot, so later evaluation and review stay reproducible.
type FrozenInput struct {
	LoadIDs          []uint                `json:"load_ids"`
	AlgorithmVersion string                `json:"algorithm_version"`
	FrozenAt         time.Time             `json:"frozen_at"`
	Zones            []FrozenThermalZone   `json:"zones"`
	Racks            []FrozenRack          `json:"racks"`
	Loads            []FrozenEquipmentLoad `json:"loads"`
}

type FrozenThermalZone struct {
	ID                uint    `json:"id"`
	ZoneCode          string  `json:"zone_code"`
	Name              string  `json:"name"`
	CoolingCapacityKW float64 `json:"cooling_capacity_kw"`
	SupplyTempC       float64 `json:"supply_temp_c"`
	MaxReturnTempC    float64 `json:"max_return_temp_c"`
	AdjacencyJSON     string  `json:"adjacency_json"`
	ZoneStatus        string  `json:"zone_status"`
}

type FrozenRack struct {
	ID              uint    `json:"id"`
	ZoneID          uint    `json:"zone_id"`
	RackCode        string  `json:"rack_code"`
	PowerLimitKW    float64 `json:"power_limit_kw"`
	AirflowLimitCFM float64 `json:"airflow_limit_cfm"`
	RackUnits       int     `json:"rack_units"`
	RackStatus      string  `json:"rack_status"`
}

type FrozenEquipmentLoad struct {
	ID              uint    `json:"id"`
	Name            string  `json:"name"`
	PowerKW         float64 `json:"power_kw"`
	HeatKW          float64 `json:"heat_kw"`
	AirflowCFM      float64 `json:"airflow_cfm"`
	RackUnits       int     `json:"rack_units"`
	RedundancyGroup string  `json:"redundancy_group"`
	PreferredZoneID *uint   `json:"preferred_zone_id"`
	LoadStatus      string  `json:"load_status"`
}

// DecodeFrozenInput parses a stored input snapshot. Legacy drafts created
// before full input freezing only carry load ids; the second return value is
// false in that case so callers can re-freeze from current data.
func DecodeFrozenInput(raw string) (FrozenInput, bool) {
	var input FrozenInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil || len(input.Loads) == 0 || len(input.Zones) == 0 {
		return FrozenInput{}, false
	}
	return input, true
}

// FrozenInputFromModels builds the immutable planning input snapshot from the
// current live entities. Association data is intentionally excluded.
func FrozenInputFromModels(loadIDs []uint, zones []model.ThermalZone, racks []model.Rack, loads []model.EquipmentLoad, algorithmVersion string, frozenAt time.Time) FrozenInput {
	input := FrozenInput{
		LoadIDs:          append([]uint(nil), loadIDs...),
		AlgorithmVersion: algorithmVersion,
		FrozenAt:         frozenAt,
		Zones:            make([]FrozenThermalZone, 0, len(zones)),
		Racks:            make([]FrozenRack, 0, len(racks)),
		Loads:            make([]FrozenEquipmentLoad, 0, len(loads)),
	}
	for _, zone := range zones {
		input.Zones = append(input.Zones, FrozenThermalZone{
			ID: zone.ID, ZoneCode: zone.ZoneCode, Name: zone.Name,
			CoolingCapacityKW: zone.CoolingCapacityKW, SupplyTempC: zone.SupplyTempC,
			MaxReturnTempC: zone.MaxReturnTempC, AdjacencyJSON: zone.AdjacencyJSON, ZoneStatus: zone.ZoneStatus,
		})
	}
	for _, rack := range racks {
		input.Racks = append(input.Racks, FrozenRack{
			ID: rack.ID, ZoneID: rack.ZoneID, RackCode: rack.RackCode,
			PowerLimitKW: rack.PowerLimitKW, AirflowLimitCFM: rack.AirflowLimitCFM,
			RackUnits: rack.RackUnits, RackStatus: string(rack.RackStatus),
		})
	}
	for _, load := range loads {
		input.Loads = append(input.Loads, FrozenEquipmentLoad{
			ID: load.ID, Name: load.Name, PowerKW: load.PowerKW, HeatKW: load.HeatKW,
			AirflowCFM: load.AirflowCFM, RackUnits: load.RackUnits, RedundancyGroup: load.RedundancyGroup,
			PreferredZoneID: load.PreferredZoneID, LoadStatus: load.LoadStatus,
		})
	}
	return input
}
