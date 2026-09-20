package service

import (
	"testing"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

func diffFixture() (dto.FrozenScenarioInput, []model.ThermalZone, []model.Rack, []model.EquipmentLoad) {
	zones := []model.ThermalZone{
		{ID: 1, ZoneCode: "TZ-A", Name: "Aisle A", CoolingCapacityKW: 100, SupplyTempC: 18, MaxReturnTempC: 30, AdjacencyJSON: `{"TZ-B":0.2}`, ZoneStatus: "active"},
		{ID: 2, ZoneCode: "TZ-B", Name: "Aisle B", CoolingCapacityKW: 80, SupplyTempC: 19, MaxReturnTempC: 31, AdjacencyJSON: `{}`, ZoneStatus: "active"},
	}
	racks := []model.Rack{
		{ID: 10, ZoneID: 1, RackCode: "A-01", RowIndex: 1, ColumnIndex: 1, PowerLimitKW: 24, AirflowLimitCFM: 6800, RackUnits: 42, RackStatus: constants.RackAvailable},
		{ID: 11, ZoneID: 2, RackCode: "B-01", RowIndex: 2, ColumnIndex: 1, PowerLimitKW: 32, AirflowLimitCFM: 9000, RackUnits: 48, RackStatus: constants.RackAvailable},
	}
	loads := []model.EquipmentLoad{
		{ID: 100, Name: "node-1", PowerKW: 10, HeatKW: 9.5, AirflowCFM: 3000, RackUnits: 8, RedundancyGroup: "G1", LoadStatus: "ready"},
		{ID: 101, Name: "node-2", PowerKW: 12, HeatKW: 11, AirflowCFM: 3200, RackUnits: 10, RedundancyGroup: "G2", PreferredZoneID: uintPtr(2), LoadStatus: "ready"},
	}
	frozen := dto.FrozenScenarioInput{
		LoadIDs: []uint{100, 101}, AlgorithmVersion: "thermal-v1",
		Zones: cloneZones(zones), Racks: cloneRacks(racks), Loads: cloneLoads(loads),
		FrozenAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	}
	return frozen, zones, racks, loads
}

func TestBuildInputDiff(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		mutate      func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad)
		wantAdded   int
		wantMissing int
		wantChanged int
		wantField   string
		wantType    string
		wantEntity  uint
	}{
		{
			name:        "identical inputs have no drift",
			mutate:      func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {},
			wantAdded:   0,
			wantMissing: 0,
			wantChanged: 0,
		},
		{
			name: "new zone and rack appear",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				*zones = append(*zones, model.ThermalZone{ID: 9, ZoneCode: "TZ-X", Name: "New", CoolingCapacityKW: 50, SupplyTempC: 18, MaxReturnTempC: 30, AdjacencyJSON: `{}`, ZoneStatus: "active"})
				*racks = append(*racks, model.Rack{ID: 99, ZoneID: 9, RackCode: "X-01", PowerLimitKW: 20, AirflowLimitCFM: 5000, RackUnits: 42, RackStatus: constants.RackAvailable})
			},
			wantAdded: 2,
		},
		{
			name: "zone removed is missing",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				*zones = (*zones)[:1]
			},
			wantMissing: 1,
			wantField:   "-",
			wantType:    "missing",
			wantEntity:  2,
		},
		{
			name: "rack removed is missing",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				*racks = (*racks)[:1]
			},
			wantMissing: 1,
			wantEntity:  11,
		},
		{
			name: "selected load deleted is missing",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				*loads = (*loads)[:1]
			},
			wantMissing: 1,
			wantEntity:  101,
		},
		{
			name: "newly created load is not added to fixed selection",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				*loads = append(*loads, model.EquipmentLoad{ID: 200, Name: "late-node", PowerKW: 5, LoadStatus: "ready"})
			},
			wantAdded: 0,
		},
		{
			name: "zone cooling capacity changed",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*zones)[0].CoolingCapacityKW = 70
			},
			wantChanged: 1,
			wantField:   "cooling_capacity_kw",
			wantType:    "changed",
			wantEntity:  1,
		},
		{
			name: "adjacency weight changed",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*zones)[0].AdjacencyJSON = `{"TZ-B":0.45}`
			},
			wantChanged: 1,
			wantField:   "adjacency",
		},
		{
			name: "zone status changed",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*zones)[1].ZoneStatus = "offline"
			},
			wantChanged: 1,
			wantField:   "zone_status",
		},
		{
			name: "rack power capacity changed",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*racks)[0].PowerLimitKW = 18
			},
			wantChanged: 1,
			wantField:   "power_limit_kw",
			wantEntity:  10,
		},
		{
			name: "rack units and status changed count as two changes",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*racks)[1].RackUnits = 40
				(*racks)[1].RackStatus = constants.RackMaintenance
			},
			wantChanged: 2,
		},
		{
			name: "load heat and status changed",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*loads)[0].HeatKW = 13
				(*loads)[0].LoadStatus = "held"
			},
			wantChanged: 2,
		},
		{
			name: "negligible float jitter is not drift",
			mutate: func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*zones)[0].CoolingCapacityKW = 100.0001
			},
			wantChanged: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frozen, zones, racks, loads := diffFixture()
			liveZones := cloneZones(zones)
			liveRacks := cloneRacks(racks)
			liveLoads := cloneLoads(loads)
			tt.mutate(&liveZones, &liveRacks, &liveLoads)

			diff := BuildInputDiff(frozen, liveZones, liveRacks, liveLoads, now)
			if len(diff.Added) != tt.wantAdded || len(diff.Missing) != tt.wantMissing || len(diff.Changed) != tt.wantChanged {
				t.Fatalf("counts added=%d missing=%d changed=%d, want %d/%d/%d; diff=%+v",
					len(diff.Added), len(diff.Missing), len(diff.Changed), tt.wantAdded, tt.wantMissing, tt.wantChanged, diff)
			}
			if !diff.HasChanges() != (tt.wantAdded+tt.wantMissing+tt.wantChanged == 0) {
				t.Fatalf("HasChanges=%t inconsistent with counts", diff.HasChanges())
			}
			if tt.wantField != "" {
				all := append(append(append([]dto.InputDiffEntry{}, diff.Added...), diff.Missing...), diff.Changed...)
				found := false
				for _, entry := range all {
					if entry.Field == tt.wantField && (tt.wantType == "" || entry.ChangeType == tt.wantType) && (tt.wantEntity == 0 || entry.EntityID == tt.wantEntity) {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected entry field=%s type=%s entity=%d in %+v", tt.wantField, tt.wantType, tt.wantEntity, all)
				}
			}
			if diff.ComputedAt != now {
				t.Fatalf("computed_at = %s, want %s", diff.ComputedAt, now)
			}
		})
	}
}

func TestInputDiffDeterministicOrder(t *testing.T) {
	frozen, zones, racks, loads := diffFixture()
	liveZones := cloneZones(zones)
	liveRacks := cloneRacks(racks)
	liveLoads := cloneLoads(loads)
	liveZones = append(liveZones, model.ThermalZone{ID: 5, ZoneCode: "TZ-M", CoolingCapacityKW: 40, SupplyTempC: 18, MaxReturnTempC: 30, AdjacencyJSON: `{}`, ZoneStatus: "active"})
	liveRacks[0].PowerLimitKW = 20
	liveLoads[0].HeatKW = 12

	first := BuildInputDiff(frozen, liveZones, liveRacks, liveLoads, time.Now())
	second := BuildInputDiff(frozen, liveZones, liveRacks, liveLoads, time.Now())
	if first.Summary != second.Summary {
		t.Fatalf("summary not deterministic: %q vs %q", first.Summary, second.Summary)
	}
	if len(first.Changed) != len(second.Changed) {
		t.Fatalf("changed length differs")
	}
	for i := range first.Changed {
		if first.Changed[i].Field != second.Changed[i].Field || first.Changed[i].EntityID != second.Changed[i].EntityID {
			t.Fatalf("changed entries not in stable order: %+v vs %+v", first.Changed, second.Changed)
		}
	}
	if first.Summary == "" {
		t.Fatalf("summary must be populated")
	}
}

func uintPtr(value uint) *uint { return &value }

func cloneZones(zones []model.ThermalZone) []model.ThermalZone {
	return append([]model.ThermalZone(nil), zones...)
}

func cloneRacks(racks []model.Rack) []model.Rack {
	return append([]model.Rack(nil), racks...)
}

func cloneLoads(loads []model.EquipmentLoad) []model.EquipmentLoad {
	cloned := append([]model.EquipmentLoad(nil), loads...)
	for i := range cloned {
		if loads[i].PreferredZoneID != nil {
			value := *loads[i].PreferredZoneID
			cloned[i].PreferredZoneID = &value
		}
	}
	return cloned
}
