package growatt

import (
	"encoding/json"
	"testing"
)

func TestFlexFloat_Number(t *testing.T) {
	var f FlexFloat
	err := json.Unmarshal([]byte(`123.45`), &f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Float64() != 123.45 {
		t.Errorf("expected 123.45, got %f", f.Float64())
	}
}

func TestFlexFloat_String(t *testing.T) {
	var f FlexFloat
	err := json.Unmarshal([]byte(`"456.78"`), &f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Float64() != 456.78 {
		t.Errorf("expected 456.78, got %f", f.Float64())
	}
}

func TestFlexFloat_EmptyString(t *testing.T) {
	var f FlexFloat
	err := json.Unmarshal([]byte(`""`), &f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Float64() != 0 {
		t.Errorf("expected 0, got %f", f.Float64())
	}
}

func TestFlexFloat_Null(t *testing.T) {
	var f FlexFloat
	err := json.Unmarshal([]byte(`null`), &f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Float64() != 0 {
		t.Errorf("expected 0, got %f", f.Float64())
	}
}

func TestFlexFloat_Integer(t *testing.T) {
	var f FlexFloat
	err := json.Unmarshal([]byte(`100`), &f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Float64() != 100 {
		t.Errorf("expected 100, got %f", f.Float64())
	}
}

func TestFlexFloat_IntegerString(t *testing.T) {
	var f FlexFloat
	err := json.Unmarshal([]byte(`"200"`), &f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Float64() != 200 {
		t.Errorf("expected 200, got %f", f.Float64())
	}
}

func TestFlexFloat_InStruct(t *testing.T) {
	type TestStruct struct {
		Value FlexFloat `json:"value"`
	}

	tests := []struct {
		name     string
		input    string
		expected float64
	}{
		{"number", `{"value": 123.45}`, 123.45},
		{"string", `{"value": "456.78"}`, 456.78},
		{"empty", `{"value": ""}`, 0},
		{"integer", `{"value": 100}`, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s TestStruct
			err := json.Unmarshal([]byte(tt.input), &s)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.Value.Float64() != tt.expected {
				t.Errorf("expected %f, got %f", tt.expected, s.Value.Float64())
			}
		})
	}
}

func TestFlexString_String(t *testing.T) {
	var s FlexString
	err := json.Unmarshal([]byte(`"hello"`), &s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.String() != "hello" {
		t.Errorf("expected %q, got %q", "hello", s.String())
	}
}

func TestFlexString_Number(t *testing.T) {
	var s FlexString
	err := json.Unmarshal([]byte(`12345`), &s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.String() != "12345" {
		t.Errorf("expected %q, got %q", "12345", s.String())
	}
}

func TestFlexString_Float(t *testing.T) {
	var s FlexString
	err := json.Unmarshal([]byte(`123.45`), &s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.String() != "123.45" {
		t.Errorf("expected %q, got %q", "123.45", s.String())
	}
}

func TestPlant_MixedTypes(t *testing.T) {
	// Simulates API returning some fields as strings
	input := `{
		"plant_id": "12345",
		"plant_name": "Test Plant",
		"total_energy": "15234.8",
		"current_power": 4523.5,
		"today_energy": "32.5"
	}`

	var plant Plant
	err := json.Unmarshal([]byte(input), &plant)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plant.PlantID.String() != "12345" {
		t.Errorf("expected plant_id %q, got %q", "12345", plant.PlantID.String())
	}

	if plant.TotalEnergy.Float64() != 15234.8 {
		t.Errorf("expected total_energy 15234.8, got %f", plant.TotalEnergy.Float64())
	}

	if plant.CurrentPower.Float64() != 4523.5 {
		t.Errorf("expected current_power 4523.5, got %f", plant.CurrentPower.Float64())
	}

	if plant.TodayEnergy.Float64() != 32.5 {
		t.Errorf("expected today_energy 32.5, got %f", plant.TodayEnergy.Float64())
	}
}

func TestFlexPowers_Map(t *testing.T) {
	input := `{"00:00": 0, "12:00": 4500.5}`
	var p FlexPowers
	err := json.Unmarshal([]byte(input), &p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p["12:00"] != 4500.5 {
		t.Errorf("expected 4500.5, got %f", p["12:00"])
	}
}

func TestFlexPowers_ArrayOfObjects(t *testing.T) {
	input := `[{"time": "00:00", "power": 0}, {"time": "12:00", "power": 4500.5}]`
	var p FlexPowers
	err := json.Unmarshal([]byte(input), &p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p["12:00"] != 4500.5 {
		t.Errorf("expected 4500.5, got %f", p["12:00"])
	}
}

func TestFlexPowers_ArrayOfArrays(t *testing.T) {
	input := `[["00:00", 0], ["12:00", 4500.5]]`
	var p FlexPowers
	err := json.Unmarshal([]byte(input), &p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p["12:00"] != 4500.5 {
		t.Errorf("expected 4500.5, got %f", p["12:00"])
	}
}

func TestFlexPowers_NullPower(t *testing.T) {
	// API returns null for power when no data
	input := `[{"time": "2025-02-03 12:00", "power": null}, {"time": "2025-02-03 12:05", "power": 4500.5}]`
	var p FlexPowers
	err := json.Unmarshal([]byte(input), &p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p["12:00"] != 0 {
		t.Errorf("expected 0 for null power, got %f", p["12:00"])
	}
	if p["12:05"] != 4500.5 {
		t.Errorf("expected 4500.5, got %f", p["12:05"])
	}
}

func TestFlexPowers_DateTimeFormat(t *testing.T) {
	// API returns time as "YYYY-MM-DD HH:MM"
	input := `[{"time": "2025-02-03 12:00", "power": 4500.5}]`
	var p FlexPowers
	err := json.Unmarshal([]byte(input), &p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should normalize to just "12:00"
	if p["12:00"] != 4500.5 {
		t.Errorf("expected key '12:00' with value 4500.5, got %v", p)
	}
}

func TestFlexPowers_Empty(t *testing.T) {
	input := `[]`
	var p FlexPowers
	err := json.Unmarshal([]byte(input), &p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p) != 0 {
		t.Errorf("expected empty map, got %d entries", len(p))
	}
}

func TestPlant_NumericPlantID(t *testing.T) {
	// Simulates API returning plant_id as a number
	input := `{
		"plant_id": 12345,
		"plant_name": "Test Plant",
		"total_energy": 15234.8
	}`

	var plant Plant
	err := json.Unmarshal([]byte(input), &plant)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plant.PlantID.String() != "12345" {
		t.Errorf("expected plant_id %q, got %q", "12345", plant.PlantID.String())
	}
}
