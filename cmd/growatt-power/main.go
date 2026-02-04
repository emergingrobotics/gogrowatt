package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gogrowatt/pkg/growatt"
	"github.com/spf13/cobra"
)

const (
	EnvPlantID = "GROWATT_PLANT_ID"
)

var (
	plantID      string
	token        string
	baseURL      string
	jsonOutput   bool
	continuous   int
)

// PowerOutput is the JSON output structure
type PowerOutput struct {
	PlantID      string  `json:"plant_id"`
	PlantName    string  `json:"plant_name"`
	CurrentPower float64 `json:"current_power_watts"`
	TodayEnergy  float64 `json:"today_energy_kwh"`
	TotalEnergy  float64 `json:"total_energy_kwh"`
	PeakPower    float64 `json:"peak_power_kw"`
	Status       int     `json:"status"`
	Timestamp    string  `json:"timestamp,omitempty"`
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "growatt-power",
		Short: "Get current power output from Growatt plant",
		Long: `Get the instantaneous power output from a Growatt solar plant.

By default outputs a human-readable text string (watts).
Use -j or --json for JSON output suitable for piping to other programs.
Use -c to poll continuously (default 60s, or specify interval).

Examples:
  growatt-power
  growatt-power --plant-id=12345
  growatt-power -j
  growatt-power -c              # poll every 60 seconds
  growatt-power -c 30           # poll every 30 seconds
  growatt-power --json | jq .current_power_watts`,
		RunE: run,
	}

	rootCmd.Flags().StringVar(&plantID, "plant-id", "", "Plant ID (auto-detected if only one plant, or set GROWATT_PLANT_ID)")
	rootCmd.Flags().StringVar(&token, "token", "", "API token (overrides GROWATT_API_KEY)")
	rootCmd.Flags().StringVar(&baseURL, "base-url", "", "API base URL")
	rootCmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	rootCmd.Flags().IntVarP(&continuous, "continuous", "c", 0, "Poll continuously every N seconds (default 60 if flag used without value)")
	rootCmd.Flag("continuous").NoOptDefVal = "60"

	rootCmd.SilenceUsage = true

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// Create client
	var opts []growatt.ClientOption
	if baseURL != "" {
		opts = append(opts, growatt.WithBaseURL(baseURL))
	}

	var client *growatt.Client
	var err error
	if token != "" {
		client = growatt.NewClient(token, opts...)
	} else {
		client, err = growatt.NewClientFromEnv(opts...)
		if err != nil {
			return fmt.Errorf("creating client: %w", err)
		}
	}

	// Resolve target plant ID once
	targetPlantID := plantID
	if targetPlantID == "" {
		targetPlantID = os.Getenv(EnvPlantID)
	}

	// If continuous mode, set up signal handling
	if continuous > 0 {
		return runContinuous(client, targetPlantID, time.Duration(continuous)*time.Second)
	}

	// Single fetch
	return fetchAndPrint(client, targetPlantID, false)
}

func runContinuous(client *growatt.Client, targetPlantID string, interval time.Duration) error {
	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Fetch immediately on start
	if err := fetchAndPrint(client, targetPlantID, true); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}

	for {
		select {
		case <-sigChan:
			fmt.Fprintln(os.Stderr, "\nStopping...")
			return nil
		case <-ticker.C:
			if err := fetchAndPrint(client, targetPlantID, true); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
		}
	}
}

func fetchAndPrint(client *growatt.Client, targetPlantID string, includeTimestamp bool) error {
	ctx := context.Background()

	// Get plant list (includes current power)
	plants, err := client.ListPlants(ctx)
	if err != nil {
		return fmt.Errorf("fetching plants: %w", err)
	}

	if len(plants) == 0 {
		return fmt.Errorf("no plants found for this account")
	}

	// Find the target plant
	var plant *growatt.Plant
	if targetPlantID != "" {
		for i := range plants {
			if plants[i].PlantID.String() == targetPlantID {
				plant = &plants[i]
				break
			}
		}
		if plant == nil {
			return fmt.Errorf("plant %s not found", targetPlantID)
		}
	} else if len(plants) == 1 {
		plant = &plants[0]
	} else {
		fmt.Fprintln(os.Stderr, "Multiple plants found:")
		for _, p := range plants {
			fmt.Fprintf(os.Stderr, "  - %s (ID: %s)\n", p.PlantName, p.PlantID.String())
		}
		return fmt.Errorf("multiple plants found; specify --plant-id or set %s environment variable", EnvPlantID)
	}

	if jsonOutput {
		output := PowerOutput{
			PlantID:      plant.PlantID.String(),
			PlantName:    plant.PlantName,
			CurrentPower: plant.CurrentPower.Float64(),
			TodayEnergy:  plant.TodayEnergy.Float64(),
			TotalEnergy:  plant.TotalEnergy.Float64(),
			PeakPower:    plant.PeakPower.Float64(),
			Status:       plant.Status,
		}
		if includeTimestamp {
			output.Timestamp = time.Now().Format(time.RFC3339)
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(output)
	}

	// Human-readable output
	if includeTimestamp {
		fmt.Printf("%s  %.0f W\n", time.Now().Format("15:04:05"), plant.CurrentPower.Float64())
	} else {
		fmt.Printf("%.0f W\n", plant.CurrentPower.Float64())
	}
	return nil
}
