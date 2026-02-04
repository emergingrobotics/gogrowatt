package stats

import (
	"math"
	"testing"
	"time"

	"github.com/gogrowatt/pkg/growatt"
)

func TestCalculateStdDev(t *testing.T) {
	tests := []struct {
		name     string
		values   []float64
		mean     float64
		expected float64
	}{
		{
			name:     "empty values",
			values:   []float64{},
			mean:     0,
			expected: 0,
		},
		{
			name:     "single value",
			values:   []float64{5.0},
			mean:     5.0,
			expected: 0,
		},
		{
			name:     "identical values",
			values:   []float64{5.0, 5.0, 5.0, 5.0},
			mean:     5.0,
			expected: 0,
		},
		{
			name:     "simple values",
			values:   []float64{2, 4, 4, 4, 5, 5, 7, 9},
			mean:     5.0,
			expected: 2.138,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateStdDev(tt.values, tt.mean)
			if math.Abs(result-tt.expected) > 0.01 {
				t.Errorf("expected %.3f, got %.3f", tt.expected, result)
			}
		})
	}
}

func TestCalculateMedian(t *testing.T) {
	tests := []struct {
		name     string
		values   []float64
		expected float64
	}{
		{
			name:     "empty values",
			values:   []float64{},
			expected: 0,
		},
		{
			name:     "single value",
			values:   []float64{5.0},
			expected: 5.0,
		},
		{
			name:     "odd count",
			values:   []float64{1, 3, 5},
			expected: 3,
		},
		{
			name:     "even count",
			values:   []float64{1, 2, 3, 4},
			expected: 2.5,
		},
		{
			name:     "unsorted",
			values:   []float64{5, 2, 8, 1, 9},
			expected: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateMedian(tt.values)
			if result != tt.expected {
				t.Errorf("expected %f, got %f", tt.expected, result)
			}
		})
	}
}

func TestHourlyStats(t *testing.T) {
	h := NewHourlyStats(12)

	// Add values
	values := []float64{100, 200, 300, 400, 500}
	for _, v := range values {
		h.AddValue(v)
	}

	h.Finalize()

	if h.Hour != 12 {
		t.Errorf("expected hour 12, got %d", h.Hour)
	}

	if h.Samples != 5 {
		t.Errorf("expected 5 samples, got %d", h.Samples)
	}

	if h.Min != 100 {
		t.Errorf("expected min 100, got %f", h.Min)
	}

	if h.Max != 500 {
		t.Errorf("expected max 500, got %f", h.Max)
	}

	expectedMean := 300.0
	if h.Mean != expectedMean {
		t.Errorf("expected mean %f, got %f", expectedMean, h.Mean)
	}

	if h.Sum != 1500 {
		t.Errorf("expected sum 1500, got %f", h.Sum)
	}

	// StdDev should be non-zero for these values
	if h.StdDev == 0 {
		t.Error("expected non-zero stddev")
	}
}

func TestHourlyStatsEmpty(t *testing.T) {
	h := NewHourlyStats(0)
	h.Finalize()

	if h.Min != 0 {
		t.Errorf("expected min 0, got %f", h.Min)
	}

	if h.Max != 0 {
		t.Errorf("expected max 0, got %f", h.Max)
	}
}

func TestAggregateToHourly(t *testing.T) {
	date, _ := time.Parse("2006-01-02", "2025-02-03")

	// Create test data with 5-minute intervals
	data := []growatt.ParsedPowerData{
		{Date: date, Time: "06:00", Power: 0, Hour: 6, Minute: 0},
		{Date: date, Time: "06:05", Power: 100, Hour: 6, Minute: 5},
		{Date: date, Time: "06:10", Power: 200, Hour: 6, Minute: 10},
		{Date: date, Time: "06:15", Power: 300, Hour: 6, Minute: 15},
		{Date: date, Time: "07:00", Power: 500, Hour: 7, Minute: 0},
		{Date: date, Time: "07:05", Power: 600, Hour: 7, Minute: 5},
		{Date: date, Time: "12:00", Power: 4500, Hour: 12, Minute: 0},
		{Date: date, Time: "12:05", Power: 4600, Hour: 12, Minute: 5},
	}

	stats := AggregateToHourly(data)

	if stats == nil {
		t.Fatal("expected non-nil stats")
	}

	if stats.Date != "2025-02-03" {
		t.Errorf("expected date %q, got %q", "2025-02-03", stats.Date)
	}

	// Check hour 6
	h6 := stats.Hours[6]
	if h6.Samples != 4 {
		t.Errorf("expected 4 samples for hour 6, got %d", h6.Samples)
	}
	if h6.Min != 0 {
		t.Errorf("expected min 0 for hour 6, got %f", h6.Min)
	}
	if h6.Max != 300 {
		t.Errorf("expected max 300 for hour 6, got %f", h6.Max)
	}

	// Check hour 7
	h7 := stats.Hours[7]
	if h7.Samples != 2 {
		t.Errorf("expected 2 samples for hour 7, got %d", h7.Samples)
	}

	// Check hour 12
	h12 := stats.Hours[12]
	if h12.Samples != 2 {
		t.Errorf("expected 2 samples for hour 12, got %d", h12.Samples)
	}

	// Check hour with no data
	h0 := stats.Hours[0]
	if h0.Samples != 0 {
		t.Errorf("expected 0 samples for hour 0, got %d", h0.Samples)
	}
}

func TestAggregateToHourlyEmpty(t *testing.T) {
	stats := AggregateToHourly(nil)
	if stats != nil {
		t.Error("expected nil stats for empty input")
	}

	stats = AggregateToHourly([]growatt.ParsedPowerData{})
	if stats != nil {
		t.Error("expected nil stats for empty slice")
	}
}

func TestAggregateDays(t *testing.T) {
	// Create stats for multiple days
	day1 := &DailyStats{Date: "2025-02-01"}
	day2 := &DailyStats{Date: "2025-02-02"}
	day3 := &DailyStats{Date: "2025-02-03"}

	for i := 0; i < 24; i++ {
		day1.Hours[i] = NewHourlyStats(i)
		day2.Hours[i] = NewHourlyStats(i)
		day3.Hours[i] = NewHourlyStats(i)
	}

	// Add data to hour 12 for all days
	day1.Hours[12].AddValue(4000)
	day1.Hours[12].AddValue(4200)
	day1.Hours[12].Finalize()

	day2.Hours[12].AddValue(4500)
	day2.Hours[12].AddValue(4700)
	day2.Hours[12].Finalize()

	day3.Hours[12].AddValue(4100)
	day3.Hours[12].AddValue(4300)
	day3.Hours[12].Finalize()

	// Finalize all hours
	for i := 0; i < 24; i++ {
		if i != 12 {
			day1.Hours[i].Finalize()
			day2.Hours[i].Finalize()
			day3.Hours[i].Finalize()
		}
	}

	days := []*DailyStats{day1, day2, day3}
	multiDay := AggregateDays(days)

	if multiDay == nil {
		t.Fatal("expected non-nil multi-day stats")
	}

	if multiDay.DaysAnalyzed != 3 {
		t.Errorf("expected 3 days analyzed, got %d", multiDay.DaysAnalyzed)
	}

	if multiDay.StartDate != "2025-02-01" {
		t.Errorf("expected start date %q, got %q", "2025-02-01", multiDay.StartDate)
	}

	if multiDay.EndDate != "2025-02-03" {
		t.Errorf("expected end date %q, got %q", "2025-02-03", multiDay.EndDate)
	}

	// Check hour 12 aggregation
	h12 := multiDay.ByHour[12]
	if h12.SampleDays != 3 {
		t.Errorf("expected 3 sample days for hour 12, got %d", h12.SampleDays)
	}

	if h12.Min != 4000 {
		t.Errorf("expected min 4000 for hour 12, got %f", h12.Min)
	}

	if h12.Max != 4700 {
		t.Errorf("expected max 4700 for hour 12, got %f", h12.Max)
	}

	// Check peak hour
	if multiDay.PeakHour != 12 {
		t.Errorf("expected peak hour 12, got %d", multiDay.PeakHour)
	}
}

func TestAggregateDaysEmpty(t *testing.T) {
	result := AggregateDays(nil)
	if result != nil {
		t.Error("expected nil for nil input")
	}

	result = AggregateDays([]*DailyStats{})
	if result != nil {
		t.Error("expected nil for empty slice")
	}
}

func TestGetHourlyRows(t *testing.T) {
	day := &DailyStats{Date: "2025-02-03"}
	for i := 0; i < 24; i++ {
		day.Hours[i] = NewHourlyStats(i)
	}

	day.Hours[6].AddValue(100)
	day.Hours[6].AddValue(200)
	day.Hours[6].Finalize()

	day.Hours[12].AddValue(4500)
	day.Hours[12].Finalize()

	for i := 0; i < 24; i++ {
		if i != 6 && i != 12 {
			day.Hours[i].Finalize()
		}
	}

	rows := GetHourlyRows([]*DailyStats{day})

	if len(rows) != 24 {
		t.Errorf("expected 24 rows, got %d", len(rows))
	}

	// Find hour 6 row
	var hour6Row HourlyRow
	for _, row := range rows {
		if row.Hour == 6 {
			hour6Row = row
			break
		}
	}

	if hour6Row.Samples != 2 {
		t.Errorf("expected 2 samples for hour 6, got %d", hour6Row.Samples)
	}

	if hour6Row.Min != 100 {
		t.Errorf("expected min 100 for hour 6, got %f", hour6Row.Min)
	}

	if hour6Row.Max != 200 {
		t.Errorf("expected max 200 for hour 6, got %f", hour6Row.Max)
	}
}
