package stats

import (
	"math"
	"sort"

	"github.com/gogrowatt/pkg/growatt"
)

// HourlyStats represents statistics for a single hour
type HourlyStats struct {
	Hour    int
	Samples int
	Min     float64
	Max     float64
	Sum     float64
	Mean    float64
	StdDev  float64
	Values  []float64 // Raw values for further calculations
}

// DailyStats represents statistics for a single day
type DailyStats struct {
	Date  string
	Hours [24]*HourlyStats
}

// AggregatedHourStats represents stats for an hour across multiple days
type AggregatedHourStats struct {
	Hour       int
	SampleDays int
	Min        float64 // Minimum of all values at this hour across days
	Max        float64 // Maximum of all values at this hour across days
	Average    float64 // Average of all values at this hour across days
	Median     float64 // Median of hourly averages
	StdDev     float64 // Standard deviation of hourly averages
	Values     []float64
}

// MultiDayStats represents statistics across multiple days
type MultiDayStats struct {
	StartDate       string
	EndDate         string
	DaysAnalyzed    int
	ByHour          [24]*AggregatedHourStats
	TotalProduction float64
	DailyAverage    float64
	PeakHour        int
	PeakPowerAvg    float64
}

// NewHourlyStats creates a new HourlyStats for the given hour
func NewHourlyStats(hour int) *HourlyStats {
	return &HourlyStats{
		Hour:   hour,
		Min:    math.MaxFloat64,
		Max:    -math.MaxFloat64,
		Values: make([]float64, 0),
	}
}

// AddValue adds a power value to the hourly stats
func (h *HourlyStats) AddValue(power float64) {
	h.Samples++
	h.Sum += power
	h.Values = append(h.Values, power)

	if power < h.Min {
		h.Min = power
	}
	if power > h.Max {
		h.Max = power
	}
}

// Finalize calculates mean and stddev after all values are added
func (h *HourlyStats) Finalize() {
	if h.Samples == 0 {
		h.Min = 0
		h.Max = 0
		return
	}

	h.Mean = h.Sum / float64(h.Samples)
	h.StdDev = CalculateStdDev(h.Values, h.Mean)
}

// CalculateStdDev calculates the standard deviation
func CalculateStdDev(values []float64, mean float64) float64 {
	if len(values) < 2 {
		return 0
	}

	var sumSquares float64
	for _, v := range values {
		diff := v - mean
		sumSquares += diff * diff
	}

	variance := sumSquares / float64(len(values)-1)
	return math.Sqrt(variance)
}

// CalculateMedian calculates the median of a slice of values
func CalculateMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	n := len(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// AggregateToHourly converts 5-minute power data to hourly statistics
func AggregateToHourly(data []growatt.ParsedPowerData) *DailyStats {
	if len(data) == 0 {
		return nil
	}

	stats := &DailyStats{
		Date: data[0].Date.Format("2006-01-02"),
	}

	// Initialize all hours
	for i := 0; i < 24; i++ {
		stats.Hours[i] = NewHourlyStats(i)
	}

	// Add values to appropriate hours
	for _, p := range data {
		if p.Hour >= 0 && p.Hour < 24 {
			stats.Hours[p.Hour].AddValue(p.Power)
		}
	}

	// Finalize all hours
	for i := 0; i < 24; i++ {
		stats.Hours[i].Finalize()
	}

	return stats
}

// AggregateDays combines statistics from multiple days
func AggregateDays(days []*DailyStats) *MultiDayStats {
	if len(days) == 0 {
		return nil
	}

	result := &MultiDayStats{
		StartDate:    days[0].Date,
		EndDate:      days[len(days)-1].Date,
		DaysAnalyzed: len(days),
	}

	// Initialize hourly aggregates
	for i := 0; i < 24; i++ {
		result.ByHour[i] = &AggregatedHourStats{
			Hour:   i,
			Min:    math.MaxFloat64,
			Max:    -math.MaxFloat64,
			Values: make([]float64, 0),
		}
	}

	// Aggregate data from all days
	for _, day := range days {
		for hour := 0; hour < 24; hour++ {
			hourStats := day.Hours[hour]
			if hourStats == nil || hourStats.Samples == 0 {
				continue
			}

			agg := result.ByHour[hour]
			agg.SampleDays++

			// Track min/max across all individual readings
			if hourStats.Min < agg.Min {
				agg.Min = hourStats.Min
			}
			if hourStats.Max > agg.Max {
				agg.Max = hourStats.Max
			}

			// Store hourly means for aggregation
			agg.Values = append(agg.Values, hourStats.Mean)
		}
	}

	// Calculate final statistics for each hour
	var maxAvg float64
	for hour := 0; hour < 24; hour++ {
		agg := result.ByHour[hour]

		if agg.SampleDays == 0 {
			agg.Min = 0
			agg.Max = 0
			continue
		}

		// Calculate average of hourly means
		var sum float64
		for _, v := range agg.Values {
			sum += v
		}
		agg.Average = sum / float64(len(agg.Values))
		agg.Median = CalculateMedian(agg.Values)
		agg.StdDev = CalculateStdDev(agg.Values, agg.Average)

		if agg.Average > maxAvg {
			maxAvg = agg.Average
			result.PeakHour = hour
			result.PeakPowerAvg = agg.Average
		}
	}

	// Calculate total and daily average production (estimated from power)
	// Assuming each hourly reading represents average power for that hour
	for _, day := range days {
		var dailyEnergy float64
		for hour := 0; hour < 24; hour++ {
			if day.Hours[hour] != nil {
				// Convert W to kWh (power * 1 hour / 1000)
				dailyEnergy += day.Hours[hour].Mean / 1000.0
			}
		}
		result.TotalProduction += dailyEnergy
	}

	if result.DaysAnalyzed > 0 {
		result.DailyAverage = result.TotalProduction / float64(result.DaysAnalyzed)
	}

	return result
}

// HourlyRow represents a single row in the hourly output
type HourlyRow struct {
	Date    string
	Hour    int
	Min     float64
	Max     float64
	Avg     float64
	Samples int
}

// GetHourlyRows returns all hourly data as rows for CSV export
func GetHourlyRows(days []*DailyStats) []HourlyRow {
	var rows []HourlyRow

	for _, day := range days {
		for hour := 0; hour < 24; hour++ {
			h := day.Hours[hour]
			if h == nil {
				continue
			}
			rows = append(rows, HourlyRow{
				Date:    day.Date,
				Hour:    hour,
				Min:     h.Min,
				Max:     h.Max,
				Avg:     h.Mean,
				Samples: h.Samples,
			})
		}
	}

	return rows
}
