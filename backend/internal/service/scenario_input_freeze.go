package service

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

// floatDriftEpsilon is the tolerance used when comparing frozen and current
// numeric boundary values so harmless floating point round trips do not drift.
const floatDriftEpsilon = 1e-6

const inputDriftBlockingReason = "frozen inputs no longer match current zones, racks or selected loads; rebuild from the latest data and re-evaluate before approval"

// encodeFrozenInput marshals the immutable planning input snapshot.
func encodeFrozenInput(loadIDs []uint, zones []model.ThermalZone, racks []model.Rack, loads []model.EquipmentLoad, algorithmVersion string, frozenAt time.Time) (string, error) {
	frozen := dto.FrozenInputFromModels(loadIDs, zones, racks, loads, algorithmVersion, frozenAt.UTC())
	raw, err := json.Marshal(frozen)
	if err != nil {
		return "", fmt.Errorf("encode frozen scenario input: %w", err)
	}
	return string(raw), nil
}

// frozenInputToModels converts a frozen snapshot back into planner inputs.
// Evaluation always runs against these values, so boundary edits made after
// draft creation never change the stored result.
func frozenInputToModels(input dto.FrozenInput) ([]model.ThermalZone, []model.Rack, []model.EquipmentLoad) {
	zones := make([]model.ThermalZone, 0, len(input.Zones))
	for _, zone := range input.Zones {
		zones = append(zones, model.ThermalZone{
			ID: zone.ID, ZoneCode: zone.ZoneCode, Name: zone.Name,
			CoolingCapacityKW: zone.CoolingCapacityKW, SupplyTempC: zone.SupplyTempC,
			MaxReturnTempC: zone.MaxReturnTempC, AdjacencyJSON: zone.AdjacencyJSON, ZoneStatus: zone.ZoneStatus,
		})
	}
	racks := make([]model.Rack, 0, len(input.Racks))
	for _, rack := range input.Racks {
		racks = append(racks, model.Rack{
			ID: rack.ID, ZoneID: rack.ZoneID, RackCode: rack.RackCode,
			PowerLimitKW: rack.PowerLimitKW, AirflowLimitCFM: rack.AirflowLimitCFM,
			RackUnits: rack.RackUnits, RackStatus: constants.RackStatus(rack.RackStatus),
		})
	}
	loads := make([]model.EquipmentLoad, 0, len(input.Loads))
	for _, load := range input.Loads {
		loads = append(loads, model.EquipmentLoad{
			ID: load.ID, Name: load.Name, PowerKW: load.PowerKW, HeatKW: load.HeatKW,
			AirflowCFM: load.AirflowCFM, RackUnits: load.RackUnits, RedundancyGroup: load.RedundancyGroup,
			PreferredZoneID: load.PreferredZoneID, LoadStatus: load.LoadStatus,
		})
	}
	return zones, racks, loads
}

// CompareFrozenInput compares a frozen draft snapshot against the current live
// zones, racks and selected loads, listing added, removed and changed inputs.
func CompareFrozenInput(input dto.FrozenInput, zones []model.ThermalZone, racks []model.Rack, loads []model.EquipmentLoad) dto.InputDriftReport {
	entries := []dto.InputDriftEntry{}

	liveZones := make(map[uint]model.ThermalZone, len(zones))
	for _, zone := range zones {
		liveZones[zone.ID] = zone
	}
	frozenZoneIDs := make(map[uint]bool, len(input.Zones))
	for _, frozen := range input.Zones {
		frozenZoneIDs[frozen.ID] = true
		current, exists := liveZones[frozen.ID]
		if !exists {
			entries = append(entries, removedEntry("thermal_zone", frozen.ID, frozen.ZoneCode, "thermal zone removed"))
			continue
		}
		entries = append(entries, compareZone(frozen, current)...)
	}
	for _, zone := range zones {
		if !frozenZoneIDs[zone.ID] {
			entries = append(entries, addedEntry("thermal_zone", zone.ID, zone.ZoneCode, "thermal zone added"))
		}
	}

	liveRacks := make(map[uint]model.Rack, len(racks))
	for _, rack := range racks {
		liveRacks[rack.ID] = rack
	}
	frozenRackIDs := make(map[uint]bool, len(input.Racks))
	for _, frozen := range input.Racks {
		frozenRackIDs[frozen.ID] = true
		current, exists := liveRacks[frozen.ID]
		if !exists {
			entries = append(entries, removedEntry("rack", frozen.ID, frozen.RackCode, "rack removed"))
			continue
		}
		entries = append(entries, compareRack(frozen, current)...)
	}
	for _, rack := range racks {
		if !frozenRackIDs[rack.ID] {
			entries = append(entries, addedEntry("rack", rack.ID, rack.RackCode, "rack added"))
		}
	}

	liveLoads := make(map[uint]model.EquipmentLoad, len(loads))
	for _, load := range loads {
		liveLoads[load.ID] = load
	}
	for _, frozen := range input.Loads {
		// The selected load set is fixed for the scenario, so live loads are
		// never reported as "added".
		current, exists := liveLoads[frozen.ID]
		if !exists {
			entries = append(entries, removedEntry("equipment_load", frozen.ID, frozen.Name, "selected load removed"))
			continue
		}
		entries = append(entries, compareLoad(frozen, current)...)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].EntityType != entries[j].EntityType {
			return entries[i].EntityType < entries[j].EntityType
		}
		if entries[i].EntityID != entries[j].EntityID {
			return entries[i].EntityID < entries[j].EntityID
		}
		return entries[i].Field < entries[j].Field
	})

	report := dto.InputDriftReport{Entries: entries}
	for _, entry := range entries {
		switch entry.Kind {
		case "added":
			report.AddedCount++
		case "removed":
			report.RemovedCount++
		default:
			report.ChangedCount++
		}
	}
	report.TotalCount = len(entries)
	report.HasDrift = report.TotalCount > 0
	if report.HasDrift {
		report.BlockingReason = inputDriftBlockingReason
	}
	return report
}

func compareZone(frozen dto.FrozenThermalZone, current model.ThermalZone) []dto.InputDriftEntry {
	entries := []dto.InputDriftEntry{}
	entries = appendChangedFloat(entries, "thermal_zone", frozen.ID, frozen.ZoneCode, "cooling_capacity_kw", "cooling capacity kW", frozen.CoolingCapacityKW, current.CoolingCapacityKW)
	entries = appendChangedFloat(entries, "thermal_zone", frozen.ID, frozen.ZoneCode, "supply_temp_c", "supply temperature C", frozen.SupplyTempC, current.SupplyTempC)
	entries = appendChangedFloat(entries, "thermal_zone", frozen.ID, frozen.ZoneCode, "max_return_temp_c", "max return temperature C", frozen.MaxReturnTempC, current.MaxReturnTempC)
	if frozen.ZoneStatus != current.ZoneStatus {
		entries = append(entries, textEntry("thermal_zone", frozen.ID, frozen.ZoneCode, "zone_status",
			fmt.Sprintf("zone status changed from %s to %s", frozen.ZoneStatus, current.ZoneStatus)))
	}
	if normalizeAdjacency(frozen.AdjacencyJSON) != normalizeAdjacency(current.AdjacencyJSON) {
		entries = append(entries, textEntry("thermal_zone", frozen.ID, frozen.ZoneCode, "adjacency",
			fmt.Sprintf("adjacency weights changed from %s to %s", frozen.AdjacencyJSON, current.AdjacencyJSON)))
	}
	return entries
}

func compareRack(frozen dto.FrozenRack, current model.Rack) []dto.InputDriftEntry {
	entries := []dto.InputDriftEntry{}
	entries = appendChangedFloat(entries, "rack", frozen.ID, frozen.RackCode, "power_limit_kw", "rack power limit kW", frozen.PowerLimitKW, current.PowerLimitKW)
	entries = appendChangedFloat(entries, "rack", frozen.ID, frozen.RackCode, "airflow_limit_cfm", "rack airflow limit CFM", frozen.AirflowLimitCFM, current.AirflowLimitCFM)
	if frozen.RackUnits != current.RackUnits {
		entries = append(entries, changedEntry("rack", frozen.ID, frozen.RackCode, "rack_units", float64(frozen.RackUnits), float64(current.RackUnits),
			fmt.Sprintf("rack capacity changed from %dU to %dU", frozen.RackUnits, current.RackUnits)))
	}
	if frozen.RackStatus != string(current.RackStatus) {
		entries = append(entries, textEntry("rack", frozen.ID, frozen.RackCode, "rack_status",
			fmt.Sprintf("rack status changed from %s to %s", frozen.RackStatus, current.RackStatus)))
	}
	if frozen.ZoneID != current.ZoneID {
		entries = append(entries, changedEntry("rack", frozen.ID, frozen.RackCode, "zone_id", float64(frozen.ZoneID), float64(current.ZoneID),
			fmt.Sprintf("rack moved from zone %d to zone %d", frozen.ZoneID, current.ZoneID)))
	}
	return entries
}

func compareLoad(frozen dto.FrozenEquipmentLoad, current model.EquipmentLoad) []dto.InputDriftEntry {
	entries := []dto.InputDriftEntry{}
	entries = appendChangedFloat(entries, "equipment_load", frozen.ID, frozen.Name, "power_kw", "load power kW", frozen.PowerKW, current.PowerKW)
	entries = appendChangedFloat(entries, "equipment_load", frozen.ID, frozen.Name, "heat_kw", "load heat kW", frozen.HeatKW, current.HeatKW)
	entries = appendChangedFloat(entries, "equipment_load", frozen.ID, frozen.Name, "airflow_cfm", "load airflow CFM", frozen.AirflowCFM, current.AirflowCFM)
	if frozen.RackUnits != current.RackUnits {
		entries = append(entries, changedEntry("equipment_load", frozen.ID, frozen.Name, "rack_units", float64(frozen.RackUnits), float64(current.RackUnits),
			fmt.Sprintf("load size changed from %dU to %dU", frozen.RackUnits, current.RackUnits)))
	}
	if frozen.RedundancyGroup != current.RedundancyGroup {
		entries = append(entries, textEntry("equipment_load", frozen.ID, frozen.Name, "redundancy_group",
			fmt.Sprintf("redundancy group changed from %s to %s", frozen.RedundancyGroup, current.RedundancyGroup)))
	}
	if frozen.LoadStatus != current.LoadStatus {
		entries = append(entries, textEntry("equipment_load", frozen.ID, frozen.Name, "load_status",
			fmt.Sprintf("load status changed from %s to %s", frozen.LoadStatus, current.LoadStatus)))
	}
	if ptrValue(frozen.PreferredZoneID) != ptrValue(current.PreferredZoneID) {
		entries = append(entries, changedEntry("equipment_load", frozen.ID, frozen.Name, "preferred_zone_id",
			float64(ptrValue(frozen.PreferredZoneID)), float64(ptrValue(current.PreferredZoneID)),
			fmt.Sprintf("preferred zone changed from %s to %s", zoneLabel(frozen.PreferredZoneID), zoneLabel(current.PreferredZoneID))))
	}
	return entries
}

func appendChangedFloat(entries []dto.InputDriftEntry, entityType string, id uint, identifier, field, label string, frozenValue, currentValue float64) []dto.InputDriftEntry {
	if math.Abs(frozenValue-currentValue) <= floatDriftEpsilon {
		return entries
	}
	return append(entries, changedEntry(entityType, id, identifier, field, frozenValue, currentValue,
		fmt.Sprintf("%s changed from %s to %s", label, formatNumber(frozenValue), formatNumber(currentValue))))
}

func addedEntry(entityType string, id uint, identifier, detail string) dto.InputDriftEntry {
	return dto.InputDriftEntry{Kind: "added", EntityType: entityType, EntityID: id, Identifier: identifier, Detail: detail}
}

func removedEntry(entityType string, id uint, identifier, detail string) dto.InputDriftEntry {
	return dto.InputDriftEntry{Kind: "removed", EntityType: entityType, EntityID: id, Identifier: identifier, Detail: detail}
}

func changedEntry(entityType string, id uint, identifier, field string, frozenValue, currentValue float64, detail string) dto.InputDriftEntry {
	return dto.InputDriftEntry{
		Kind: "changed", EntityType: entityType, EntityID: id, Identifier: identifier,
		Field: field, Frozen: frozenValue, Current: currentValue, Detail: detail,
	}
}

// textEntry records a non-numeric value change (status or group); numeric
// fields stay at zero and the values are carried in the human-readable detail.
func textEntry(entityType string, id uint, identifier, field, detail string) dto.InputDriftEntry {
	return changedEntry(entityType, id, identifier, field, 0, 0, detail)
}

func formatNumber(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", value), "0"), ".")
}

func ptrValue(value *uint) uint {
	if value == nil {
		return 0
	}
	return *value
}

func zoneLabel(value *uint) string {
	if value == nil {
		return "none"
	}
	return fmt.Sprintf("zone %d", *value)
}

// normalizeAdjacency canonicalizes an adjacency JSON document so key ordering
// never produces false diffs.
func normalizeAdjacency(raw string) string {
	values := map[string]float64{}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return strings.TrimSpace(raw)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%g", key, values[key]))
	}
	return strings.Join(parts, ",")
}
