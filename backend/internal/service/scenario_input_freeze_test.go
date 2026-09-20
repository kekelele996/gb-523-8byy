package service

import (
	"testing"
	"time"

	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

func driftFixture() (dto.FrozenInput, []model.ThermalZone, []model.Rack, []model.EquipmentLoad) {
	zoneB := uint(2)
	zoneA := uint(1)
	input := dto.FrozenInput{
		LoadIDs:          []uint{10},
		AlgorithmVersion: "thermal-v1",
		FrozenAt:         time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
		Zones: []dto.FrozenThermalZone{
			{ID: 1, ZoneCode: "TZ-A", Name: "A", CoolingCapacityKW: 100, SupplyTempC: 18, MaxReturnTempC: 30, AdjacencyJSON: `{"TZ-B":0.2}`, ZoneStatus: "active"},
			{ID: 2, ZoneCode: "TZ-B", Name: "B", CoolingCapacityKW: 80, SupplyTempC: 19, MaxReturnTempC: 31, AdjacencyJSON: `{"TZ-A":0.1}`, ZoneStatus: "active"},
		},
		Racks: []dto.FrozenRack{
			{ID: 1, ZoneID: 1, RackCode: "A-01", PowerLimitKW: 20, AirflowLimitCFM: 6000, RackUnits: 42, RackStatus: "available"},
			{ID: 2, ZoneID: 2, RackCode: "B-01", PowerLimitKW: 30, AirflowLimitCFM: 9000, RackUnits: 48, RackStatus: "reserved"},
		},
		Loads: []dto.FrozenEquipmentLoad{
			{ID: 10, Name: "node", PowerKW: 10, HeatKW: 9, AirflowCFM: 2000, RackUnits: 8, RedundancyGroup: "RG-A", PreferredZoneID: &zoneA, LoadStatus: "ready"},
		},
	}
	zones := []model.ThermalZone{
		{ID: 1, ZoneCode: "TZ-A", Name: "A", CoolingCapacityKW: 100, SupplyTempC: 18, MaxReturnTempC: 30, AdjacencyJSON: `{"TZ-B":0.2}`, ZoneStatus: "active"},
		{ID: 2, ZoneCode: "TZ-B", Name: "B", CoolingCapacityKW: 80, SupplyTempC: 19, MaxReturnTempC: 31, AdjacencyJSON: `{"TZ-A":0.1}`, ZoneStatus: "active"},
	}
	racks := []model.Rack{
		{ID: 1, ZoneID: 1, RackCode: "A-01", PowerLimitKW: 20, AirflowLimitCFM: 6000, RackUnits: 42, RackStatus: constants.RackAvailable},
		{ID: 2, ZoneID: 2, RackCode: "B-01", PowerLimitKW: 30, AirflowLimitCFM: 9000, RackUnits: 48, RackStatus: constants.RackReserved},
	}
	loads := []model.EquipmentLoad{
		{ID: 10, Name: "node", PowerKW: 10, HeatKW: 9, AirflowCFM: 2000, RackUnits: 8, RedundancyGroup: "RG-A", PreferredZoneID: &zoneB, LoadStatus: "ready"},
	}
	return input, zones, racks, loads
}

// identicalBaseline returns deep copies matching the frozen snapshot.
func identicalBaseline() (dto.FrozenInput, []model.ThermalZone, []model.Rack, []model.EquipmentLoad) {
	input, _, _, _ := driftFixture()
	_, zones, racks, loads := driftFixture()
	zoneA := uint(1)
	loads[0].PreferredZoneID = &zoneA
	return input, zones, racks, loads
}

func TestCompareFrozenInput(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad)
		wantAdded   int
		wantRemoved int
		wantChanged int
		wantField   string
		wantType    string
		wantKind    string
	}{
		{name: "identical inputs have no drift", mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) {}},
		{
			name: "zone cooling capacity change", wantChanged: 1, wantField: "cooling_capacity_kw", wantType: "thermal_zone", wantKind: "changed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) {
				(*z)[0].CoolingCapacityKW = 120
			},
		},
		{
			name: "adjacency weight change ignores key order", wantChanged: 1, wantField: "adjacency", wantType: "thermal_zone", wantKind: "changed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) {
				(*z)[0].AdjacencyJSON = `{"TZ-B":0.35}`
			},
		},
		{
			name: "zone removal", wantRemoved: 1, wantType: "thermal_zone", wantKind: "removed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) { *z = (*z)[:1] },
		},
		{
			name: "zone addition", wantAdded: 1, wantType: "thermal_zone", wantKind: "added",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) {
				*z = append(*z, model.ThermalZone{ID: 3, ZoneCode: "TZ-C", Name: "C", CoolingCapacityKW: 60, SupplyTempC: 18, MaxReturnTempC: 30, AdjacencyJSON: `{}`, ZoneStatus: "active"})
			},
		},
		{
			name: "rack capacity value change", wantChanged: 1, wantField: "power_limit_kw", wantType: "rack", wantKind: "changed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) { (*r)[0].PowerLimitKW = 25 },
		},
		{
			name: "rack status change", wantChanged: 1, wantField: "rack_status", wantType: "rack", wantKind: "changed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) {
				(*r)[0].RackStatus = constants.RackUnavailable
			},
		},
		{
			name: "rack removal", wantRemoved: 1, wantType: "rack", wantKind: "removed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) { *r = (*r)[:1] },
		},
		{
			name: "rack addition", wantAdded: 1, wantType: "rack", wantKind: "added",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) {
				*r = append(*r, model.Rack{ID: 3, ZoneID: 1, RackCode: "A-03", PowerLimitKW: 20, AirflowLimitCFM: 6000, RackUnits: 42, RackStatus: constants.RackAvailable})
			},
		},
		{
			name: "selected load removal", wantRemoved: 1, wantType: "equipment_load", wantKind: "removed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) { *l = []model.EquipmentLoad{} },
		},
		{
			name: "load numeric value change", wantChanged: 1, wantField: "heat_kw", wantType: "equipment_load", wantKind: "changed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) { (*l)[0].HeatKW = 12.5 },
		},
		{
			name: "load status change", wantChanged: 1, wantField: "load_status", wantType: "equipment_load", wantKind: "changed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) { (*l)[0].LoadStatus = "held" },
		},
		{
			name: "preferred zone change", wantChanged: 1, wantField: "preferred_zone_id", wantType: "equipment_load", wantKind: "changed",
			mutate: func(z *[]model.ThermalZone, r *[]model.Rack, l *[]model.EquipmentLoad) {
				other := uint(2)
				(*l)[0].PreferredZoneID = &other
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, zones, racks, loads := identicalBaseline()
			tt.mutate(&zones, &racks, &loads)
			report := CompareFrozenInput(input, zones, racks, loads)
			if report.AddedCount != tt.wantAdded || report.RemovedCount != tt.wantRemoved || report.ChangedCount != tt.wantChanged {
				t.Fatalf("counts added=%d removed=%d changed=%d, want %d/%d/%d; entries=%+v",
					report.AddedCount, report.RemovedCount, report.ChangedCount, tt.wantAdded, tt.wantRemoved, tt.wantChanged, report.Entries)
			}
			if tt.wantKind != "" {
				if !report.HasDrift || report.BlockingReason == "" {
					t.Fatalf("drift should set has_drift and blocking reason: %+v", report)
				}
				if len(report.Entries) == 0 || report.Entries[0].Kind != tt.wantKind || report.Entries[0].EntityType != tt.wantType {
					t.Fatalf("unexpected first entry: %+v", report.Entries)
				}
				if tt.wantField != "" && report.Entries[0].Field != tt.wantField {
					t.Fatalf("field=%q, want %q", report.Entries[0].Field, tt.wantField)
				}
			} else if report.HasDrift {
				t.Fatalf("expected no drift, got %+v", report.Entries)
			}
		})
	}
}

func TestNormalizeAdjacencyOrderIndependent(t *testing.T) {
	if normalizeAdjacency(`{"B":0.2,"A":0.1}`) != normalizeAdjacency(`{"A":0.1,"B":0.2}`) {
		t.Fatal("adjacency comparison must ignore JSON key order")
	}
}

func TestEncodeAndDecodeFrozenSnapshotRoundTrip(t *testing.T) {
	_, zones, racks, loads := driftFixture()
	zoneA := uint(1)
	loads[0].PreferredZoneID = &zoneA
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	raw, err := encodeFrozenInput([]uint{10}, zones, racks, loads, "thermal-v1", at)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, ok := dto.DecodeFrozenInput(raw)
	if !ok {
		t.Fatal("full snapshot should decode as complete")
	}
	if len(decoded.Zones) != 2 || len(decoded.Racks) != 2 || len(decoded.Loads) != 1 {
		t.Fatalf("decoded snapshot lost entities: %+v", decoded)
	}
	decodedZones, decodedRacks, decodedLoads := frozenInputToModels(decoded)
	if decodedRacks[0].RackStatus != constants.RackAvailable {
		t.Fatalf("rack status not restored: %s", decodedRacks[0].RackStatus)
	}
	if decodedZones[0].CoolingCapacityKW != zones[0].CoolingCapacityKW || decodedLoads[0].HeatKW != loads[0].HeatKW {
		t.Fatal("numeric frozen values did not round trip")
	}
}

func TestDecodeLegacySnapshotIncomplete(t *testing.T) {
	if _, ok := dto.DecodeFrozenInput(`{"load_ids":[1],"algorithm_version":"thermal-v1"}`); ok {
		t.Fatal("legacy snapshot without frozen entities must be reported incomplete")
	}
}
