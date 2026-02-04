# GoGrowatt

A Go library and CLI tool for accessing solar production data from Growatt inverters via the OpenAPI v1.

## Why Monitor Your Solar Production?

Your solar installation generates valuable data every five minutes. Understanding your production patterns helps you:

- **Optimize energy usage** by scheduling high-consumption tasks during peak production hours
- **Detect problems early** when production drops unexpectedly compared to historical patterns
- **Track ROI** by monitoring actual vs. projected energy generation
- **Plan battery storage** by understanding when you produce more than you consume
- **Feed data to home automation** systems for smarter energy management

The Growatt ShinePhone app shows basic stats, but getting raw data unlocks deeper analysis. GoGrowatt provides both a library for building your own tools and a CLI for quick data exports.

## Installation

```bash
# Clone and build
git clone https://github.com/gogrowatt/gogrowatt.git
cd gogrowatt
make build

# Or install directly
go install github.com/gogrowatt/cmd/growatt-export@latest
```

## Quick Start

Set your API token (obtained from https://openapi.growatt.com/):

```bash
export GROWATT_API_KEY="your-api-token"
```

If you have only one plant, that's all you need - the plant ID will be auto-detected.

For multiple plants, also set:

```bash
export GROWATT_PLANT_ID="your-plant-id"
```

To find your plant ID:

```bash
curl -H "token: $GROWATT_API_KEY" https://openapi.growatt.com/v1/plant/list
```

## CLI Usage

The `growatt-export` tool fetches 5-minute interval power data and generates CSV files with optional statistical analysis.

### Plant ID Resolution

The plant ID is resolved in this order:
1. `--plant-id` command line flag
2. `GROWATT_PLANT_ID` environment variable
3. Auto-detection (if you have exactly one plant)

If you have multiple plants and don't specify one, the tool will list them and exit.

### Export Today's Data

```bash
# Auto-detect plant (single plant accounts)
./bin/growatt-export today

# Or specify explicitly
./bin/growatt-export --plant-id=12345 today
```

Output:
```
Auto-detected plant: Home Solar (12345)
Fetching power data for plant 12345 from 2025-02-04 to 2025-02-04...
Wrote raw data to power_2025-02-04.csv
Wrote hourly data to hourly_2025-02-04.csv
```

### Export a Single Day

```bash
./bin/growatt-export --date=2025-01-15
```

### Export a Date Range

```bash
./bin/growatt-export --from=2025-01-01 --to=2025-01-31
```

When exporting multiple days, you get an additional markdown file with statistical analysis:

```
Wrote raw data to power_2025-01-01_to_2025-01-31.csv
Wrote hourly data to hourly_2025-01-01_to_2025-01-31.csv
Wrote statistics to stats_2025-01-01_to_2025-01-31.md
```

### Export to a Specific Directory

```bash
./bin/growatt-export --from=2025-01-01 --to=2025-01-07 --output=./solar-data
```

### Use a Different API Endpoint

For non-EU regions:

```bash
# North America
./bin/growatt-export --base-url=https://openapi-us.growatt.com/v1/ today

# Australia/NZ
./bin/growatt-export --base-url=https://openapi-au.growatt.com/v1/ today
```

### Multiple Plants

If you have multiple plants:

```bash
# Set via environment
export GROWATT_PLANT_ID=12345
./bin/growatt-export today

# Or specify on command line
./bin/growatt-export --plant-id=12345 today
```

Without specifying, you'll see:
```
No plant ID specified, checking available plants...

Multiple plants found:
  - Home Solar (ID: 12345)
  - Office Solar (ID: 12346)
Error: multiple plants found; specify --plant-id or set GROWATT_PLANT_ID environment variable
```

### Output Files

**Raw CSV** (`power_YYYY-MM-DD.csv`):
```csv
date,time,power_watts
2025-02-04,06:00,0.00
2025-02-04,06:05,45.20
2025-02-04,06:10,128.50
...
```

**Hourly CSV** (`hourly_YYYY-MM-DD.csv`):
```csv
date,hour,min_watts,max_watts,avg_watts,samples
2025-02-04,6,0.00,523.40,245.20,12
2025-02-04,7,534.20,1245.80,892.30,12
...
```

**Statistics Markdown** (multi-day exports):

Contains min/max/average/median/standard deviation by hour across all days, peak production analysis, and total energy estimates. Formatted for easy interpretation by humans or LLMs.

## Library Usage

The `pkg/growatt` package provides a clean API for accessing Growatt data in your Go programs.

### Basic Example

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/gogrowatt/pkg/growatt"
)

func main() {
    // Create client from environment variable
    client, err := growatt.NewClientFromEnv()
    if err != nil {
        log.Fatal(err)
    }

    ctx := context.Background()

    // List all plants
    plants, err := client.ListPlants(ctx)
    if err != nil {
        log.Fatal(err)
    }

    for _, p := range plants {
        fmt.Printf("Plant: %s (ID: %s) - Current: %.1f W, Today: %.1f kWh\n",
            p.PlantName, p.PlantID, p.CurrentPower.Float64(), p.TodayEnergy.Float64())
    }
}
```

### Fetch Power Data

```go
// Get today's 5-minute interval data
today := time.Now()
power, err := client.GetPlantPower(ctx, "12345", today)
if err != nil {
    log.Fatal(err)
}

for _, p := range power.Powers {
    fmt.Printf("%s: %.1f W\n", p.Time, p.Power)
}
```

### Fetch Multiple Days

```go
from, _ := time.Parse("2006-01-02", "2025-01-01")
to, _ := time.Parse("2006-01-02", "2025-01-07")

// Fetches each day with automatic rate limiting
data, err := client.GetPlantPowerRange(ctx, "12345", from, to)
if err != nil {
    log.Fatal(err)
}

for _, day := range data {
    fmt.Printf("Date: %s - %d readings\n", day.Date, len(day.Powers))
}
```

### Get Energy Totals

```go
// Daily totals for a month
energy, err := client.GetPlantEnergy(ctx, "12345",
    "2025-01-01", "2025-01-31", growatt.TimeUnitDay)
if err != nil {
    log.Fatal(err)
}

var total float64
for _, d := range energy.Datas {
    fmt.Printf("%s: %.1f kWh\n", d.Date, d.Energy)
    total += d.Energy
}
fmt.Printf("Total: %.1f kWh\n", total)
```

### Client Options

```go
// Custom configuration
client := growatt.NewClient("your-token",
    growatt.WithBaseURL("https://openapi-us.growatt.com/v1/"),
    growatt.WithTimeout(60*time.Second),
    growatt.WithRateLimit(5*time.Second),
)
```

### Statistical Analysis

The `internal/stats` package provides hourly aggregation:

```go
import "github.com/gogrowatt/internal/stats"

// Parse raw power data
parsed, _ := growatt.ParsePowerData(powerData)

// Aggregate to hourly stats
daily := stats.AggregateToHourly(parsed)

for hour := 0; hour < 24; hour++ {
    h := daily.Hours[hour]
    if h.Samples > 0 {
        fmt.Printf("%02d:00 - Avg: %.1f W, Max: %.1f W\n",
            hour, h.Mean, h.Max)
    }
}
```

### Error Handling

```go
plants, err := client.ListPlants(ctx)
if err != nil {
    if growatt.IsPermissionDenied(err) {
        log.Fatal("Invalid API token")
    }
    if growatt.IsPlantNotFound(err) {
        log.Fatal("Plant ID not found")
    }
    log.Fatal(err)
}
```

## Development

```bash
make help     # Show all targets
make build    # Build binary
make test     # Run tests
make cover    # Run tests with coverage
make lint     # Format and vet code
make clean    # Remove build artifacts
```

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `plant/list` | `ListPlants` | List all plants |
| `plant/data` | `GetPlantData` | Energy overview |
| `plant/power` | `GetPlantPower` | 5-minute intervals |
| `plant/energy` | `GetPlantEnergy` | Daily/monthly totals |
| `device/list` | `ListDevices` | List devices in plant |
| `device/tlx/tlx_data_info` | `GetMINInverterDetails` | MIN inverter details |

## Environment Variables

| Variable | Description |
|----------|-------------|
| `GROWATT_API_KEY` | API token (required) |
| `GROWATT_PLANT_ID` | Default plant ID (optional, auto-detected for single-plant accounts) |
| `GROWATT_BASE_URL` | Override API endpoint for regional servers |

## Rate Limits

The Growatt API has undocumented rate limits. This library defaults to 3 seconds between requests. For bulk exports, the client automatically paces requests. Polling more frequently than every 2 minutes is not recommended.

## License

MIT
