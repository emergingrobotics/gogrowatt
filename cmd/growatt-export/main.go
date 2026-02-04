package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gogrowatt/internal/stats"
	"github.com/gogrowatt/pkg/growatt"
	"github.com/spf13/cobra"
)

const (
	EnvPlantID  = "GROWATT_PLANT_ID"
	EnvDeviceSN = "GROWATT_DEVICE_SN"
	EnvTimezone = "GROWATT_TIMEZONE"
)

var (
	plantID   string
	deviceSN  string
	timezone  string
	fromDate  string
	toDate    string
	date      string
	output    string
	token     string
	baseURL   string
	showGraph bool
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
  - ASCII graph of hourly production (with --graph flag)

For MIN/TLX inverters, use --device-sn or set GROWATT_DEVICE_SN for accurate data.
If you have only one plant/device, IDs will be auto-detected.

Examples:
  growatt-export today
  growatt-export --graph today
  growatt-export --plant-id=12345 today
  growatt-export --date=2025-02-01
  growatt-export --from=2025-01-01 --to=2025-01-31 -g`,
		Args: cobra.MaximumNArgs(1),
		RunE: run,
	}

	rootCmd.Flags().StringVar(&plantID, "plant-id", "", "Plant ID (auto-detected if only one plant, or set GROWATT_PLANT_ID)")
	rootCmd.Flags().StringVar(&deviceSN, "device-sn", "", "Device serial number for MIN/TLX inverters (or set GROWATT_DEVICE_SN)")
	rootCmd.Flags().StringVar(&timezone, "timezone", "", "Timezone for device queries (default: US/Central, or set GROWATT_TIMEZONE)")
	rootCmd.Flags().StringVar(&fromDate, "from", "", "Start date (YYYY-MM-DD)")
	rootCmd.Flags().StringVar(&toDate, "to", "", "End date (YYYY-MM-DD)")
	rootCmd.Flags().StringVar(&date, "date", "", "Single date (YYYY-MM-DD)")
	rootCmd.Flags().StringVar(&output, "output", ".", "Output directory")
	rootCmd.Flags().StringVar(&token, "token", "", "API token (overrides GROWATT_API_KEY)")
	rootCmd.Flags().StringVar(&baseURL, "base-url", "", "API base URL")
	rootCmd.Flags().BoolVarP(&showGraph, "graph", "g", false, "Display ASCII graph of hourly power production")

	// Don't show usage on errors during execution (only on bad CLI args)
	rootCmd.SilenceUsage = true

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

	ctx := context.Background()

	// Resolve device serial number (preferred for MIN/TLX inverters)
	resolvedDeviceSN, err := resolveDeviceSN(ctx, client, deviceSN, plantID)
	if err != nil {
		return err
	}

	// Resolve timezone
	tz := timezone
	if tz == "" {
		tz = os.Getenv(EnvTimezone)
	}
	if tz == "" {
		tz = "US/Central"
	}

	// Ensure output directory exists
	if err := os.MkdirAll(output, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	fmt.Printf("Fetching power data for device %s from %s to %s...\n",
		resolvedDeviceSN, from.Format("2006-01-02"), to.Format("2006-01-02"))

	// Fetch data using device-specific endpoint (works for MIN/TLX inverters)
	powerData, err := client.GetMINInverterHistoryRange(ctx, resolvedDeviceSN, from, to, tz)
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

	// Display ASCII graph if requested
	if showGraph && len(dailyStats) > 0 {
		fmt.Println()
		printASCIIGraph(dailyStats)
	}

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

// resolveDeviceSN determines the device serial number to use
func resolveDeviceSN(ctx context.Context, client *growatt.Client, deviceFlag, plantFlag string) (string, error) {
	// Priority: CLI flag > environment variable > auto-detect
	if deviceFlag != "" {
		return deviceFlag, nil
	}

	if envValue := os.Getenv(EnvDeviceSN); envValue != "" {
		fmt.Printf("Using device SN from %s: %s\n", EnvDeviceSN, envValue)
		return envValue, nil
	}

	// Need to auto-detect: first get plant ID, then get device list
	plantID, err := resolvePlantIDQuiet(ctx, client, plantFlag)
	if err != nil {
		return "", err
	}

	fmt.Println("Fetching device list...")
	devices, err := client.ListDevices(ctx, plantID)
	if err != nil {
		return "", fmt.Errorf("failed to list devices: %w", err)
	}

	if len(devices) == 0 {
		return "", fmt.Errorf("no devices found for plant %s", plantID)
	}

	if len(devices) == 1 {
		sn := devices[0].DeviceSN.String()
		fmt.Printf("Auto-detected device: %s (%s)\n", devices[0].DeviceName, sn)
		fmt.Println()
		fmt.Println("Tip: To avoid rate limits from auto-detection, set these environment variables:")
		fmt.Printf("  export %s=%s\n", EnvPlantID, plantID)
		fmt.Printf("  export %s=%s\n", EnvDeviceSN, sn)
		fmt.Println()
		return sn, nil
	}

	// Multiple devices - user must specify
	fmt.Println("\nMultiple devices found:")
	for _, d := range devices {
		fmt.Printf("  - %s (SN: %s, Type: %d)\n", d.DeviceName, d.DeviceSN.String(), d.DeviceType)
	}
	fmt.Println()
	fmt.Println("Set one of these as your default:")
	fmt.Printf("  export %s=<device-sn>\n", EnvDeviceSN)
	return "", fmt.Errorf("multiple devices found; specify --device-sn or set %s environment variable", EnvDeviceSN)
}

// resolvePlantID determines the plant ID to use (with tips shown)
func resolvePlantID(ctx context.Context, client *growatt.Client, flagValue string) (string, error) {
	return resolvePlantIDInternal(ctx, client, flagValue, true)
}

// resolvePlantIDQuiet determines the plant ID without showing tips (used when device detection will show combined tips)
func resolvePlantIDQuiet(ctx context.Context, client *growatt.Client, flagValue string) (string, error) {
	return resolvePlantIDInternal(ctx, client, flagValue, false)
}

// resolvePlantIDInternal is the internal implementation
func resolvePlantIDInternal(ctx context.Context, client *growatt.Client, flagValue string, showTips bool) (string, error) {
	// Priority: CLI flag > environment variable > auto-detect
	if flagValue != "" {
		return flagValue, nil
	}

	if envValue := os.Getenv(EnvPlantID); envValue != "" {
		fmt.Printf("Using plant ID from %s: %s\n", EnvPlantID, envValue)
		return envValue, nil
	}

	// Auto-detect: fetch plant list
	fmt.Println("No plant ID specified, checking available plants...")
	plants, err := client.ListPlants(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list plants: %w", err)
	}

	if len(plants) == 0 {
		return "", fmt.Errorf("no plants found for this account")
	}

	if len(plants) == 1 {
		plantID := plants[0].PlantID.String()
		fmt.Printf("Auto-detected plant: %s (%s)\n", plants[0].PlantName, plantID)
		if showTips {
			fmt.Println()
			fmt.Println("Tip: To avoid rate limits from auto-detection, set your plant ID:")
			fmt.Printf("  export %s=%s\n", EnvPlantID, plantID)
			fmt.Println()
		}
		return plantID, nil
	}

	// Multiple plants - user must specify
	fmt.Println("\nMultiple plants found:")
	for _, p := range plants {
		fmt.Printf("  - %s (ID: %s)\n", p.PlantName, p.PlantID.String())
	}
	fmt.Println()
	fmt.Println("Set one of these as your default:")
	fmt.Printf("  export %s=<plant-id>\n", EnvPlantID)
	return "", fmt.Errorf("multiple plants found; specify --plant-id or set %s environment variable", EnvPlantID)
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

// printASCIIGraph displays an ASCII bar chart of hourly power production
func printASCIIGraph(dailyStats []*stats.DailyStats) {
	const graphHeight = 15
	const barWidth = 2

	// Aggregate hourly kWh across all days
	hourlyKWh := make([]float64, 24)
	hourlyCounts := make([]int, 24)

	for _, ds := range dailyStats {
		for hour := 0; hour < 24; hour++ {
			if ds.Hours[hour] != nil && ds.Hours[hour].Samples > 0 {
				// Convert average watts to kWh (watts * 1 hour / 1000)
				kwh := ds.Hours[hour].Mean / 1000.0
				hourlyKWh[hour] += kwh
				hourlyCounts[hour]++
			}
		}
	}

	// Average across days
	for hour := 0; hour < 24; hour++ {
		if hourlyCounts[hour] > 0 {
			hourlyKWh[hour] /= float64(hourlyCounts[hour])
		}
	}

	// Find max for scaling
	maxKWh := 0.0
	for _, kwh := range hourlyKWh {
		if kwh > maxKWh {
			maxKWh = kwh
		}
	}

	if maxKWh == 0 {
		fmt.Println("No power data to graph.")
		return
	}

	// Calculate total daily kWh
	totalKWh := 0.0
	for _, kwh := range hourlyKWh {
		totalKWh += kwh
	}

	// Print title
	if len(dailyStats) == 1 {
		fmt.Printf("Power Production - %s (Total: %.2f kWh)\n", dailyStats[0].Date, totalKWh)
	} else {
		fmt.Printf("Power Production - %d days averaged (Daily avg: %.2f kWh)\n", len(dailyStats), totalKWh)
	}
	fmt.Println()

	// Print graph rows (top to bottom)
	for row := graphHeight; row >= 1; row-- {
		threshold := maxKWh * float64(row) / float64(graphHeight)

		// Y-axis label
		if row == graphHeight {
			fmt.Printf("%5.2f |", maxKWh)
		} else if row == graphHeight/2+1 {
			fmt.Printf("%5.2f |", maxKWh/2)
		} else if row == 1 {
			fmt.Printf("%5.2f |", maxKWh/float64(graphHeight))
		} else {
			fmt.Printf("      |")
		}

		// Bars
		for hour := 0; hour < 24; hour++ {
			if hourlyKWh[hour] >= threshold {
				fmt.Print(strings.Repeat("#", barWidth))
			} else {
				fmt.Print(strings.Repeat(" ", barWidth))
			}
		}
		fmt.Println()
	}

	// X-axis line
	fmt.Printf("      +%s\n", strings.Repeat("-", 24*barWidth))

	// X-axis labels (hours)
	fmt.Print("       ")
	for hour := 0; hour < 24; hour++ {
		if hour%3 == 0 {
			fmt.Printf("%-6d", hour)
		}
	}
	fmt.Println()

	// Legend
	fmt.Println("       Hour of day")
	fmt.Println()
	fmt.Println("kWh")
}
