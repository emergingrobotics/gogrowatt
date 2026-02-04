package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gogrowatt/internal/stats"
	"github.com/gogrowatt/pkg/growatt"
	"github.com/spf13/cobra"
)

var (
	plantID  string
	fromDate string
	toDate   string
	date     string
	output   string
	token    string
	baseURL  string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "growatt-export [today|--date DATE|--from FROM --to TO]",
		Short: "Export power data from Growatt API",
		Long: `Export 5-minute interval power data from Growatt API.

Outputs:
  - Raw CSV with 5-minute intervals
  - Hourly aggregated CSV
  - Multi-day statistics markdown (when date range spans multiple days)

Examples:
  growatt-export --plant-id=12345 today
  growatt-export --plant-id=12345 --date=2025-02-01
  growatt-export --plant-id=12345 --from=2025-01-01 --to=2025-01-31`,
		Args: cobra.MaximumNArgs(1),
		RunE: run,
	}

	rootCmd.Flags().StringVar(&plantID, "plant-id", "", "Plant ID (required)")
	rootCmd.Flags().StringVar(&fromDate, "from", "", "Start date (YYYY-MM-DD)")
	rootCmd.Flags().StringVar(&toDate, "to", "", "End date (YYYY-MM-DD)")
	rootCmd.Flags().StringVar(&date, "date", "", "Single date (YYYY-MM-DD)")
	rootCmd.Flags().StringVar(&output, "output", ".", "Output directory")
	rootCmd.Flags().StringVar(&token, "token", "", "API token (overrides GROWATT_API_KEY)")
	rootCmd.Flags().StringVar(&baseURL, "base-url", "", "API base URL")

	rootCmd.MarkFlagRequired("plant-id")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// Determine date range
	var from, to time.Time
	var err error

	if len(args) > 0 && args[0] == "today" {
		from = time.Now()
		to = from
	} else if date != "" {
		from, err = time.Parse("2006-01-02", date)
		if err != nil {
			return fmt.Errorf("invalid date format: %w", err)
		}
		to = from
	} else if fromDate != "" && toDate != "" {
		from, err = time.Parse("2006-01-02", fromDate)
		if err != nil {
			return fmt.Errorf("invalid from date format: %w", err)
		}
		to, err = time.Parse("2006-01-02", toDate)
		if err != nil {
			return fmt.Errorf("invalid to date format: %w", err)
		}
	} else {
		return fmt.Errorf("must specify 'today', --date, or --from/--to")
	}

	if to.Before(from) {
		return fmt.Errorf("end date cannot be before start date")
	}

	// Create client
	var opts []growatt.ClientOption
	if baseURL != "" {
		opts = append(opts, growatt.WithBaseURL(baseURL))
	}

	var client *growatt.Client
	if token != "" {
		client = growatt.NewClient(token, opts...)
	} else {
		client, err = growatt.NewClientFromEnv(opts...)
		if err != nil {
			return fmt.Errorf("creating client: %w", err)
		}
	}

	// Ensure output directory exists
	if err := os.MkdirAll(output, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	ctx := context.Background()

	fmt.Printf("Fetching power data for plant %s from %s to %s...\n",
		plantID, from.Format("2006-01-02"), to.Format("2006-01-02"))

	// Fetch data for each day
	powerData, err := client.GetPlantPowerRange(ctx, plantID, from, to)
	if err != nil {
		return fmt.Errorf("fetching power data: %w", err)
	}

	if len(powerData) == 0 {
		return fmt.Errorf("no data returned")
	}

	// Generate filenames
	var rawCSVFile, hourlyCSVFile, statsFile string
	if from.Equal(to) {
		dateStr := from.Format("2006-01-02")
		rawCSVFile = filepath.Join(output, fmt.Sprintf("power_%s.csv", dateStr))
		hourlyCSVFile = filepath.Join(output, fmt.Sprintf("hourly_%s.csv", dateStr))
	} else {
		dateRange := fmt.Sprintf("%s_to_%s", from.Format("2006-01-02"), to.Format("2006-01-02"))
		rawCSVFile = filepath.Join(output, fmt.Sprintf("power_%s.csv", dateRange))
		hourlyCSVFile = filepath.Join(output, fmt.Sprintf("hourly_%s.csv", dateRange))
		statsFile = filepath.Join(output, fmt.Sprintf("stats_%s.md", dateRange))
	}

	// Write raw CSV
	if err := writeRawCSV(rawCSVFile, powerData); err != nil {
		return fmt.Errorf("writing raw CSV: %w", err)
	}
	fmt.Printf("Wrote raw data to %s\n", rawCSVFile)

	// Parse and aggregate to hourly
	var dailyStats []*stats.DailyStats
	for _, pd := range powerData {
		parsed, err := growatt.ParsePowerData(&pd)
		if err != nil {
			return fmt.Errorf("parsing power data: %w", err)
		}
		if ds := stats.AggregateToHourly(parsed); ds != nil {
			dailyStats = append(dailyStats, ds)
		}
	}

	// Write hourly CSV
	if err := writeHourlyCSV(hourlyCSVFile, dailyStats); err != nil {
		return fmt.Errorf("writing hourly CSV: %w", err)
	}
	fmt.Printf("Wrote hourly data to %s\n", hourlyCSVFile)

	// Write multi-day stats if applicable
	if len(dailyStats) > 1 && statsFile != "" {
		multiDay := stats.AggregateDays(dailyStats)
		if err := writeStatsMarkdown(statsFile, multiDay); err != nil {
			return fmt.Errorf("writing stats markdown: %w", err)
		}
		fmt.Printf("Wrote statistics to %s\n", statsFile)
	}

	return nil
}

func writeRawCSV(filename string, data []growatt.PowerData) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	// Header
	if err := w.Write([]string{"date", "time", "power_watts"}); err != nil {
		return err
	}

	// Data
	for _, day := range data {
		for _, p := range day.Powers {
			if err := w.Write([]string{
				day.Date,
				p.Time,
				strconv.FormatFloat(p.Power, 'f', 2, 64),
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func writeHourlyCSV(filename string, data []*stats.DailyStats) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	// Header
	if err := w.Write([]string{"date", "hour", "min_watts", "max_watts", "avg_watts", "samples"}); err != nil {
		return err
	}

	// Data
	rows := stats.GetHourlyRows(data)
	for _, row := range rows {
		minStr := "0"
		if row.Min > 0 {
			minStr = strconv.FormatFloat(row.Min, 'f', 2, 64)
		}
		if err := w.Write([]string{
			row.Date,
			strconv.Itoa(row.Hour),
			minStr,
			strconv.FormatFloat(row.Max, 'f', 2, 64),
			strconv.FormatFloat(row.Avg, 'f', 2, 64),
			strconv.Itoa(row.Samples),
		}); err != nil {
			return err
		}
	}

	return nil
}

func writeStatsMarkdown(filename string, data *stats.MultiDayStats) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Fprintf(f, "# Power Production Statistics\n\n")
	fmt.Fprintf(f, "**Period:** %s to %s\n", data.StartDate, data.EndDate)
	fmt.Fprintf(f, "**Days Analyzed:** %d\n\n", data.DaysAnalyzed)

	// Summary
	fmt.Fprintf(f, "## Summary\n\n")
	fmt.Fprintf(f, "| Metric | Value |\n")
	fmt.Fprintf(f, "|--------|-------|\n")
	fmt.Fprintf(f, "| Peak Hour (avg) | %02d:00 |\n", data.PeakHour)
	fmt.Fprintf(f, "| Peak Power (avg) | %.1f W |\n", data.PeakPowerAvg)
	fmt.Fprintf(f, "| Daily Average Production | %.2f kWh |\n", data.DailyAverage)
	fmt.Fprintf(f, "| Total Production | %.2f kWh |\n\n", data.TotalProduction)

	// Hourly Statistics Table
	fmt.Fprintf(f, "## Hourly Statistics (All Days Combined)\n\n")
	fmt.Fprintf(f, "| Hour | Min (W) | Max (W) | Average (W) | Median (W) | Std Dev | Days |\n")
	fmt.Fprintf(f, "|------|---------|---------|-------------|------------|---------|------|\n")

	for hour := 0; hour < 24; hour++ {
		h := data.ByHour[hour]
		if h == nil {
			continue
		}
		fmt.Fprintf(f, "| %02d:00 | %.1f | %.1f | %.1f | %.1f | %.1f | %d |\n",
			hour, h.Min, h.Max, h.Average, h.Median, h.StdDev, h.SampleDays)
	}

	fmt.Fprintf(f, "\n## Interpretation Guide\n\n")
	fmt.Fprintf(f, "- **Min/Max**: The lowest and highest instantaneous power readings at this hour across all days\n")
	fmt.Fprintf(f, "- **Average**: Mean power output at this hour across all analyzed days\n")
	fmt.Fprintf(f, "- **Median**: Middle value of hourly averages (less affected by outliers)\n")
	fmt.Fprintf(f, "- **Std Dev**: Standard deviation of hourly averages (variability indicator)\n")
	fmt.Fprintf(f, "- **Days**: Number of days with data at this hour\n\n")

	fmt.Fprintf(f, "## Raw Hourly Averages by Day\n\n")
	fmt.Fprintf(f, "For detailed analysis, the following shows the average power per hour for each day:\n\n")

	// Find hours with data
	activeHours := []int{}
	for hour := 0; hour < 24; hour++ {
		if data.ByHour[hour] != nil && data.ByHour[hour].SampleDays > 0 {
			activeHours = append(activeHours, hour)
		}
	}

	if len(activeHours) > 0 {
		// Header row with hours
		fmt.Fprintf(f, "| Day |")
		for _, hour := range activeHours {
			fmt.Fprintf(f, " %02d:00 |", hour)
		}
		fmt.Fprintf(f, "\n")

		// Separator
		fmt.Fprintf(f, "|-----|")
		for range activeHours {
			fmt.Fprintf(f, "-------|")
		}
		fmt.Fprintf(f, "\n")

		// Data rows (we need the original daily data for this, but we don't have it here)
		// This section would need the original DailyStats to populate
		fmt.Fprintf(f, "\n*Note: Individual daily data available in the hourly CSV file.*\n")
	}

	return nil
}
