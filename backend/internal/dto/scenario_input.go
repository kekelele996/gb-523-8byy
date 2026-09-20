package dto

import (
	"encoding/json"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/model"
)

// FrozenScenarioInput is the immutable planning input captured when a draft is
// created (or when a rebuild starts): selected loads, zone cooling/adjacency
// boundaries and rack capacities. Evaluation always reads this snapshot, never
// live entity rows, so later edits cannot alter a frozen result.
type FrozenScenarioInput struct {
	LoadIDs          []uint                `json:"load_ids"`
	AlgorithmVersion string                `json:"algorithm_version"`
	Zones            []model.ThermalZone   `json:"zones"`
	Racks            []model.Rack          `json:"racks"`
	Loads            []model.EquipmentLoad `json:"loads"`
	FrozenAt         time.Time             `json:"frozen_at"`
}

func EncodeFrozenInput(input FrozenScenarioInput) (string, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func DecodeFrozenInput(raw string) (FrozenScenarioInput, error) {
	var input FrozenScenarioInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return FrozenScenarioInput{}, err
	}
	return input, nil
}

type InputDiffEntry struct {
	EntityType string  `json:"entity_type"`
	EntityID   uint    `json:"entity_id"`
	Label      string  `json:"label"`
	Field      string  `json:"field"`
	ChangeType string  `json:"change_type"`
	Frozen     float64 `json:"frozen"`
	Current    float64 `json:"current"`
	Detail     string  `json:"detail"`
}

type ScenarioInputDiff struct {
	Added      []InputDiffEntry `json:"added"`
	Missing    []InputDiffEntry `json:"missing"`
	Changed    []InputDiffEntry `json:"changed"`
	Summary    string           `json:"summary"`
	ComputedAt time.Time        `json:"computed_at"`
}

func (d ScenarioInputDiff) HasChanges() bool {
	return len(d.Added) > 0 || len(d.Missing) > 0 || len(d.Changed) > 0
}

func EmptyScenarioInputDiff(computedAt time.Time) ScenarioInputDiff {
	return ScenarioInputDiff{
		Added: []InputDiffEntry{}, Missing: []InputDiffEntry{}, Changed: []InputDiffEntry{},
		Summary: "frozen inputs match current zones, racks and selected loads", ComputedAt: computedAt,
	}
}

func EncodeScenarioInputDiff(diff ScenarioInputDiff) (string, error) {
	raw, err := json.Marshal(diff)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func DecodeScenarioInputDiff(raw string) ScenarioInputDiff {
	diff := ScenarioInputDiff{Added: []InputDiffEntry{}, Missing: []InputDiffEntry{}, Changed: []InputDiffEntry{}}
	if raw == "" {
		return diff
	}
	_ = json.Unmarshal([]byte(raw), &diff)
	if diff.Added == nil {
		diff.Added = []InputDiffEntry{}
	}
	if diff.Missing == nil {
		diff.Missing = []InputDiffEntry{}
	}
	if diff.Changed == nil {
		diff.Changed = []InputDiffEntry{}
	}
	return diff
}
