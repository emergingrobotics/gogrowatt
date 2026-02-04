package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gogrowatt/internal/stats"
	"github.com/gogrowatt/pkg/growatt"
)

func TestWriteRawCSV(t *testing.T) {
	tmpDir := t.TempDir()
	filename := filepath.Join(tmpDir, "test_raw.csv")

	data := []growatt.PowerData{
		{
			PlantID: "12345",
			Date:    "2025-02-03",
			Powers: []growatt.PowerDataPoint{
				{Time: "06:00", Power: 0},
				{Time: "06:05", Power: 100.5},
				{Time: "12:00", Power: 4500.25},
			},
		},
	}

	err := writeRawCSV(filename, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read and verify
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 4 { // header + 3 data rows
		t.Errorf("expected 4 lines, got %d", len(lines))
	}

	// Check header
	if lines[0] != "date,time,power_watts" {
		t.Errorf("unexpected header: %s", lines[0])
	}

	// Check first data row
	if lines[1] != "2025-02-03,06:00,0.00" {
		t.Errorf("unexpected first data row: %s", lines[1])
	}

	// Check power value formatting
	if !strings.Contains(lines[3], "4500.25") {
		t.Errorf("expected power value 4500.25 in row: %s", lines[3])
	}
}

func TestWriteHourlyCSV(t *testing.T) {
	tmpDir := t.TempDir()
	filename := filepath.Join(tmpDir, "test_hourly.csv")

	// Create test daily stats
	day := &stats.DailyStats{Date: "2025-02-03"}
	for i := 0; i < 24; i++ {
		day.Hours[i] = stats.NewHourlyStats(i)
	}

	day.Hours[6].AddValue(100)
	day.Hours[6].AddValue(200)
	day.Hours[6].Finalize()

	day.Hours[12].AddValue(4500)
	day.Hours[12].AddValue(4600)
	day.Hours[12].Finalize()

	for i := 0; i < 24; i++ {
		if i != 6 && i != 12 {
			day.Hours[i].Finalize()
		}
	}

	data := []*stats.DailyStats{day}

	err := writeHourlyCSV(filename, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read and verify
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 25 { // header + 24 hours
		t.Errorf("expected 25 lines, got %d", len(lines))
	}

	// Check header
	if lines[0] != "date,hour,min_watts,max_watts,avg_watts,samples" {
		t.Errorf("unexpected header: %s", lines[0])
	}

	// Find hour 6 row (should be at index 7)
	var found6 bool
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "2025-02-03,6,") {
			found6 = true
			if !strings.Contains(line, ",2") { // 2 samples
				t.Errorf("expected 2 samples for hour 6: %s", line)
			}
			break
		}
	}
	if !found6 {
		t.Error("hour 6 row not found")
	}
}

func TestWriteStatsMarkdown(t *testing.T) {
	tmpDir := t.TempDir()
	filename := filepath.Join(tmpDir, "test_stats.md")

	multiDay := &stats.MultiDayStats{
		StartDate:       "2025-02-01",
		EndDate:         "2025-02-03",
		DaysAnalyzed:    3,
		TotalProduction: 100.5,
		DailyAverage:    33.5,
		PeakHour:        12,
		PeakPowerAvg:    4500.0,
	}

	for i := 0; i < 24; i++ {
		multiDay.ByHour[i] = &stats.AggregatedHourStats{
			Hour:       i,
			SampleDays: 3,
			Min:        0,
			Max:        1000,
			Average:    500,
			Median:     480,
			StdDev:     50,
		}
	}

	multiDay.ByHour[12].Min = 4000
	multiDay.ByHour[12].Max = 5000
	multiDay.ByHour[12].Average = 4500

	err := writeStatsMarkdown(filename, multiDay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read and verify
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	contentStr := string(content)

	// Check header
	if !strings.Contains(contentStr, "# Power Production Statistics") {
		t.Error("missing main header")
	}

	// Check period
	if !strings.Contains(contentStr, "**Period:** 2025-02-01 to 2025-02-03") {
		t.Error("missing period info")
	}

	// Check days analyzed
	if !strings.Contains(contentStr, "**Days Analyzed:** 3") {
		t.Error("missing days analyzed")
	}

	// Check summary table
	if !strings.Contains(contentStr, "| Peak Hour (avg) | 12:00 |") {
		t.Error("missing peak hour in summary")
	}

	// Check hourly stats table headers
	if !strings.Contains(contentStr, "| Hour | Min (W) | Max (W) | Average (W) | Median (W) | Std Dev | Days |") {
		t.Error("missing hourly stats table header")
	}

	// Check interpretation guide
	if !strings.Contains(contentStr, "## Interpretation Guide") {
		t.Error("missing interpretation guide")
	}
}

func TestResolvePlantID_FromFlag(t *testing.T) {
	// When flag is provided, use it directly (no API call needed)
	client := growatt.NewClient("test-token")
	ctx := context.Background()

	result, err := resolvePlantID(ctx, client, "flag-plant-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "flag-plant-id" {
		t.Errorf("expected %q, got %q", "flag-plant-id", result)
	}
}

func TestResolvePlantID_FromEnv(t *testing.T) {
	os.Setenv(EnvPlantID, "env-plant-id")
	defer os.Unsetenv(EnvPlantID)

	client := growatt.NewClient("test-token")
	ctx := context.Background()

	result, err := resolvePlantID(ctx, client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "env-plant-id" {
		t.Errorf("expected %q, got %q", "env-plant-id", result)
	}
}

func TestResolvePlantID_AutoDetectSingle(t *testing.T) {
	os.Unsetenv(EnvPlantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "success",
			"data": {
				"count": 1,
				"plants": [{"plant_id": "auto-detected-123", "plant_name": "My Solar"}]
			}
		}`))
	}))
	defer server.Close()

	client := growatt.NewClient("test-token",
		growatt.WithBaseURL(server.URL+"/"),
		growatt.WithRateLimit(0),
	)
	ctx := context.Background()

	result, err := resolvePlantID(ctx, client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "auto-detected-123" {
		t.Errorf("expected %q, got %q", "auto-detected-123", result)
	}
}

func TestResolvePlantID_MultiplePlantsError(t *testing.T) {
	os.Unsetenv(EnvPlantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "success",
			"data": {
				"count": 2,
				"plants": [
					{"plant_id": "plant-1", "plant_name": "Home"},
					{"plant_id": "plant-2", "plant_name": "Office"}
				]
			}
		}`))
	}))
	defer server.Close()

	client := growatt.NewClient("test-token",
		growatt.WithBaseURL(server.URL+"/"),
		growatt.WithRateLimit(0),
	)
	ctx := context.Background()

	_, err := resolvePlantID(ctx, client, "")
	if err == nil {
		t.Fatal("expected error for multiple plants, got nil")
	}

	if !strings.Contains(err.Error(), "multiple plants found") {
		t.Errorf("expected 'multiple plants found' error, got: %v", err)
	}
}

func TestResolvePlantID_NoPlantsError(t *testing.T) {
	os.Unsetenv(EnvPlantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "success",
			"data": {"count": 0, "plants": []}
		}`))
	}))
	defer server.Close()

	client := growatt.NewClient("test-token",
		growatt.WithBaseURL(server.URL+"/"),
		growatt.WithRateLimit(0),
	)
	ctx := context.Background()

	_, err := resolvePlantID(ctx, client, "")
	if err == nil {
		t.Fatal("expected error for no plants, got nil")
	}

	if !strings.Contains(err.Error(), "no plants found") {
		t.Errorf("expected 'no plants found' error, got: %v", err)
	}
}

func TestMultiDayOutput(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test data for multiple days
	data := []growatt.PowerData{
		{PlantID: "12345", Date: "2025-02-01", Powers: []growatt.PowerDataPoint{{Time: "12:00", Power: 4000}}},
		{PlantID: "12345", Date: "2025-02-02", Powers: []growatt.PowerDataPoint{{Time: "12:00", Power: 4500}}},
		{PlantID: "12345", Date: "2025-02-03", Powers: []growatt.PowerDataPoint{{Time: "12:00", Power: 4200}}},
	}

	// Write raw CSV
	rawFile := filepath.Join(tmpDir, "power_2025-02-01_to_2025-02-03.csv")
	err := writeRawCSV(rawFile, data)
	if err != nil {
		t.Fatalf("failed to write raw CSV: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(rawFile); os.IsNotExist(err) {
		t.Error("raw CSV file not created")
	}
}
