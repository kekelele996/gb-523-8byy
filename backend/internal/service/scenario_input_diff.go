package service

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

const (
	changeAdded   = "added"
	changeMissing = "missing"
	changeChanged = "changed"
	diffEpsilon   = 0.0005
)

type fieldSpec struct {
	field  string
	number func(model.ThermalZone) float64
}

var zoneFields = []fieldSpec{
	{field: "cooling_capacity_kw", number: func(z model.ThermalZone) float64 { return z.CoolingCapacityKW }},
	{field: "supply_temp_c", number: func(z model.ThermalZone) float64 { return z.SupplyTempC }},
	{field: "max_return_temp_c", number: func(z model.ThermalZone) float64 { return z.MaxReturnTempC }},
}

type rackFieldSpec struct {
	field  string
	number func(model.Rack) float64
}

var rackFields = []rackFieldSpec{
	{field: "zone_id", number: func(r model.Rack) float64 { return float64(r.ZoneID) }},
	{field: "power_limit_kw", number: func(r model.Rack) float64 { return r.PowerLimitKW }},
	{field: "airflow_limit_cfm", number: func(r model.Rack) float64 { return r.AirflowLimitCFM }},
	{field: "rack_units", number: func(r model.Rack) float64 { return float64(r.RackUnits) }},
}

type loadFieldSpec struct {
	field  string
	number func(model.EquipmentLoad) float64
}

var loadFields = []loadFieldSpec{
	{field: "power_kw", number: func(l model.EquipmentLoad) float64 { return l.PowerKW }},
	{field: "heat_kw", number: func(l model.EquipmentLoad) float64 { return l.HeatKW }},
	{field: "airflow_cfm", number: func(l model.EquipmentLoad) float64 { return l.AirflowCFM }},
	{field: "rack_units", number: func(l model.EquipmentLoad) float64 { return float64(l.RackUnits) }},
}

// BuildInputDiff compares the frozen planning input against current entity rows
// and reports added, missing and changed values. Selection is fixed by load id,
// so loads only produce missing/changed entries; newly created loads are never
// "added" to an existing frozen selection.
func BuildInputDiff(frozen dto.FrozenScenarioInput, currentZones []model.ThermalZone, currentRacks []model.Rack, currentLoads []model.EquipmentLoad, computedAt time.Time) dto.ScenarioInputDiff {
	diff := dto.ScenarioInputDiff{
		Added: []dto.InputDiffEntry{}, Missing: []dto.InputDiffEntry{}, Changed: []dto.InputDiffEntry{},
		ComputedAt: computedAt,
	}

	frozenZones := make(map[uint]model.ThermalZone, len(frozen.Zones))
	for _, zone := range frozen.Zones {
		frozenZones[zone.ID] = zone
	}
	liveZones := make(map[uint]model.ThermalZone, len(currentZones))
	for _, zone := range currentZones {
		liveZones[zone.ID] = zone
	}
	for id, frozenZone := range frozenZones {
		label := zoneLabel(frozenZone)
		live, exists := liveZones[id]
		if !exists {
			diff.Missing = append(diff.Missing, missingEntry("thermal_zone", id, label))
			continue
		}
		if label == "" {
			label = zoneLabel(live)
		}
		for _, spec := range zoneFields {
			if valueChanged(spec.number(frozenZone), spec.number(live)) {
				diff.Changed = append(diff.Changed, numericEntry("thermal_zone", id, label, spec.field, spec.number(frozenZone), spec.number(live)))
			}
		}
		if frozenZone.ZoneStatus != live.ZoneStatus {
			diff.Changed = append(diff.Changed, textEntry("thermal_zone", id, label, "zone_status", frozenZone.ZoneStatus, live.ZoneStatus))
		}
		if !sameAdjacency(frozenZone.AdjacencyJSON, live.AdjacencyJSON) {
			diff.Changed = append(diff.Changed, textEntry("thermal_zone", id, label, "adjacency", strings.TrimSpace(frozenZone.AdjacencyJSON), strings.TrimSpace(live.AdjacencyJSON)))
		}
	}
	for id, liveZone := range liveZones {
		if _, existed := frozenZones[id]; !existed {
			diff.Added = append(diff.Added, addedEntry("thermal_zone", id, zoneLabel(liveZone)))
		}
	}

	frozenRacks := make(map[uint]model.Rack, len(frozen.Racks))
	for _, rack := range frozen.Racks {
		frozenRacks[rack.ID] = rack
	}
	liveRacks := make(map[uint]model.Rack, len(currentRacks))
	for _, rack := range currentRacks {
		liveRacks[rack.ID] = rack
	}
	for id, frozenRack := range frozenRacks {
		label := rackLabel(frozenRack)
		live, exists := liveRacks[id]
		if !exists {
			diff.Missing = append(diff.Missing, missingEntry("rack", id, label))
			continue
		}
		if label == "" {
			label = rackLabel(live)
		}
		for _, spec := range rackFields {
			if valueChanged(spec.number(frozenRack), spec.number(live)) {
				diff.Changed = append(diff.Changed, numericEntry("rack", id, label, spec.field, spec.number(frozenRack), spec.number(live)))
			}
		}
		if frozenRack.RackStatus != live.RackStatus {
			diff.Changed = append(diff.Changed, textEntry("rack", id, label, "rack_status", string(frozenRack.RackStatus), string(live.RackStatus)))
		}
	}
	for id, liveRack := range liveRacks {
		if _, existed := frozenRacks[id]; !existed {
			diff.Added = append(diff.Added, addedEntry("rack", id, rackLabel(liveRack)))
		}
	}

	frozenLoads := make(map[uint]model.EquipmentLoad, len(frozen.Loads))
	for _, load := range frozen.Loads {
		frozenLoads[load.ID] = load
	}
	liveLoads := make(map[uint]model.EquipmentLoad, len(currentLoads))
	for _, load := range currentLoads {
		liveLoads[load.ID] = load
	}
	for id, frozenLoad := range frozenLoads {
		label := frozenLoad.Name
		live, exists := liveLoads[id]
		if !exists {
			diff.Missing = append(diff.Missing, missingEntry("equipment_load", id, label))
			continue
		}
		if label == "" {
			label = live.Name
		}
		for _, spec := range loadFields {
			if valueChanged(spec.number(frozenLoad), spec.number(live)) {
				diff.Changed = append(diff.Changed, numericEntry("equipment_load", id, label, spec.field, spec.number(frozenLoad), spec.number(live)))
			}
		}
		if frozenLoad.RedundancyGroup != live.RedundancyGroup {
			diff.Changed = append(diff.Changed, textEntry("equipment_load", id, label, "redundancy_group", frozenLoad.RedundancyGroup, live.RedundancyGroup))
		}
		if frozenLoad.LoadStatus != live.LoadStatus {
			diff.Changed = append(diff.Changed, textEntry("equipment_load", id, label, "load_status", frozenLoad.LoadStatus, live.LoadStatus))
		}
		if optionalID(frozenLoad.PreferredZoneID) != optionalID(live.PreferredZoneID) {
			diff.Changed = append(diff.Changed, textEntry("equipment_load", id, label, "preferred_zone_id", optionalLabel(frozenLoad.PreferredZoneID), optionalLabel(live.PreferredZoneID)))
		}
	}

	sortEntries(diff.Added)
	sortEntries(diff.Missing)
	sortEntries(diff.Changed)
	diff.Summary = summarizeDiff(diff)
	return diff
}

func summarizeDiff(diff dto.ScenarioInputDiff) string {
	if !diff.HasChanges() {
		return "frozen inputs match current zones, racks and selected loads"
	}
	parts := make([]string, 0, 3)
	if len(diff.Added) > 0 {
		parts = append(parts, fmt.Sprintf("%d added", len(diff.Added)))
	}
	if len(diff.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d missing", len(diff.Missing)))
	}
	if len(diff.Changed) > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", len(diff.Changed)))
	}
	return strings.Join(parts, ", ")
}

func sortEntries(entries []dto.InputDiffEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].EntityType != entries[j].EntityType {
			return entries[i].EntityType < entries[j].EntityType
		}
		if entries[i].EntityID != entries[j].EntityID {
			return entries[i].EntityID < entries[j].EntityID
		}
		return entries[i].Field < entries[j].Field
	})
}

func valueChanged(frozen, current float64) bool {
	return math.Abs(frozen-current) > diffEpsilon
}

func zoneLabel(zone model.ThermalZone) string {
	if zone.ZoneCode != "" {
		return zone.ZoneCode
	}
	return zone.Name
}

func rackLabel(rack model.Rack) string {
	if rack.RackCode != "" {
		return rack.RackCode
	}
	return fmt.Sprintf("rack-%d", rack.ID)
}

func addedEntry(entityType string, id uint, label string) dto.InputDiffEntry {
	return dto.InputDiffEntry{EntityType: entityType, EntityID: id, Label: label, Field: "-", ChangeType: changeAdded, Detail: label + " is not part of the frozen input"}
}

func missingEntry(entityType string, id uint, label string) dto.InputDiffEntry {
	return dto.InputDiffEntry{EntityType: entityType, EntityID: id, Label: label, Field: "-", ChangeType: changeMissing, Detail: label + " referenced by the frozen input no longer exists"}
}

func numericEntry(entityType string, id uint, label, field string, frozen, current float64) dto.InputDiffEntry {
	return dto.InputDiffEntry{
		EntityType: entityType, EntityID: id, Label: label, Field: field, ChangeType: changeChanged,
		Frozen: frozen, Current: current,
		Detail: fmt.Sprintf("%s %s changed from %s to %s", label, field, trimNumber(frozen), trimNumber(current)),
	}
}

func textEntry(entityType string, id uint, label, field, frozen, current string) dto.InputDiffEntry {
	entry := numericEntry(entityType, id, label, field, 0, 0)
	entry.Detail = fmt.Sprintf("%s %s changed from %q to %q", label, field, frozen, current)
	return entry
}

func trimNumber(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", value), "0"), ".")
}

func sameAdjacency(frozen, current string) bool {
	frozenMap := dto.DecodeAdjacency(frozen)
	currentMap := dto.DecodeAdjacency(current)
	if len(frozenMap) != len(currentMap) {
		return false
	}
	for code, weight := range frozenMap {
		other, exists := currentMap[code]
		if !exists || valueChanged(weight, other) {
			return false
		}
	}
	return true
}

func optionalID(value *uint) uint {
	if value == nil {
		return 0
	}
	return *value
}

func optionalLabel(value *uint) string {
	if value == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *value)
}
