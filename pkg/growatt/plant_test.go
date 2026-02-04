package growatt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadTestData(t *testing.T, filename string) []byte {
	t.Helper()
	path := filepath.Join("testdata", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to load test data %s: %v", filename, err)
	}
	return data
}

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	return NewClient("test-token",
		WithBaseURL(server.URL+"/"),
		WithRateLimit(0),
	)
}

func TestListPlants(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plant/list" {
			t.Errorf("expected path /plant/list, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "plant_list.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	plants, err := client.ListPlants(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(plants) != 2 {
		t.Errorf("expected 2 plants, got %d", len(plants))
	}

	if plants[0].PlantID != "12345" {
		t.Errorf("expected plant ID %q, got %q", "12345", plants[0].PlantID)
	}

	if plants[0].PlantName != "Home Solar" {
		t.Errorf("expected plant name %q, got %q", "Home Solar", plants[0].PlantName)
	}

	if plants[0].CurrentPower != 4523.5 {
		t.Errorf("expected current power %f, got %f", 4523.5, plants[0].CurrentPower)
	}
}

func TestListPlantsError(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "error_permission_denied.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	_, err := client.ListPlants(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !IsPermissionDenied(err) {
		t.Errorf("expected permission denied error, got %v", err)
	}
}

func TestGetPlantPower(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plant/power" {
			t.Errorf("expected path /plant/power, got %s", r.URL.Path)
		}

		plantID := r.URL.Query().Get("plant_id")
		if plantID != "12345" {
			t.Errorf("expected plant_id %q, got %q", "12345", plantID)
		}

		date := r.URL.Query().Get("date")
		if date != "2025-02-03" {
			t.Errorf("expected date %q, got %q", "2025-02-03", date)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "plant_power.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	testDate, _ := time.Parse("2006-01-02", "2025-02-03")
	power, err := client.GetPlantPower(ctx, "12345", testDate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if power.PlantID != "12345" {
		t.Errorf("expected plant ID %q, got %q", "12345", power.PlantID)
	}

	if power.Date != "2025-02-03" {
		t.Errorf("expected date %q, got %q", "2025-02-03", power.Date)
	}

	if len(power.Powers) == 0 {
		t.Error("expected power data, got none")
	}

	// Verify data is sorted by time
	for i := 1; i < len(power.Powers); i++ {
		if power.Powers[i].Time < power.Powers[i-1].Time {
			t.Errorf("power data not sorted: %s < %s", power.Powers[i].Time, power.Powers[i-1].Time)
		}
	}
}

func TestGetPlantEnergy(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plant/energy" {
			t.Errorf("expected path /plant/energy, got %s", r.URL.Path)
		}

		q := r.URL.Query()
		if q.Get("plant_id") != "12345" {
			t.Errorf("expected plant_id %q, got %q", "12345", q.Get("plant_id"))
		}
		if q.Get("start_date") != "2025-01-01" {
			t.Errorf("expected start_date %q, got %q", "2025-01-01", q.Get("start_date"))
		}
		if q.Get("end_date") != "2025-01-07" {
			t.Errorf("expected end_date %q, got %q", "2025-01-07", q.Get("end_date"))
		}
		if q.Get("time_unit") != "day" {
			t.Errorf("expected time_unit %q, got %q", "day", q.Get("time_unit"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "plant_energy.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	energy, err := client.GetPlantEnergy(ctx, "12345", "2025-01-01", "2025-01-07", TimeUnitDay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if energy.PlantID != "12345" {
		t.Errorf("expected plant ID %q, got %q", "12345", energy.PlantID)
	}

	if len(energy.Datas) != 7 {
		t.Errorf("expected 7 data points, got %d", len(energy.Datas))
	}

	// Verify data is sorted by date
	for i := 1; i < len(energy.Datas); i++ {
		if energy.Datas[i].Date < energy.Datas[i-1].Date {
			t.Errorf("energy data not sorted: %s < %s", energy.Datas[i].Date, energy.Datas[i-1].Date)
		}
	}
}

func TestGetPlantPowerRange(t *testing.T) {
	callCount := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "plant_power.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	from, _ := time.Parse("2006-01-02", "2025-02-01")
	to, _ := time.Parse("2006-01-02", "2025-02-03")

	data, err := client.GetPlantPowerRange(ctx, "12345", from, to)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should make 3 API calls (Feb 1, 2, 3)
	if callCount != 3 {
		t.Errorf("expected 3 API calls, got %d", callCount)
	}

	if len(data) != 3 {
		t.Errorf("expected 3 days of data, got %d", len(data))
	}
}

func TestParsePowerData(t *testing.T) {
	powerData := &PowerData{
		PlantID: "12345",
		Date:    "2025-02-03",
		Powers: []PowerDataPoint{
			{Time: "06:00", Power: 0},
			{Time: "06:05", Power: 25.5},
			{Time: "12:30", Power: 4523.5},
			{Time: "18:55", Power: 25.5},
		},
	}

	parsed, err := ParsePowerData(powerData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(parsed) != 4 {
		t.Errorf("expected 4 parsed points, got %d", len(parsed))
	}

	// Check first point
	if parsed[0].Hour != 6 || parsed[0].Minute != 0 {
		t.Errorf("expected hour 6, minute 0, got hour %d, minute %d", parsed[0].Hour, parsed[0].Minute)
	}

	// Check noon point
	if parsed[2].Hour != 12 || parsed[2].Minute != 30 {
		t.Errorf("expected hour 12, minute 30, got hour %d, minute %d", parsed[2].Hour, parsed[2].Minute)
	}

	// Check date
	expectedDate, _ := time.Parse("2006-01-02", "2025-02-03")
	if !parsed[0].Date.Equal(expectedDate) {
		t.Errorf("expected date %v, got %v", expectedDate, parsed[0].Date)
	}
}
