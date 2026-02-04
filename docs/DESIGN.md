# GoGrowatt Design Document

## Overview

This document describes the design of the GoGrowatt module, a Go library for accessing the Growatt OpenAPI v1, and an example CLI program demonstrating its usage.

## Architecture

```
gogrowatt/
├── pkg/
│   └── growatt/
│       ├── client.go       # HTTP client and authentication
│       ├── client_test.go  # Client tests
│       ├── types.go        # API response types
│       ├── plant.go        # Plant-related API calls
│       ├── plant_test.go   # Plant API tests
│       ├── device.go       # Device-related API calls
│       ├── device_test.go  # Device API tests
│       └── errors.go       # Error types and handling
├── cmd/
│   └── growatt-export/
│       └── main.go         # Example CLI program
├── internal/
│   └── stats/
│       ├── stats.go        # Statistical calculations
│       └── stats_test.go   # Stats tests
├── docs/
│   ├── api-access.md       # API reference
│   └── DESIGN.md           # This document
├── go.mod
└── go.sum
```

## Module Design: `pkg/growatt`

### Client

The `Client` struct manages authentication and HTTP requests to the Growatt API.

```go
type Client struct {
    baseURL    string
    token      string
    httpClient *http.Client
}

func NewClient(token string, opts ...ClientOption) *Client
func NewClientFromEnv(opts ...ClientOption) (*Client, error)
```

**Client Options:**
- `WithBaseURL(url string)` - Set custom API endpoint (for regional servers)
- `WithHTTPClient(client *http.Client)` - Use custom HTTP client
- `WithTimeout(d time.Duration)` - Set request timeout

### API Response Types

```go
// Base response structure
type Response[T any] struct {
    ErrorCode int    `json:"error_code"`
    ErrorMsg  string `json:"error_msg"`
    Data      T      `json:"data"`
}

// Plant types
type Plant struct {
    PlantID     string `json:"plant_id"`
    PlantName   string `json:"plant_name"`
    // ... additional fields
}

type PlantList struct {
    Plants []Plant `json:"plants"`
}

type PlantData struct {
    TodayEnergy  float64 `json:"today_energy"`
    TotalEnergy  float64 `json:"total_energy"`
    CurrentPower float64 `json:"current_power"`
}

// Power data (5-minute intervals)
type PowerDataPoint struct {
    Time  string  `json:"time"`  // HH:MM format
    Power float64 `json:"power"` // Watts
}

type PowerData struct {
    PlantID string           `json:"plant_id"`
    Date    string           `json:"date"`
    Powers  []PowerDataPoint `json:"powers"`
}

// Energy data (daily/monthly)
type EnergyDataPoint struct {
    Date   string  `json:"date"`
    Energy float64 `json:"energy"` // kWh
}

type EnergyData struct {
    PlantID string            `json:"plant_id"`
    Datas   []EnergyDataPoint `json:"datas"`
}
```

### Plant API Methods

```go
func (c *Client) ListPlants(ctx context.Context) ([]Plant, error)
func (c *Client) GetPlantDetails(ctx context.Context, plantID string) (*Plant, error)
func (c *Client) GetPlantData(ctx context.Context, plantID string) (*PlantData, error)
func (c *Client) GetPlantPower(ctx context.Context, plantID string, date time.Time) (*PowerData, error)
func (c *Client) GetPlantEnergy(ctx context.Context, plantID, startDate, endDate string, timeUnit TimeUnit) (*EnergyData, error)
```

### Device API Methods

```go
func (c *Client) ListDevices(ctx context.Context, plantID string) ([]Device, error)
func (c *Client) GetMINInverterDetails(ctx context.Context, serial string) (*MINInverterData, error)
```

### Error Handling

```go
type APIError struct {
    Code    int
    Message string
}

func (e *APIError) Error() string

// Common errors
var (
    ErrPermissionDenied = &APIError{Code: 10011, Message: "permission denied"}
    ErrPlantNotFound    = &APIError{Code: 10012, Message: "plant not found"}
)
```

## Internal Package: `internal/stats`

Statistical calculations for aggregating power data.

```go
type HourlyStats struct {
    Hour    int
    Samples int
    Min     float64
    Max     float64
    Sum     float64
    Mean    float64
    StdDev  float64
}

type DailyStats struct {
    Date   string
    Hours  []HourlyStats
}

type MultiDayStats struct {
    StartDate string
    EndDate   string
    ByHour    [24]AggregatedHourStats // Stats across all days for each hour
}

type AggregatedHourStats struct {
    Hour       int
    SampleDays int
    Min        float64
    Max        float64
    Average    float64
    Mean       float64
    StdDev     float64
}

func AggregateToHourly(data []PowerDataPoint) []HourlyStats
func AggregateDays(days []DailyStats) *MultiDayStats
func CalculateStdDev(values []float64, mean float64) float64
```

## CLI Program: `growatt-export`

### Usage

```bash
# Export today's data
growatt-export --plant-id=XXXXX today

# Export date range
growatt-export --plant-id=XXXXX --from=2025-01-01 --to=2025-01-31

# Export single day
growatt-export --plant-id=XXXXX --date=2025-02-01

# Specify output directory
growatt-export --plant-id=XXXXX --output=./data today
```

### Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--plant-id` | Plant ID (required) | - |
| `--from` | Start date (YYYY-MM-DD) | - |
| `--to` | End date (YYYY-MM-DD) | - |
| `--date` | Single date (YYYY-MM-DD) | - |
| `--output` | Output directory | `.` |
| `--token` | API token (overrides env) | `$GROWATT_API_KEY` |
| `--base-url` | API base URL | `https://openapi.growatt.com/v1/` |

### Output Files

**Raw CSV** (`power_YYYY-MM-DD.csv` or `power_YYYY-MM-DD_to_YYYY-MM-DD.csv`):
```csv
date,time,power_watts
2025-02-01,00:00,0
2025-02-01,00:05,0
2025-02-01,06:30,245.5
...
```

**Hourly Aggregated CSV** (`hourly_YYYY-MM-DD.csv`):
```csv
date,hour,min_watts,max_watts,avg_watts,samples
2025-02-01,0,0,0,0,12
2025-02-01,6,0,523.4,245.2,12
...
```

**Multi-day Statistics Markdown** (`stats_YYYY-MM-DD_to_YYYY-MM-DD.md`):

```markdown
# Power Production Statistics

**Period:** 2025-01-01 to 2025-01-31
**Days Analyzed:** 31

## Hourly Statistics (All Days Combined)

### Minimum Power by Hour
| Hour | Min (W) | Max (W) | Avg (W) |
|------|---------|---------|---------|
| 00   | 0       | 0       | 0       |
| 06   | 0       | 125     | 45.2    |
...

### Maximum Power by Hour
| Hour | Min (W) | Max (W) | Avg (W) |
|------|---------|---------|---------|
...

### Average Power by Hour
| Hour | Value (W) | Std Dev |
|------|-----------|---------|
| 06   | 245.3     | 82.1    |
...

### Statistical Summary
| Metric | Value |
|--------|-------|
| Peak Hour (avg) | 12:00 |
| Peak Power (avg) | 4523.2 W |
| Daily Average Production | 32.5 kWh |
| Total Production | 1007.5 kWh |
```

## Testing Strategy

### Unit Tests

1. **Client Tests** (`client_test.go`)
   - `TestNewClient` - Verify client creation with options
   - `TestNewClientFromEnv` - Verify env var loading
   - `TestClientRequest` - Mock HTTP responses

2. **Plant Tests** (`plant_test.go`)
   - `TestListPlants` - Parse plant list response
   - `TestGetPlantPower` - Parse power data response
   - `TestGetPlantEnergy` - Parse energy data response
   - `TestAPIErrors` - Handle error responses

3. **Stats Tests** (`stats_test.go`)
   - `TestAggregateToHourly` - Verify 5-min to hourly aggregation
   - `TestCalculateStdDev` - Verify standard deviation calculation
   - `TestAggregateDays` - Verify multi-day aggregation

### Integration Tests

Integration tests require a valid API token and are skipped by default:

```bash
GROWATT_API_KEY=xxx GROWATT_PLANT_ID=yyy go test -tags=integration ./...
```

### Test Fixtures

Mock responses stored in `testdata/` directory:
- `testdata/plant_list.json`
- `testdata/plant_power.json`
- `testdata/plant_energy.json`
- `testdata/error_permission_denied.json`

## Rate Limiting Considerations

The client implements:
- Configurable request delay between API calls (default: 3 seconds)
- Automatic retry with exponential backoff on rate limit errors
- Context cancellation support for long-running operations

```go
func (c *Client) SetRateLimit(delay time.Duration)
func (c *Client) GetPlantPowerRange(ctx context.Context, plantID string, from, to time.Time) ([]PowerData, error)
```

## Environment Variables

| Variable | Description |
|----------|-------------|
| `GROWATT_API_KEY` | API token for authentication |
| `GROWATT_BASE_URL` | Override default API endpoint |
| `GROWATT_PLANT_ID` | Default plant ID (for testing) |

## Dependencies

- `github.com/spf13/cobra` - CLI framework
- Standard library only for the core module
