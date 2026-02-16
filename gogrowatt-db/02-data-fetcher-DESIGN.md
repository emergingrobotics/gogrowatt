# Data Fetcher Service - Implementation Design

## Document Purpose

This is the complete implementation blueprint for the `gogrowatt-fetcher` daemon. Every Go type,
function signature, SQL query, configuration option, and test case is specified here with the
precision needed to write code directly from this document. All column names, primary key orders,
and schema namespaces conform to the consistency analysis in `00-consistency-analysis.md` and the
canonical schema in `01-database-schema.md`.

---

## Table of Contents

1. [Critical: Raw History Data Access](#1-critical-raw-history-data-access)
2. [Package Structure](#2-package-structure)
3. [Configuration](#3-configuration)
4. [Main Entry Point](#4-main-entry-point)
5. [Database Layer](#5-database-layer)
6. [Fetcher Core](#6-fetcher-core)
7. [Adaptive Timer](#7-adaptive-timer)
8. [Upsert Writer](#8-upsert-writer)
9. [Metadata Tracker](#9-metadata-tracker)
10. [Backfill Manager](#10-backfill-manager)
11. [Gap Detection](#11-gap-detection)
12. [Health and Metrics](#12-health-and-metrics)
13. [Interfaces for Testability](#13-interfaces-for-testability)
14. [Error Handling and Retry Strategy](#14-error-handling-and-retry-strategy)
15. [Logging Strategy](#15-logging-strategy)
16. [Unit Test Strategy](#16-unit-test-strategy)
17. [Integration Test Strategy](#17-integration-test-strategy)
18. [Dockerfile](#18-dockerfile)
19. [Complete SQL Reference](#19-complete-sql-reference)

---

## 1. Critical: Raw History Data Access

### Problem

The existing `Client.GetMINInverterHistory()` in `pkg/growatt/device.go` converts
`MINHistoryDataPoint` into `PowerDataPoint`, discarding the per-string voltage and current
fields (`Vpv1`, `Vpv2`, `Ipv1`, `Ipv2`, `Vac1`, `Iac1`, `Ppv`). The fetcher needs all fields
to populate `growatt.power_readings` fully.

### Solution: New Method `GetMINInverterHistoryRaw`

Add a thin wrapper to `pkg/growatt/device.go` that returns the raw `MINHistoryResponse` without
conversion. This does NOT modify any existing method signatures.

```go
// pkg/growatt/device.go -- NEW METHOD (append to existing file)

// GetMINInverterHistoryRaw returns raw historical data for a MIN/TLX inverter
// without converting to PowerData. This preserves all per-string voltage/current fields.
// Note: Maximum date range is 1 day per call.
func (c *Client) GetMINInverterHistoryRaw(ctx context.Context, serial string, date time.Time, timezone string) (*MINHistoryResponse, string, error) {
	if timezone == "" {
		timezone = "US/Central"
	}

	dateStr := date.Format("2006-01-02")

	reqBody := MINHistoryRequest{
		DeviceSN:   serial,
		StartDate:  dateStr,
		EndDate:    dateStr,
		TimezoneID: timezone,
		Page:       1,
		PerPage:    100,
	}

	body, err := c.postForm(ctx, "device/tlx/tlx_data", reqBody.ToFormData())
	if err != nil {
		return nil, dateStr, err
	}

	histResp, err := parseResponse[MINHistoryResponse](body)
	if err != nil {
		return nil, dateStr, err
	}

	return histResp, dateStr, nil
}
```

The return value `dateStr` is the date string used for the request, needed by the caller for
timestamp parsing. The existing `GetMINInverterHistory` and `GetMINInverterHistoryRange` remain
unchanged.

---

## 2. Package Structure

```
gogrowatt/
├── cmd/
│   ├── gogrowatt-fetcher/
│   │   └── main.go                  # CLI entry point, signal handling
│   ├── growatt-export/
│   │   └── main.go                  # existing export tool (unchanged)
│   └── growatt-power/
│       └── main.go                  # existing power tool (unchanged)
├── internal/
│   ├── fetcher/
│   │   ├── config.go                # Configuration struct + loader
│   │   ├── config_test.go           # Config loading tests
│   │   ├── fetcher.go               # Fetch Orchestrator (main loop, state machine)
│   │   ├── fetcher_test.go          # Orchestrator unit tests
│   │   ├── timer.go                 # Adaptive Timer
│   │   ├── timer_test.go            # Timer algorithm tests
│   │   ├── parser.go                # MINHistoryDataPoint -> PowerReading conversion
│   │   ├── parser_test.go           # Timestamp parsing tests
│   │   ├── writer.go                # Upsert Writer (batch inserts)
│   │   ├── writer_test.go           # Writer tests with mock DB
│   │   ├── metadata.go              # collection_runs + collection_gaps tracking
│   │   ├── metadata_test.go         # Metadata tests
│   │   ├── backfill.go              # Startup backfill logic
│   │   ├── backfill_test.go         # Backfill tests
│   │   ├── gaps.go                  # Gap detection
│   │   ├── gaps_test.go             # Gap detection tests
│   │   ├── health.go                # HTTP health/metrics endpoint
│   │   └── health_test.go           # Health endpoint tests
│   ├── db/
│   │   ├── db.go                    # Connection pool setup
│   │   ├── db_test.go               # Pool tests
│   │   └── queries.go               # SQL query constants
│   └── stats/                       # existing stats package (unchanged)
│       └── ...
├── pkg/
│   └── growatt/                     # existing API client
│       ├── client.go                # unchanged
│       ├── device.go                # + GetMINInverterHistoryRaw (new method)
│       ├── errors.go                # unchanged
│       ├── plant.go                 # unchanged
│       └── types.go                 # unchanged
├── go.mod
├── go.sum
└── Dockerfile.fetcher
```

### Dependency Graph

```
cmd/gogrowatt-fetcher/main.go
  ├── internal/fetcher   (config, orchestrator, timer, writer, metadata, backfill, gaps, health)
  ├── internal/db        (pool setup, query constants)
  └── pkg/growatt        (API client -- existing)

internal/fetcher
  ├── internal/db
  └── pkg/growatt

internal/db
  └── github.com/jackc/pgx/v5/pgxpool
```

### New Dependencies (go.mod additions)

```
require (
    github.com/jackc/pgx/v5 v5.7.2
    github.com/spf13/cobra v1.8.0  // already present
)
```

---

## 3. Configuration

### `internal/fetcher/config.go`

```go
package fetcher

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration for the fetcher daemon.
type Config struct {
	// Growatt API
	APIKey     string // GROWATT_API_KEY (required)
	BaseURL    string // GROWATT_BASE_URL (default: https://openapi.growatt.com/v1/)
	DeviceSN   string // GROWATT_DEVICE_SN (required or auto-detected)
	PlantID    string // GROWATT_PLANT_ID (optional, for auto-detection)
	Timezone   string // GROWATT_TIMEZONE (default: US/Central)

	// Database
	DatabaseURL string // DATABASE_URL (required)

	// Polling
	FetchInterval time.Duration // FETCH_INTERVAL (default: 5m)
	FetchOverlap  time.Duration // FETCH_OVERLAP (default: 1h)
	BackfillDays  int           // BACKFILL_DAYS (default: 7, max: 7)

	// HTTP
	HealthPort int // HEALTH_PORT (default: 8080)

	// Logging
	LogLevel string // LOG_LEVEL (default: info)
}

// DefaultConfig returns a Config with all default values populated.
func DefaultConfig() Config {
	return Config{
		BaseURL:       "https://openapi.growatt.com/v1/",
		Timezone:      "US/Central",
		FetchInterval: 5 * time.Minute,
		FetchOverlap:  1 * time.Hour,
		BackfillDays:  7,
		HealthPort:    8080,
		LogLevel:      "info",
	}
}

// LoadConfigFromEnv populates a Config from environment variables.
// CLI flags (applied separately) take precedence over env vars.
func LoadConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	// Required
	cfg.APIKey = os.Getenv("GROWATT_API_KEY")
	if cfg.APIKey == "" {
		return cfg, fmt.Errorf("GROWATT_API_KEY is required")
	}

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}

	// Optional overrides
	if v := os.Getenv("GROWATT_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("GROWATT_DEVICE_SN"); v != "" {
		cfg.DeviceSN = v
	}
	if v := os.Getenv("GROWATT_PLANT_ID"); v != "" {
		cfg.PlantID = v
	}
	if v := os.Getenv("GROWATT_TIMEZONE"); v != "" {
		cfg.Timezone = v
	}
	if v := os.Getenv("FETCH_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid FETCH_INTERVAL %q: %w", v, err)
		}
		cfg.FetchInterval = d
	}
	if v := os.Getenv("FETCH_OVERLAP"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid FETCH_OVERLAP %q: %w", v, err)
		}
		cfg.FetchOverlap = d
	}
	if v := os.Getenv("BACKFILL_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid BACKFILL_DAYS %q: %w", v, err)
		}
		if n > 7 {
			n = 7 // API hard limit
		}
		cfg.BackfillDays = n
	}
	if v := os.Getenv("HEALTH_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid HEALTH_PORT %q: %w", v, err)
		}
		cfg.HealthPort = n
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}

	return cfg, nil
}

// Validate checks that all required fields are set and values are sane.
func (c Config) Validate() error {
	if c.APIKey == "" {
		return fmt.Errorf("API key is required")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("database URL is required")
	}
	if c.FetchInterval < 1*time.Minute {
		return fmt.Errorf("fetch interval must be at least 1 minute")
	}
	if c.BackfillDays < 0 || c.BackfillDays > 7 {
		return fmt.Errorf("backfill days must be 0-7")
	}
	if c.HealthPort < 1 || c.HealthPort > 65535 {
		return fmt.Errorf("health port must be 1-65535")
	}
	return nil
}
```

### Environment Variable Summary

| Variable | Required | Default | Description |
|---|---|---|---|
| `GROWATT_API_KEY` | Yes | -- | API token for `token` header |
| `GROWATT_DEVICE_SN` | Yes* | auto-detect | Inverter serial number |
| `GROWATT_PLANT_ID` | No | auto-detect | Plant ID (needed only for auto-detection) |
| `GROWATT_TIMEZONE` | No | `US/Central` | Timezone ID for API requests |
| `GROWATT_BASE_URL` | No | `https://openapi.growatt.com/v1/` | API base URL |
| `DATABASE_URL` | Yes | -- | PostgreSQL DSN |
| `FETCH_INTERVAL` | No | `5m` | Default polling interval |
| `FETCH_OVERLAP` | No | `1h` | Overlap window for gap filling |
| `BACKFILL_DAYS` | No | `7` | Max startup backfill (capped at 7) |
| `HEALTH_PORT` | No | `8080` | Health/metrics HTTP port |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |

### CLI Flags

```
gogrowatt-fetcher \
  --device-sn=ABC123 \
  --plant-id=12345 \
  --timezone=US/Central \
  --db=postgres://user:pass@host:5432/growatt \
  --interval=5m \
  --overlap=1h \
  --backfill-days=7 \
  --health-port=8080 \
  --log-level=info
```

CLI flags override environment variables. Environment variables override defaults.

---

## 4. Main Entry Point

### `cmd/gogrowatt-fetcher/main.go`

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gogrowatt/internal/db"
	"github.com/gogrowatt/internal/fetcher"
	"github.com/gogrowatt/pkg/growatt"
	"github.com/spf13/cobra"
)

var (
	flagDeviceSN    string
	flagPlantID     string
	flagTimezone    string
	flagDatabaseURL string
	flagInterval    string
	flagOverlap     string
	flagBackfill    int
	flagHealthPort  int
	flagLogLevel    string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "gogrowatt-fetcher",
		Short: "Continuously fetch Growatt solar data into PostgreSQL",
		Long: `A long-running daemon that polls the Growatt OpenAPI for 5-minute
interval power data and writes it to a PostgreSQL/TimescaleDB database.

Performs automatic backfill on startup, adaptive polling, gap detection,
and upsert-based conflict resolution (latest fetch always wins).`,
		RunE:         run,
		SilenceUsage: true,
	}

	rootCmd.Flags().StringVar(&flagDeviceSN, "device-sn", "", "Device serial number (overrides GROWATT_DEVICE_SN)")
	rootCmd.Flags().StringVar(&flagPlantID, "plant-id", "", "Plant ID (overrides GROWATT_PLANT_ID)")
	rootCmd.Flags().StringVar(&flagTimezone, "timezone", "", "Timezone (overrides GROWATT_TIMEZONE)")
	rootCmd.Flags().StringVar(&flagDatabaseURL, "db", "", "Database URL (overrides DATABASE_URL)")
	rootCmd.Flags().StringVar(&flagInterval, "interval", "", "Fetch interval e.g. 5m (overrides FETCH_INTERVAL)")
	rootCmd.Flags().StringVar(&flagOverlap, "overlap", "", "Overlap window e.g. 1h (overrides FETCH_OVERLAP)")
	rootCmd.Flags().IntVar(&flagBackfill, "backfill-days", -1, "Backfill days 0-7 (overrides BACKFILL_DAYS)")
	rootCmd.Flags().IntVar(&flagHealthPort, "health-port", -1, "Health endpoint port (overrides HEALTH_PORT)")
	rootCmd.Flags().StringVar(&flagLogLevel, "log-level", "", "Log level (overrides LOG_LEVEL)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// ---- 1. Load configuration ----
	cfg, err := fetcher.LoadConfigFromEnv()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Apply CLI flag overrides
	applyFlagOverrides(&cfg)

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// ---- 2. Set up structured logging ----
	logLevel := parseLogLevel(cfg.LogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	logger.Info("starting gogrowatt-fetcher",
		"device_sn", cfg.DeviceSN,
		"timezone", cfg.Timezone,
		"interval", cfg.FetchInterval.String(),
		"backfill_days", cfg.BackfillDays,
	)

	// ---- 3. Create context with signal handling ----
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ---- 4. Open database connection pool ----
	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()

	logger.Info("database connection established")

	// ---- 5. Create Growatt API client ----
	var clientOpts []growatt.ClientOption
	if cfg.BaseURL != "" && cfg.BaseURL != growatt.DefaultBaseURL {
		clientOpts = append(clientOpts, growatt.WithBaseURL(cfg.BaseURL))
	}
	client := growatt.NewClient(cfg.APIKey, clientOpts...)

	// ---- 6. Auto-detect device SN if not configured ----
	if cfg.DeviceSN == "" {
		sn, err := autoDetectDevice(ctx, client, cfg.PlantID, logger)
		if err != nil {
			return fmt.Errorf("auto-detecting device: %w", err)
		}
		cfg.DeviceSN = sn
		logger.Info("auto-detected device", "device_sn", cfg.DeviceSN)
	}

	// ---- 7. Build and run the fetcher ----
	f := fetcher.New(cfg, client, pool, logger)

	// Start health endpoint in background
	healthServer := fetcher.StartHealthServer(cfg.HealthPort, f, logger)
	defer healthServer.Close()

	logger.Info("health endpoint started", "port", cfg.HealthPort)

	// Run blocks until context is cancelled
	if err := f.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("fetcher error: %w", err)
	}

	logger.Info("gogrowatt-fetcher stopped gracefully")
	return nil
}

func autoDetectDevice(ctx context.Context, client *growatt.Client, plantID string, logger *slog.Logger) (string, error) {
	// Resolve plant ID first
	if plantID == "" {
		logger.Info("no plant ID configured, listing plants...")
		plants, err := client.ListPlants(ctx)
		if err != nil {
			return "", fmt.Errorf("listing plants: %w", err)
		}
		if len(plants) == 0 {
			return "", fmt.Errorf("no plants found on this account")
		}
		if len(plants) > 1 {
			return "", fmt.Errorf("multiple plants found (%d); set GROWATT_PLANT_ID", len(plants))
		}
		plantID = plants[0].PlantID.String()
		logger.Info("auto-detected plant", "plant_id", plantID, "plant_name", plants[0].PlantName)
	}

	// List devices for the plant
	logger.Info("listing devices for plant", "plant_id", plantID)
	devices, err := client.ListDevices(ctx, plantID)
	if err != nil {
		return "", fmt.Errorf("listing devices: %w", err)
	}
	if len(devices) == 0 {
		return "", fmt.Errorf("no devices found for plant %s", plantID)
	}
	if len(devices) > 1 {
		return "", fmt.Errorf("multiple devices found (%d); set GROWATT_DEVICE_SN", len(devices))
	}

	return devices[0].DeviceSN.String(), nil
}

func applyFlagOverrides(cfg *fetcher.Config) {
	if flagDeviceSN != "" {
		cfg.DeviceSN = flagDeviceSN
	}
	if flagPlantID != "" {
		cfg.PlantID = flagPlantID
	}
	if flagTimezone != "" {
		cfg.Timezone = flagTimezone
	}
	if flagDatabaseURL != "" {
		cfg.DatabaseURL = flagDatabaseURL
	}
	if flagInterval != "" {
		// Error ignored here; Validate() will catch invalid values
		if d, err := time.ParseDuration(flagInterval); err == nil {
			cfg.FetchInterval = d
		}
	}
	if flagOverlap != "" {
		if d, err := time.ParseDuration(flagOverlap); err == nil {
			cfg.FetchOverlap = d
		}
	}
	if flagBackfill >= 0 {
		cfg.BackfillDays = flagBackfill
	}
	if flagHealthPort >= 0 {
		cfg.HealthPort = flagHealthPort
	}
	if flagLogLevel != "" {
		cfg.LogLevel = flagLogLevel
	}
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
```

### Signal Handling and Graceful Shutdown

The shutdown sequence:

1. OS sends `SIGINT` or `SIGTERM`.
2. `signal.NotifyContext` cancels `ctx`.
3. `f.Run(ctx)` detects `ctx.Done()` and exits the polling loop.
4. If a fetch/upsert cycle is in progress, it completes before returning (the current transaction
   commits or rolls back cleanly).
5. `defer healthServer.Close()` shuts down the HTTP server.
6. `defer pool.Close()` closes all database connections.
7. `main()` returns, process exits with code 0.

---

## 5. Database Layer

### `internal/db/db.go`

```go
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates a new pgxpool connection pool configured for the fetcher.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}

	// Connection pool settings tuned for a single-device fetcher daemon
	config.MaxConns = 5                       // main loop + health check + headroom
	config.MinConns = 1                       // keep at least one warm connection
	config.MaxConnLifetime = 30 * time.Minute // recycle connections periodically
	config.MaxConnIdleTime = 5 * time.Minute  // release idle connections promptly
	config.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	// Verify connectivity
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return pool, nil
}
```

### `internal/db/queries.go`

All SQL queries used by the fetcher, centralized for maintainability. Every query uses the
`growatt.` schema prefix and the canonical column names from `01-database-schema.md`.

```go
package db

// ---- power_readings ----

// UpsertPowerReadingSQL inserts or updates a single power reading row.
// Parameters: $1=ts, $2=device_sn, $3=pac_w, $4=ppv_w, $5=vpv1_v, $6=vpv2_v,
//             $7=ipv1_a, $8=ipv2_a, $9=vac1_v, $10=iac1_a
// Note: fetched_at is set to now() automatically.
// PK order: (ts, device_sn) per schema.
const UpsertPowerReadingSQL = `
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (ts, device_sn) DO UPDATE SET
    pac_w      = EXCLUDED.pac_w,
    ppv_w      = EXCLUDED.ppv_w,
    vpv1_v     = EXCLUDED.vpv1_v,
    vpv2_v     = EXCLUDED.vpv2_v,
    ipv1_a     = EXCLUDED.ipv1_a,
    ipv2_a     = EXCLUDED.ipv2_a,
    vac1_v     = EXCLUDED.vac1_v,
    iac1_a     = EXCLUDED.iac1_a,
    fetched_at = EXCLUDED.fetched_at`

// UpsertPowerReadingBatchSQL generates a multi-row INSERT for N rows.
// This is constructed dynamically in the writer (see writer.go).
// Shown here for reference only -- the writer builds this at runtime.
//
// INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
// VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now()),
//        ($11, $12, $13, $14, $15, $16, $17, $18, $19, $20, now()),
//        ...
// ON CONFLICT (ts, device_sn) DO UPDATE SET
//     pac_w = EXCLUDED.pac_w,
//     ppv_w = EXCLUDED.ppv_w,
//     vpv1_v = EXCLUDED.vpv1_v,
//     vpv2_v = EXCLUDED.vpv2_v,
//     ipv1_a = EXCLUDED.ipv1_a,
//     ipv2_a = EXCLUDED.ipv2_a,
//     vac1_v = EXCLUDED.vac1_v,
//     iac1_a = EXCLUDED.iac1_a,
//     fetched_at = EXCLUDED.fetched_at

// LastReadingTimeSQL returns the most recent timestamp for a device.
// Used by backfill logic to determine where to start.
const LastReadingTimeSQL = `
SELECT MAX(ts)
FROM growatt.power_readings
WHERE device_sn = $1`

// ReadingsInRangeSQL returns all timestamps for a device in a time range.
// Used by gap detection.
const ReadingsInRangeSQL = `
SELECT ts
FROM growatt.power_readings
WHERE device_sn = $1
  AND ts >= $2
  AND ts < $3
ORDER BY ts`

// ---- collection_runs ----

// InsertCollectionRunSQL starts a new collection run record.
// Returns the generated id.
const InsertCollectionRunSQL = `
INSERT INTO growatt.collection_runs (source_type, source_id, query_date_start, query_date_end, started_at, status)
VALUES ($1, $2, $3, $4, now(), 'running')
RETURNING id`

// UpdateCollectionRunSuccessSQL marks a run as successful.
const UpdateCollectionRunSuccessSQL = `
UPDATE growatt.collection_runs
SET finished_at = now(),
    points_collected = $2,
    status = 'success'
WHERE id = $1`

// UpdateCollectionRunErrorSQL marks a run as failed.
const UpdateCollectionRunErrorSQL = `
UPDATE growatt.collection_runs
SET finished_at = now(),
    status = 'error',
    error_message = $2
WHERE id = $1`

// UpdateCollectionRunPartialSQL marks a run as partially successful.
const UpdateCollectionRunPartialSQL = `
UPDATE growatt.collection_runs
SET finished_at = now(),
    points_collected = $2,
    status = 'partial',
    error_message = $3
WHERE id = $1`

// ---- collection_gaps ----

// UpsertCollectionGapSQL inserts or updates a gap record.
// Uses the UNIQUE constraint (device_sn, gap_date).
const UpsertCollectionGapSQL = `
INSERT INTO growatt.collection_gaps (device_sn, gap_date, expected_start, expected_end, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (device_sn, gap_date) DO UPDATE SET
    expected_start = EXCLUDED.expected_start,
    expected_end   = EXCLUDED.expected_end,
    reason         = EXCLUDED.reason,
    detected_at    = now()`

// ResolveCollectionGapSQL marks a gap as resolved.
const ResolveCollectionGapSQL = `
UPDATE growatt.collection_gaps
SET resolved_at = now()
WHERE device_sn = $1
  AND gap_date = $2
  AND resolved_at IS NULL`

// UnresolvedGapsSQL returns all unresolved gaps for a device.
const UnresolvedGapsSQL = `
SELECT gap_date, expected_start, expected_end, reason
FROM growatt.collection_gaps
WHERE device_sn = $1
  AND resolved_at IS NULL
ORDER BY gap_date`
```

---

## 6. Fetcher Core

### State Machine

The fetcher runs through these states:

```
[Init] -> [Backfill] -> [SteadyState] -> [WaitForTick] -> [Fetching] ->
    [ProcessData] -> [Upserting] -> [AdjustTimer] -> [WaitForTick] -> ...
                                                                      |
Errors:  [Fetching] -> [RetryWait] -> [Fetching]                     |
         [Fetching] -> [RateLimitWait] -> [Fetching]                  |
         [WaitForTick|Fetching] -> [Shutdown] -------------------------+
```

### `internal/fetcher/fetcher.go`

```go
package fetcher

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/gogrowatt/pkg/growatt"
)

// Fetcher is the main fetch orchestrator.
type Fetcher struct {
	cfg    Config
	client GrowattClient   // interface, see section 13
	pool   DBPool          // interface, see section 13
	logger *slog.Logger
	timer  *AdaptiveTimer
	writer *Writer
	meta   *MetadataTracker

	// Health state (protected by mutex)
	mu              sync.RWMutex
	lastFetchTime   time.Time
	lastFetchStatus string
	lastFetchPoints int
	totalFetches    int64
	totalErrors     int64
	totalPoints     int64
	state           string
}

// New creates a new Fetcher with all sub-components.
func New(cfg Config, client GrowattClient, pool DBPool, logger *slog.Logger) *Fetcher {
	return &Fetcher{
		cfg:    cfg,
		client: client,
		pool:   pool,
		logger: logger,
		timer:  NewAdaptiveTimer(cfg.FetchInterval, logger),
		writer: NewWriter(pool, logger),
		meta:   NewMetadataTracker(pool, logger),
		state:  "init",
	}
}

// Run is the main entry point. It blocks until ctx is cancelled.
func (f *Fetcher) Run(ctx context.Context) error {
	f.setState("backfill")

	// ---- Phase 1: Startup backfill ----
	f.logger.Info("starting backfill check",
		"device_sn", f.cfg.DeviceSN,
		"max_days", f.cfg.BackfillDays,
	)

	backfiller := NewBackfillManager(f.cfg, f.client, f.writer, f.meta, f.pool, f.logger)
	if err := backfiller.Run(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Log but don't fail -- enter steady state anyway
		f.logger.Error("backfill completed with errors", "error", err)
	}

	// ---- Phase 2: Steady-state polling ----
	f.setState("steady_state")
	f.logger.Info("entering steady-state polling",
		"device_sn", f.cfg.DeviceSN,
		"initial_interval", f.cfg.FetchInterval.String(),
	)

	return f.pollLoop(ctx)
}

func (f *Fetcher) pollLoop(ctx context.Context) error {
	// Fire immediately on first iteration, then use adaptive timer
	nextFetch := time.Now()

	for {
		// Wait for next fetch time or cancellation
		f.setState("wait_for_tick")
		waitDuration := time.Until(nextFetch)
		if waitDuration > 0 {
			f.logger.Debug("waiting for next fetch",
				"next_fetch", nextFetch.Format(time.RFC3339),
				"wait", waitDuration.String(),
			)
			select {
			case <-ctx.Done():
				f.setState("shutdown")
				return ctx.Err()
			case <-time.After(waitDuration):
			}
		}

		// Perform fetch cycle
		f.setState("fetching")
		result, err := f.fetchCycle(ctx)
		if err != nil {
			if ctx.Err() != nil {
				f.setState("shutdown")
				return ctx.Err()
			}
			f.recordError()
			f.logger.Error("fetch cycle failed", "error", err)
			nextFetch = time.Now().Add(f.cfg.FetchInterval)
			continue
		}

		// Update health state
		f.recordSuccess(result.PointsWritten)

		// Compute next fetch time from adaptive timer
		nextFetch = f.timer.NextFetchTime(result.LatestTimestamp, time.Now())
		f.logger.Info("fetch cycle complete",
			"points_written", result.PointsWritten,
			"latest_ts", result.LatestTimestamp.Format(time.RFC3339),
			"next_fetch", nextFetch.Format(time.RFC3339),
		)
	}
}

// FetchCycleResult holds the outcome of a single fetch cycle.
type FetchCycleResult struct {
	PointsWritten   int
	LatestTimestamp time.Time
	DatesQueried   []string
}

func (f *Fetcher) fetchCycle(ctx context.Context) (*FetchCycleResult, error) {
	now := time.Now()
	loc, err := time.LoadLocation(f.cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("loading timezone %q: %w", f.cfg.Timezone, err)
	}

	localNow := now.In(loc)

	// Determine target dates: today, and yesterday if near midnight
	dates := []time.Time{localNow}
	minutesSinceMidnight := localNow.Hour()*60 + localNow.Minute()
	minutesUntilMidnight := 24*60 - minutesSinceMidnight

	if minutesUntilMidnight <= 65 || minutesSinceMidnight <= 65 {
		yesterday := localNow.AddDate(0, 0, -1)
		dates = append(dates, yesterday)
	}

	// Start collection run tracking
	dateStrs := make([]string, len(dates))
	for i, d := range dates {
		dateStrs[i] = d.Format("2006-01-02")
	}

	runID, err := f.meta.StartRun(ctx, "device_history", f.cfg.DeviceSN, dateStrs[0], dateStrs[len(dateStrs)-1])
	if err != nil {
		f.logger.Warn("failed to start collection run tracking", "error", err)
		// Continue anyway -- tracking failure should not block data collection
	}

	// Fetch data for each target date
	var allReadings []PowerReading
	var latestTS time.Time

	for _, date := range dates {
		readings, err := f.fetchAndParse(ctx, date, loc)
		if err != nil {
			// Apply retry strategy
			readings, err = f.fetchWithRetry(ctx, date, loc)
			if err != nil {
				if runID > 0 {
					_ = f.meta.FinishRunError(ctx, runID, err.Error())
				}
				return nil, fmt.Errorf("fetching %s: %w", date.Format("2006-01-02"), err)
			}
		}

		allReadings = append(allReadings, readings...)

		for _, r := range readings {
			if r.TS.After(latestTS) {
				latestTS = r.TS
			}
		}
	}

	if len(allReadings) == 0 {
		f.logger.Info("no data points returned", "dates", dateStrs)
		if runID > 0 {
			_ = f.meta.FinishRunSuccess(ctx, runID, 0)
		}
		return &FetchCycleResult{
			PointsWritten:  0,
			LatestTimestamp: time.Time{},
			DatesQueried:   dateStrs,
		}, nil
	}

	// Upsert all readings
	f.setState("upserting")
	written, err := f.writer.UpsertBatch(ctx, allReadings)
	if err != nil {
		if runID > 0 {
			_ = f.meta.FinishRunError(ctx, runID, err.Error())
		}
		return nil, fmt.Errorf("upserting readings: %w", err)
	}

	// Track success
	if runID > 0 {
		_ = f.meta.FinishRunSuccess(ctx, runID, written)
	}

	// Run gap detection (non-blocking)
	f.setState("gap_detection")
	for _, date := range dates {
		if err := DetectGaps(ctx, f.pool, f.cfg.DeviceSN, date, f.logger); err != nil {
			f.logger.Warn("gap detection failed", "date", date.Format("2006-01-02"), "error", err)
		}
	}

	return &FetchCycleResult{
		PointsWritten:  written,
		LatestTimestamp: latestTS,
		DatesQueried:   dateStrs,
	}, nil
}

func (f *Fetcher) fetchAndParse(ctx context.Context, date time.Time, loc *time.Location) ([]PowerReading, error) {
	histResp, dateStr, err := f.client.GetMINInverterHistoryRaw(ctx, f.cfg.DeviceSN, date, f.cfg.Timezone)
	if err != nil {
		return nil, err
	}

	readings := ParseMINHistoryData(histResp.Datas, dateStr, f.cfg.DeviceSN, loc)
	return readings, nil
}

func (f *Fetcher) fetchWithRetry(ctx context.Context, date time.Time, loc *time.Location) ([]PowerReading, error) {
	const maxRetries = 5
	const maxRateLimitRetries = 3

	rateLimitRetries := 0
	backoff := 5 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		readings, err := f.fetchAndParse(ctx, date, loc)
		if err == nil {
			return readings, nil
		}

		// Classify error
		if growatt.IsRateLimited(err) {
			rateLimitRetries++
			if rateLimitRetries > maxRateLimitRetries {
				return nil, fmt.Errorf("rate limited %d times, giving up: %w", rateLimitRetries, err)
			}
			f.logger.Warn("rate limited, waiting 30s",
				"attempt", attempt,
				"rate_limit_retry", rateLimitRetries,
			)
			f.setState("rate_limit_wait")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(30 * time.Second):
			}
			continue
		}

		if growatt.IsPermissionDenied(err) {
			f.logger.Error("permission denied -- check API token", "error", err)
			return nil, err // No retry for auth errors
		}

		// Network/timeout/unknown error: exponential backoff
		f.logger.Warn("transient error, retrying",
			"attempt", attempt,
			"backoff", backoff.String(),
			"error", err,
		)
		f.setState("retry_wait")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
	}

	return nil, fmt.Errorf("exhausted %d retries", maxRetries)
}

// ---- Health state accessors ----

func (f *Fetcher) setState(s string) {
	f.mu.Lock()
	f.state = s
	f.mu.Unlock()
}

func (f *Fetcher) recordSuccess(points int) {
	f.mu.Lock()
	f.lastFetchTime = time.Now()
	f.lastFetchStatus = "success"
	f.lastFetchPoints = points
	f.totalFetches++
	f.totalPoints += int64(points)
	f.mu.Unlock()
}

func (f *Fetcher) recordError() {
	f.mu.Lock()
	f.lastFetchTime = time.Now()
	f.lastFetchStatus = "error"
	f.totalErrors++
	f.mu.Unlock()
}

// HealthStatus returns the current health status for the /healthz endpoint.
func (f *Fetcher) HealthStatus() HealthData {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return HealthData{
		State:           f.state,
		LastFetchTime:   f.lastFetchTime,
		LastFetchStatus: f.lastFetchStatus,
		LastFetchPoints: f.lastFetchPoints,
		TotalFetches:    f.totalFetches,
		TotalErrors:     f.totalErrors,
		TotalPoints:     f.totalPoints,
		DeviceSN:        f.cfg.DeviceSN,
	}
}
```

---

## 7. Adaptive Timer

### `internal/fetcher/timer.go`

```go
package fetcher

import (
	"log/slog"
	"time"
)

const (
	// Growatt reports data every 5 minutes
	dataInterval = 5 * time.Minute
	// Buffer after expected data availability before fetching
	fetchBuffer = 1 * time.Minute
	// Maximum wait between fetches (safety cap)
	maxWait = 10 * time.Minute
	// Minimum wait between fetches (prevent spinning)
	minWait = 1 * time.Minute
)

// AdaptiveTimer computes the next fetch time based on the latest data point.
type AdaptiveTimer struct {
	defaultInterval time.Duration
	logger          *slog.Logger
}

// NewAdaptiveTimer creates a new AdaptiveTimer.
func NewAdaptiveTimer(defaultInterval time.Duration, logger *slog.Logger) *AdaptiveTimer {
	return &AdaptiveTimer{
		defaultInterval: defaultInterval,
		logger:          logger,
	}
}

// NextFetchTime computes when to fetch next based on the latest data timestamp.
//
// Algorithm:
//   - If no data: now + defaultInterval (usually 5 min)
//   - Otherwise: latestTS + 5min (interval) + 1min (buffer)
//   - Clamped to [now + minWait, now + maxWait]
func (t *AdaptiveTimer) NextFetchTime(latestTS time.Time, now time.Time) time.Time {
	if latestTS.IsZero() {
		next := now.Add(t.defaultInterval)
		t.logger.Debug("no data, using default interval",
			"next_fetch", next.Format(time.RFC3339),
		)
		return next
	}

	// The next data point should appear at latestTS + 5 min.
	// We add a 1 minute buffer to give the API time to make it available.
	nextFetch := latestTS.Add(dataInterval + fetchBuffer)

	// Enforce minimum wait
	earliest := now.Add(minWait)
	if nextFetch.Before(earliest) {
		t.logger.Debug("data is stale, using minimum wait",
			"latest_ts", latestTS.Format(time.RFC3339),
			"computed", nextFetch.Format(time.RFC3339),
			"clamped_to", earliest.Format(time.RFC3339),
		)
		nextFetch = earliest
	}

	// Enforce maximum wait
	latest := now.Add(maxWait)
	if nextFetch.After(latest) {
		t.logger.Debug("computed time too far out, capping",
			"latest_ts", latestTS.Format(time.RFC3339),
			"computed", nextFetch.Format(time.RFC3339),
			"clamped_to", latest.Format(time.RFC3339),
		)
		nextFetch = latest
	}

	return nextFetch
}
```

### Timing Examples

| Scenario | `latestTS` | `now` | Computed | Final |
|---|---|---|---|---|
| Normal | 12:05 | 12:06 | 12:11 | 12:11 |
| Stale data | 11:00 | 12:00 | 11:06 | 12:01 (minWait) |
| No data | (zero) | 12:00 | -- | 12:05 (default) |
| Far future | 12:30 | 12:10 | 12:36 | 12:20 (maxWait) |

---

## 8. Upsert Writer

### `internal/fetcher/writer.go`

```go
package fetcher

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gogrowatt/internal/db"
)

const batchSize = 100 // matches API page size

// PowerReading is the internal representation of a single data point
// ready for database insertion.
type PowerReading struct {
	TS       time.Time // UTC timestamp
	DeviceSN string
	PacW     float64 // AC output power (watts)
	PpvW     float64 // total PV input power (watts)
	Vpv1V    float64 // PV string 1 voltage (V)
	Vpv2V    float64 // PV string 2 voltage (V)
	Ipv1A    float64 // PV string 1 current (A)
	Ipv2A    float64 // PV string 2 current (A)
	Vac1V    float64 // grid AC voltage (V)
	Iac1A    float64 // grid AC current (A)
}

// Writer handles batch upserting of PowerReading rows into PostgreSQL.
type Writer struct {
	pool   DBPool
	logger *slog.Logger
}

// NewWriter creates a new Writer.
func NewWriter(pool DBPool, logger *slog.Logger) *Writer {
	return &Writer{pool: pool, logger: logger}
}

// UpsertBatch writes a slice of PowerReading rows in batches of 100.
// Returns the total number of rows affected (inserted + updated).
func (w *Writer) UpsertBatch(ctx context.Context, readings []PowerReading) (int, error) {
	if len(readings) == 0 {
		return 0, nil
	}

	totalAffected := 0

	// Process in batches
	for i := 0; i < len(readings); i += batchSize {
		end := i + batchSize
		if end > len(readings) {
			end = len(readings)
		}
		batch := readings[i:end]

		affected, err := w.upsertBatchChunk(ctx, batch)
		if err != nil {
			return totalAffected, fmt.Errorf("upserting batch chunk %d-%d: %w", i, end, err)
		}
		totalAffected += affected

		w.logger.Debug("upserted batch chunk",
			"chunk_start", i,
			"chunk_end", end,
			"affected", affected,
		)
	}

	return totalAffected, nil
}

// upsertBatchChunk inserts a single batch (up to 100 rows) in one statement.
func (w *Writer) upsertBatchChunk(ctx context.Context, batch []PowerReading) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}

	// Build the multi-row VALUES clause
	// Each row has 10 parameters: ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a
	const colsPerRow = 10
	valuesClauses := make([]string, 0, len(batch))
	args := make([]any, 0, len(batch)*colsPerRow)

	for i, r := range batch {
		offset := i * colsPerRow
		valuesClauses = append(valuesClauses, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, now())",
			offset+1, offset+2, offset+3, offset+4, offset+5,
			offset+6, offset+7, offset+8, offset+9, offset+10,
		))
		args = append(args,
			r.TS,       // $1
			r.DeviceSN, // $2
			r.PacW,     // $3
			r.PpvW,     // $4
			r.Vpv1V,    // $5
			r.Vpv2V,    // $6
			r.Ipv1A,    // $7
			r.Ipv2A,    // $8
			r.Vac1V,    // $9
			r.Iac1A,    // $10
		)
	}

	query := fmt.Sprintf(`
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
VALUES %s
ON CONFLICT (ts, device_sn) DO UPDATE SET
    pac_w      = EXCLUDED.pac_w,
    ppv_w      = EXCLUDED.ppv_w,
    vpv1_v     = EXCLUDED.vpv1_v,
    vpv2_v     = EXCLUDED.vpv2_v,
    ipv1_a     = EXCLUDED.ipv1_a,
    ipv2_a     = EXCLUDED.ipv2_a,
    vac1_v     = EXCLUDED.vac1_v,
    iac1_a     = EXCLUDED.iac1_a,
    fetched_at = EXCLUDED.fetched_at`,
		strings.Join(valuesClauses, ",\n       "))

	tag, err := w.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("executing upsert: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

// UpsertSingle inserts a single reading. Used when batch is not needed.
func (w *Writer) UpsertSingle(ctx context.Context, r PowerReading) error {
	_, err := w.pool.Exec(ctx, db.UpsertPowerReadingSQL,
		r.TS, r.DeviceSN, r.PacW, r.PpvW,
		r.Vpv1V, r.Vpv2V, r.Ipv1A, r.Ipv2A,
		r.Vac1V, r.Iac1A,
	)
	return err
}
```

### Parser: `internal/fetcher/parser.go`

```go
package fetcher

import (
	"strconv"
	"strings"
	"time"

	"github.com/gogrowatt/pkg/growatt"
)

// ParseMINHistoryData converts raw MINHistoryDataPoint slices into PowerReading slices
// ready for database insertion.
//
// Parameters:
//   - datas: the raw data points from the API response
//   - dateStr: the request date string "YYYY-MM-DD"
//   - deviceSN: the device serial number
//   - loc: the timezone location used for parsing
//
// Timestamps are converted to UTC for storage.
func ParseMINHistoryData(datas []growatt.MINHistoryDataPoint, dateStr string, deviceSN string, loc *time.Location) []PowerReading {
	readings := make([]PowerReading, 0, len(datas))

	for _, dp := range datas {
		ts, err := parseReadingTime(dp.Time, dateStr, loc)
		if err != nil {
			// Skip unparseable data points rather than failing the whole batch
			continue
		}

		readings = append(readings, PowerReading{
			TS:       ts.UTC(), // Store as UTC
			DeviceSN: deviceSN,
			PacW:     dp.Pac.Float64(),
			PpvW:     dp.Ppv.Float64(),
			Vpv1V:    dp.Vpv1.Float64(),
			Vpv2V:    dp.Vpv2.Float64(),
			Ipv1A:    dp.Ipv1.Float64(),
			Ipv2A:    dp.Ipv2.Float64(),
			Vac1V:    dp.Vac1.Float64(),
			Iac1A:    dp.Iac1.Float64(),
		})
	}

	return readings
}

// parseReadingTime constructs a full timestamp from a Growatt time string and request date.
//
// The Growatt API returns time in one of these formats:
//   - "HH:MM"                   (e.g., "12:05")
//   - "YYYY-MM-DD HH:MM"       (e.g., "2026-02-15 12:05")
//   - "YYYY-MM-DD HH:MM:SS"    (e.g., "2026-02-15 12:05:00")
//
// In all cases, the time is in the timezone specified by the timezone_id parameter
// passed to the API. We parse it in that timezone and then convert to UTC.
func parseReadingTime(timeStr string, dateStr string, loc *time.Location) (time.Time, error) {
	// Normalize: extract just HH:MM from any format
	normalized := normalizeTimeStr(timeStr)

	// Combine date + time
	combined := dateStr + " " + normalized

	// Parse in the configured timezone
	t, err := time.ParseInLocation("2006-01-02 15:04", combined, loc)
	if err != nil {
		return time.Time{}, err
	}

	return t, nil
}

// normalizeTimeStr extracts HH:MM from various time formats.
// This mirrors the logic in pkg/growatt/types.go:normalizeTime() but is
// kept here to avoid coupling the fetcher's parsing to the client library's
// internal helper.
func normalizeTimeStr(t string) string {
	// Handle "YYYY-MM-DD HH:MM" or "YYYY-MM-DD HH:MM:SS"
	if strings.Contains(t, " ") {
		parts := strings.Split(t, " ")
		if len(parts) >= 2 {
			t = parts[1]
		}
	}
	// Truncate seconds if present (HH:MM:SS -> HH:MM)
	if len(t) > 5 && t[2] == ':' {
		t = t[:5]
	}
	return t
}

// parseHourMinute extracts hour and minute from an "HH:MM" string.
func parseHourMinute(hhmm string) (int, int, error) {
	parts := strings.Split(hhmm, ":")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("invalid time format: %q", hhmm)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return hour, minute, nil
}
```

Note: `parseHourMinute` requires an import of `"fmt"` -- the import list in the actual file should include it.

---

## 9. Metadata Tracker

### `internal/fetcher/metadata.go`

```go
package fetcher

import (
	"context"
	"log/slog"

	"github.com/gogrowatt/internal/db"
)

// MetadataTracker records collection runs and gap information.
type MetadataTracker struct {
	pool   DBPool
	logger *slog.Logger
}

// NewMetadataTracker creates a new MetadataTracker.
func NewMetadataTracker(pool DBPool, logger *slog.Logger) *MetadataTracker {
	return &MetadataTracker{pool: pool, logger: logger}
}

// StartRun inserts a new 'running' row into growatt.collection_runs
// and returns the generated ID.
func (m *MetadataTracker) StartRun(ctx context.Context, sourceType, sourceID, startDate, endDate string) (int64, error) {
	var id int64
	err := m.pool.QueryRow(ctx, db.InsertCollectionRunSQL,
		sourceType, sourceID, startDate, endDate,
	).Scan(&id)
	if err != nil {
		m.logger.Warn("failed to insert collection_runs row", "error", err)
		return 0, err
	}
	return id, nil
}

// FinishRunSuccess marks a collection run as successful.
func (m *MetadataTracker) FinishRunSuccess(ctx context.Context, runID int64, pointsCollected int) error {
	_, err := m.pool.Exec(ctx, db.UpdateCollectionRunSuccessSQL, runID, pointsCollected)
	return err
}

// FinishRunError marks a collection run as failed.
func (m *MetadataTracker) FinishRunError(ctx context.Context, runID int64, errMsg string) error {
	_, err := m.pool.Exec(ctx, db.UpdateCollectionRunErrorSQL, runID, errMsg)
	return err
}

// FinishRunPartial marks a collection run as partially successful.
func (m *MetadataTracker) FinishRunPartial(ctx context.Context, runID int64, pointsCollected int, errMsg string) error {
	_, err := m.pool.Exec(ctx, db.UpdateCollectionRunPartialSQL, runID, pointsCollected, errMsg)
	return err
}

// RecordGap inserts or updates a gap record in growatt.collection_gaps.
func (m *MetadataTracker) RecordGap(ctx context.Context, deviceSN string, gapDate string, expectedStart, expectedEnd string, reason string) error {
	_, err := m.pool.Exec(ctx, db.UpsertCollectionGapSQL,
		deviceSN, gapDate, expectedStart, expectedEnd, reason,
	)
	return err
}

// ResolveGap marks a previously-detected gap as resolved.
func (m *MetadataTracker) ResolveGap(ctx context.Context, deviceSN string, gapDate string) error {
	_, err := m.pool.Exec(ctx, db.ResolveCollectionGapSQL, deviceSN, gapDate)
	return err
}
```

---

## 10. Backfill Manager

### `internal/fetcher/backfill.go`

```go
package fetcher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gogrowatt/internal/db"
)

// BackfillManager handles startup backfill of historical data.
type BackfillManager struct {
	cfg    Config
	client GrowattClient
	writer *Writer
	meta   *MetadataTracker
	pool   DBPool
	logger *slog.Logger
}

// NewBackfillManager creates a new BackfillManager.
func NewBackfillManager(cfg Config, client GrowattClient, writer *Writer, meta *MetadataTracker, pool DBPool, logger *slog.Logger) *BackfillManager {
	return &BackfillManager{
		cfg:    cfg,
		client: client,
		writer: writer,
		meta:   meta,
		pool:   pool,
		logger: logger,
	}
}

// Run performs the startup backfill.
//
// Algorithm:
//  1. Query MAX(ts) from growatt.power_readings for this device.
//  2. If NULL or > BackfillDays ago: start from now - BackfillDays.
//  3. If within BackfillDays: start from that reading's calendar date.
//  4. Iterate day-by-day from backfill_start to today.
//  5. For each day, call GetMINInverterHistoryRaw and upsert all points.
//  6. The client's built-in 3-second rate limiter spaces API calls automatically.
func (b *BackfillManager) Run(ctx context.Context) error {
	if b.cfg.BackfillDays == 0 {
		b.logger.Info("backfill disabled (BackfillDays=0)")
		return nil
	}

	loc, err := time.LoadLocation(b.cfg.Timezone)
	if err != nil {
		return fmt.Errorf("loading timezone: %w", err)
	}

	// Step 1: Find last reading
	var lastTS *time.Time
	err = b.pool.QueryRow(ctx, db.LastReadingTimeSQL, b.cfg.DeviceSN).Scan(&lastTS)
	if err != nil {
		// pgx returns nil scan target for NULL, not an error
		b.logger.Debug("no previous readings found", "device_sn", b.cfg.DeviceSN)
	}

	// Step 2: Compute backfill start date
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	maxLookback := today.AddDate(0, 0, -b.cfg.BackfillDays)

	var backfillStart time.Time
	if lastTS == nil {
		backfillStart = maxLookback
		b.logger.Info("no previous data, backfilling from max lookback",
			"start", backfillStart.Format("2006-01-02"),
		)
	} else {
		lastLocal := lastTS.In(loc)
		lastDate := time.Date(lastLocal.Year(), lastLocal.Month(), lastLocal.Day(), 0, 0, 0, 0, loc)
		if lastDate.Before(maxLookback) {
			backfillStart = maxLookback
			b.logger.Info("last reading older than lookback window, starting from max",
				"last_ts", lastTS.Format(time.RFC3339),
				"start", backfillStart.Format("2006-01-02"),
			)
		} else {
			backfillStart = lastDate
			b.logger.Info("resuming from last reading date",
				"last_ts", lastTS.Format(time.RFC3339),
				"start", backfillStart.Format("2006-01-02"),
			)
		}
	}

	// Step 3: Iterate day-by-day
	totalPoints := 0
	daysProcessed := 0

	for date := backfillStart; !date.After(today); date = date.AddDate(0, 0, 1) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		dateStr := date.Format("2006-01-02")

		// Start tracking
		runID, _ := b.meta.StartRun(ctx, "device_history", b.cfg.DeviceSN, dateStr, dateStr)

		// Fetch raw data
		b.logger.Info("backfilling", "date", dateStr)
		histResp, _, err := b.client.GetMINInverterHistoryRaw(ctx, b.cfg.DeviceSN, date, b.cfg.Timezone)
		if err != nil {
			b.logger.Error("backfill fetch failed", "date", dateStr, "error", err)
			if runID > 0 {
				_ = b.meta.FinishRunError(ctx, runID, err.Error())
			}
			// Continue with next day rather than aborting entire backfill
			continue
		}

		// Parse and upsert
		readings := ParseMINHistoryData(histResp.Datas, dateStr, b.cfg.DeviceSN, loc)
		if len(readings) > 0 {
			written, err := b.writer.UpsertBatch(ctx, readings)
			if err != nil {
				b.logger.Error("backfill upsert failed", "date", dateStr, "error", err)
				if runID > 0 {
					_ = b.meta.FinishRunError(ctx, runID, err.Error())
				}
				continue
			}
			totalPoints += written
			b.logger.Info("backfill day complete",
				"date", dateStr,
				"points", written,
			)
		} else {
			b.logger.Info("backfill day empty", "date", dateStr)
		}

		if runID > 0 {
			_ = b.meta.FinishRunSuccess(ctx, runID, len(readings))
		}
		daysProcessed++
	}

	b.logger.Info("backfill complete",
		"days_processed", daysProcessed,
		"total_points", totalPoints,
	)
	return nil
}
```

### Backfill Timing

A 7-day backfill requires at most 8 API calls (7 past days + today). With the client's 3-second
rate limiter, this takes approximately 24 seconds. This is fast enough to run synchronously at
startup before entering steady-state polling.

---

## 11. Gap Detection

### `internal/fetcher/gaps.go`

```go
package fetcher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gogrowatt/internal/db"
)

// gapThreshold is the minimum gap duration to report.
// Normal 5-minute intervals mean a gap > 10 minutes is suspicious.
const gapThreshold = 10 * time.Minute

// DetectGaps scans power_readings for a given device and date,
// looking for gaps larger than gapThreshold during expected production hours.
func DetectGaps(ctx context.Context, pool DBPool, deviceSN string, date time.Time, logger *slog.Logger) error {
	dateStr := date.Format("2006-01-02")
	loc := date.Location()

	// Define the scan window: 5 AM to 10 PM local time (covers all US solar hours)
	dayStart := time.Date(date.Year(), date.Month(), date.Day(), 5, 0, 0, 0, loc)
	dayEnd := time.Date(date.Year(), date.Month(), date.Day(), 22, 0, 0, 0, loc)

	rows, err := pool.Query(ctx, db.ReadingsInRangeSQL, deviceSN, dayStart.UTC(), dayEnd.UTC())
	if err != nil {
		return fmt.Errorf("querying readings for gap detection: %w", err)
	}
	defer rows.Close()

	var timestamps []time.Time
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			return fmt.Errorf("scanning timestamp: %w", err)
		}
		timestamps = append(timestamps, ts)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating timestamps: %w", err)
	}

	if len(timestamps) == 0 {
		// No data at all for this day
		logger.Info("no readings found for date, recording gap",
			"device_sn", deviceSN,
			"date", dateStr,
		)
		meta := NewMetadataTracker(pool, logger)
		return meta.RecordGap(ctx, deviceSN, dateStr, "05:00", "22:00", "no_data_returned")
	}

	// Walk timestamps looking for gaps
	meta := NewMetadataTracker(pool, logger)
	gapsFound := 0

	for i := 1; i < len(timestamps); i++ {
		gap := timestamps[i].Sub(timestamps[i-1])
		if gap > gapThreshold {
			gapStartLocal := timestamps[i-1].In(loc)
			gapEndLocal := timestamps[i].In(loc)
			logger.Warn("gap detected",
				"device_sn", deviceSN,
				"date", dateStr,
				"gap_start", gapStartLocal.Format("15:04"),
				"gap_end", gapEndLocal.Format("15:04"),
				"duration", gap.String(),
			)
			// Record the gap (will be upserted on the UNIQUE constraint)
			if err := meta.RecordGap(ctx, deviceSN, dateStr,
				gapStartLocal.Format("15:04"),
				gapEndLocal.Format("15:04"),
				"partial_day",
			); err != nil {
				logger.Warn("failed to record gap", "error", err)
			}
			gapsFound++
		}
	}

	// If no gaps found for this date, resolve any previously recorded gap
	if gapsFound == 0 {
		_ = meta.ResolveGap(ctx, deviceSN, dateStr)
	}

	return nil
}
```

---

## 12. Health and Metrics

### `internal/fetcher/health.go`

```go
package fetcher

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// HealthData is the JSON response from /healthz.
type HealthData struct {
	State           string    `json:"state"`
	LastFetchTime   time.Time `json:"last_fetch_time,omitempty"`
	LastFetchStatus string    `json:"last_fetch_status,omitempty"`
	LastFetchPoints int       `json:"last_fetch_points"`
	TotalFetches    int64     `json:"total_fetches"`
	TotalErrors     int64     `json:"total_errors"`
	TotalPoints     int64     `json:"total_points"`
	DeviceSN        string    `json:"device_sn"`
}

// HealthProvider is implemented by the Fetcher to expose health data.
type HealthProvider interface {
	HealthStatus() HealthData
}

// StartHealthServer creates and starts an HTTP server for health checks.
// Returns the server for deferred Close().
func StartHealthServer(port int, provider HealthProvider, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()

	// /healthz -- liveness probe
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		status := provider.HealthStatus()
		w.Header().Set("Content-Type", "application/json")

		// Return 503 if no successful fetch in the last 30 minutes
		if status.TotalFetches > 0 && time.Since(status.LastFetchTime) > 30*time.Minute {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		json.NewEncoder(w).Encode(status)
	})

	// /readyz -- readiness probe (ready once at least one fetch has succeeded)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		status := provider.HealthStatus()
		if status.TotalFetches == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "not ready: no successful fetches yet")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ready")
	})

	// /metrics -- Prometheus-style plain text metrics
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		status := provider.HealthStatus()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "# HELP gogrowatt_fetcher_total_fetches Total number of fetch cycles\n")
		fmt.Fprintf(w, "# TYPE gogrowatt_fetcher_total_fetches counter\n")
		fmt.Fprintf(w, "gogrowatt_fetcher_total_fetches %d\n", status.TotalFetches)
		fmt.Fprintf(w, "# HELP gogrowatt_fetcher_total_errors Total number of failed fetch cycles\n")
		fmt.Fprintf(w, "# TYPE gogrowatt_fetcher_total_errors counter\n")
		fmt.Fprintf(w, "gogrowatt_fetcher_total_errors %d\n", status.TotalErrors)
		fmt.Fprintf(w, "# HELP gogrowatt_fetcher_total_points Total data points written\n")
		fmt.Fprintf(w, "# TYPE gogrowatt_fetcher_total_points counter\n")
		fmt.Fprintf(w, "gogrowatt_fetcher_total_points %d\n", status.TotalPoints)
		fmt.Fprintf(w, "# HELP gogrowatt_fetcher_last_fetch_points Points in last fetch cycle\n")
		fmt.Fprintf(w, "# TYPE gogrowatt_fetcher_last_fetch_points gauge\n")
		fmt.Fprintf(w, "gogrowatt_fetcher_last_fetch_points %d\n", status.LastFetchPoints)
		if !status.LastFetchTime.IsZero() {
			fmt.Fprintf(w, "# HELP gogrowatt_fetcher_last_fetch_timestamp_seconds Unix timestamp of last fetch\n")
			fmt.Fprintf(w, "# TYPE gogrowatt_fetcher_last_fetch_timestamp_seconds gauge\n")
			fmt.Fprintf(w, "gogrowatt_fetcher_last_fetch_timestamp_seconds %d\n", status.LastFetchTime.Unix())
		}
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("health server error", "error", err)
		}
	}()

	return server
}
```

### Endpoints Summary

| Endpoint | Purpose | Response |
|---|---|---|
| `GET /healthz` | Liveness probe | 200 if fetching normally, 503 if stale > 30 min |
| `GET /readyz` | Readiness probe | 200 after first successful fetch, 503 before |
| `GET /metrics` | Prometheus metrics | Plain text counters and gauges |

---

## 13. Interfaces for Testability

All external dependencies are behind interfaces so unit tests can inject mocks.

### `internal/fetcher/interfaces.go`

```go
package fetcher

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/gogrowatt/pkg/growatt"
)

// GrowattClient abstracts the Growatt API methods used by the fetcher.
// The real implementation is *growatt.Client.
type GrowattClient interface {
	// GetMINInverterHistoryRaw returns raw historical data without conversion.
	GetMINInverterHistoryRaw(ctx context.Context, serial string, date time.Time, timezone string) (*growatt.MINHistoryResponse, string, error)

	// ListPlants returns all plants (used for auto-detection).
	ListPlants(ctx context.Context) ([]growatt.Plant, error)

	// ListDevices returns all devices for a plant (used for auto-detection).
	ListDevices(ctx context.Context, plantID string) ([]growatt.Device, error)
}

// DBPool abstracts the pgxpool.Pool methods used by the fetcher.
// This allows injecting a mock or test pool.
type DBPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Close()
}
```

### Why Interfaces

| Interface | Real Implementation | Mock (tests) |
|---|---|---|
| `GrowattClient` | `*growatt.Client` (after adding `GetMINInverterHistoryRaw`) | `MockGrowattClient` with canned responses |
| `DBPool` | `*pgxpool.Pool` | `MockDBPool` using pgxmock or in-memory state |

The `*growatt.Client` struct already satisfies `GrowattClient` once the new
`GetMINInverterHistoryRaw` method is added. No adapter or wrapper needed.

---

## 14. Error Handling and Retry Strategy

### Error Classification

```go
// In fetchWithRetry() (shown in section 6), errors are classified as:

switch {
case growatt.IsRateLimited(err):
    // Fixed 30s wait, max 3 rate-limit retries per cycle
    // Detection: error code 10012 + message contains "frequently"

case growatt.IsPermissionDenied(err):
    // No retry. Log CRITICAL and return immediately.
    // Detection: error code 10011

case ctx.Err() != nil:
    // Context cancelled (shutdown). Return immediately.

default:
    // Network, timeout, or unknown API error.
    // Exponential backoff: 5s, 10s, 20s, 40s, 80s, capped at 5 min.
    // Max 5 retries per cycle.
}
```

### Retry Parameters

| Error Type | Wait Strategy | Max Retries | On Exhaustion |
|---|---|---|---|
| Rate limited (10012 + "frequently") | Fixed 30s | 3 | Skip cycle, schedule next |
| Network / HTTP timeout | Exponential 5s base, 2x | 5 | Skip cycle, schedule next |
| Permission denied (10011) | None | 0 | Log critical, return error |
| Context cancelled | None | 0 | Graceful shutdown |
| DB write error | Fixed 5s | 3 | Skip write, data re-upserted next cycle |

### DB Error Handling

Database errors during upsert do NOT stop the fetcher. The daemon continues polling, and the
same data points will be re-upserted on the next cycle (since we always fetch the full day).
The upsert's `ON CONFLICT DO UPDATE` ensures idempotency.

```go
// In the fetchCycle method, DB errors are logged but do not terminate polling:
written, err := f.writer.UpsertBatch(ctx, allReadings)
if err != nil {
    f.logger.Error("upsert failed, will retry next cycle", "error", err)
    // Record in collection_runs as 'error'
    // Continue to next poll tick -- data will be fetched and upserted again
}
```

---

## 15. Logging Strategy

### Structured Logging with `log/slog`

All logging uses Go's standard `log/slog` package with JSON output. This provides structured
fields that are easy to parse in log aggregation systems (CloudWatch, Loki, etc.).

### Log Levels

| Level | Usage |
|---|---|
| `DEBUG` | Timer computations, batch chunk details, SQL debug |
| `INFO` | Fetch cycle start/complete, backfill progress, startup, shutdown |
| `WARN` | Rate limiting, gap detection, metadata tracking failures |
| `ERROR` | API failures, DB write failures, permission denied |

### Standard Log Fields

Every log message includes these contextual fields where applicable:

```go
logger.Info("fetch cycle complete",
    "device_sn", f.cfg.DeviceSN,     // always present
    "points_written", result.PointsWritten,
    "latest_ts", result.LatestTimestamp.Format(time.RFC3339),
    "next_fetch", nextFetch.Format(time.RFC3339),
    "dates_queried", result.DatesQueried,
)
```

### Example Log Output

```json
{"time":"2026-02-15T12:11:03Z","level":"INFO","msg":"fetch cycle complete","device_sn":"ABC123","points_written":144,"latest_ts":"2026-02-15T18:10:00Z","next_fetch":"2026-02-15T18:16:00Z"}
{"time":"2026-02-15T12:11:03Z","level":"WARN","msg":"gap detected","device_sn":"ABC123","date":"2026-02-15","gap_start":"13:00","gap_end":"13:30","duration":"30m0s"}
{"time":"2026-02-15T12:11:33Z","level":"WARN","msg":"rate limited, waiting 30s","attempt":1,"rate_limit_retry":1}
```

---

## 16. Unit Test Strategy

### General Approach

All unit tests use table-driven patterns with fixed time values for determinism. External
dependencies (API client, database) are mocked via the interfaces defined in section 13.

### Mock Implementations

```go
// internal/fetcher/mocks_test.go

package fetcher

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/gogrowatt/pkg/growatt"
)

// MockGrowattClient implements GrowattClient for testing.
type MockGrowattClient struct {
	HistoryResponses map[string]*growatt.MINHistoryResponse // keyed by date string
	HistoryErr       error
	Plants           []growatt.Plant
	PlantsErr        error
	Devices          []growatt.Device
	DevicesErr       error
	CallCount        int
}

func (m *MockGrowattClient) GetMINInverterHistoryRaw(ctx context.Context, serial string, date time.Time, timezone string) (*growatt.MINHistoryResponse, string, error) {
	m.CallCount++
	dateStr := date.Format("2006-01-02")
	if m.HistoryErr != nil {
		return nil, dateStr, m.HistoryErr
	}
	resp, ok := m.HistoryResponses[dateStr]
	if !ok {
		return &growatt.MINHistoryResponse{Count: 0, Datas: nil}, dateStr, nil
	}
	return resp, dateStr, nil
}

func (m *MockGrowattClient) ListPlants(ctx context.Context) ([]growatt.Plant, error) {
	return m.Plants, m.PlantsErr
}

func (m *MockGrowattClient) ListDevices(ctx context.Context, plantID string) ([]growatt.Device, error) {
	return m.Devices, m.DevicesErr
}

// MockDBPool implements DBPool for testing.
type MockDBPool struct {
	ExecFunc     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryFunc    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRowFunc func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (m *MockDBPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if m.ExecFunc != nil {
		return m.ExecFunc(ctx, sql, args...)
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (m *MockDBPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.QueryFunc != nil {
		return m.QueryFunc(ctx, sql, args...)
	}
	return nil, nil
}

func (m *MockDBPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.QueryRowFunc != nil {
		return m.QueryRowFunc(ctx, sql, args...)
	}
	return nil
}

func (m *MockDBPool) Close() {}
```

### Timer Tests: `internal/fetcher/timer_test.go`

```go
package fetcher

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestAdaptiveTimer_NextFetchTime(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	timer := NewAdaptiveTimer(5*time.Minute, logger)

	now := time.Date(2026, 2, 15, 12, 6, 0, 0, time.UTC)

	tests := []struct {
		name     string
		latestTS time.Time
		now      time.Time
		want     time.Time
	}{
		{
			name:     "no data points",
			latestTS: time.Time{}, // zero value
			now:      now,
			want:     now.Add(5 * time.Minute), // 12:11
		},
		{
			name:     "normal case: latest at 12:05",
			latestTS: time.Date(2026, 2, 15, 12, 5, 0, 0, time.UTC),
			now:      now,
			want:     time.Date(2026, 2, 15, 12, 11, 0, 0, time.UTC), // 12:05 + 6m
		},
		{
			name:     "stale data: latest at 11:00",
			latestTS: time.Date(2026, 2, 15, 11, 0, 0, 0, time.UTC),
			now:      now,
			want:     now.Add(1 * time.Minute), // minWait floor
		},
		{
			name:     "future data: latest at 12:30",
			latestTS: time.Date(2026, 2, 15, 12, 30, 0, 0, time.UTC),
			now:      now,
			want:     now.Add(10 * time.Minute), // maxWait cap (12:36 > 12:16)
		},
		{
			name:     "data exactly at now",
			latestTS: now,
			now:      now,
			want:     now.Add(6 * time.Minute), // 12:06 + 6m = 12:12
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timer.NextFetchTime(tt.latestTS, tt.now)
			if !got.Equal(tt.want) {
				t.Errorf("NextFetchTime() = %v, want %v", got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
		})
	}
}
```

### Parser Tests: `internal/fetcher/parser_test.go`

```go
package fetcher

import (
	"testing"
	"time"

	"github.com/gogrowatt/pkg/growatt"
)

func TestParseReadingTime(t *testing.T) {
	loc, _ := time.LoadLocation("US/Central")

	tests := []struct {
		name    string
		timeStr string
		dateStr string
		wantUTC time.Time
		wantErr bool
	}{
		{
			name:    "HH:MM format",
			timeStr: "12:05",
			dateStr: "2026-02-15",
			wantUTC: time.Date(2026, 2, 15, 18, 5, 0, 0, time.UTC), // CST = UTC-6
		},
		{
			name:    "full datetime format",
			timeStr: "2026-02-15 12:05",
			dateStr: "2026-02-15",
			wantUTC: time.Date(2026, 2, 15, 18, 5, 0, 0, time.UTC),
		},
		{
			name:    "with seconds",
			timeStr: "2026-02-15 12:05:00",
			dateStr: "2026-02-15",
			wantUTC: time.Date(2026, 2, 15, 18, 5, 0, 0, time.UTC),
		},
		{
			name:    "midnight boundary",
			timeStr: "23:55",
			dateStr: "2026-02-15",
			wantUTC: time.Date(2026, 2, 16, 5, 55, 0, 0, time.UTC), // 23:55 CST = 05:55 UTC next day
		},
		{
			name:    "empty string",
			timeStr: "",
			dateStr: "2026-02-15",
			wantErr: true,
		},
		{
			name:    "invalid format",
			timeStr: "not-a-time",
			dateStr: "2026-02-15",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseReadingTime(tt.timeStr, tt.dateStr, loc)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotUTC := got.UTC()
			if !gotUTC.Equal(tt.wantUTC) {
				t.Errorf("parseReadingTime() = %v, want %v",
					gotUTC.Format(time.RFC3339),
					tt.wantUTC.Format(time.RFC3339),
				)
			}
		})
	}
}

func TestParseMINHistoryData(t *testing.T) {
	loc, _ := time.LoadLocation("US/Central")

	datas := []growatt.MINHistoryDataPoint{
		{
			Time: "12:05",
			Pac:  growatt.FlexFloat(2729.5),
			Ppv:  growatt.FlexFloat(2810.0),
			Vpv1: growatt.FlexFloat(312.4),
			Vpv2: growatt.FlexFloat(310.1),
			Ipv1: growatt.FlexFloat(4.5),
			Ipv2: growatt.FlexFloat(4.6),
			Vac1: growatt.FlexFloat(241.2),
			Iac1: growatt.FlexFloat(11.3),
		},
		{
			Time: "12:10",
			Pac:  growatt.FlexFloat(5223.3),
			Ppv:  growatt.FlexFloat(5380.0),
			Vpv1: growatt.FlexFloat(315.7),
			Vpv2: growatt.FlexFloat(313.2),
			Ipv1: growatt.FlexFloat(8.5),
			Ipv2: growatt.FlexFloat(8.7),
			Vac1: growatt.FlexFloat(240.8),
			Iac1: growatt.FlexFloat(21.7),
		},
	}

	readings := ParseMINHistoryData(datas, "2026-02-15", "DEVICE001", loc)

	if len(readings) != 2 {
		t.Fatalf("expected 2 readings, got %d", len(readings))
	}

	// First reading
	r0 := readings[0]
	if r0.DeviceSN != "DEVICE001" {
		t.Errorf("DeviceSN = %q, want %q", r0.DeviceSN, "DEVICE001")
	}
	wantTS := time.Date(2026, 2, 15, 18, 5, 0, 0, time.UTC)
	if !r0.TS.Equal(wantTS) {
		t.Errorf("TS = %v, want %v", r0.TS, wantTS)
	}
	if r0.PacW != 2729.5 {
		t.Errorf("PacW = %f, want %f", r0.PacW, 2729.5)
	}
	if r0.Vpv1V != 312.4 {
		t.Errorf("Vpv1V = %f, want %f", r0.Vpv1V, 312.4)
	}
}
```

### Config Tests: `internal/fetcher/config_test.go`

```go
package fetcher

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfigFromEnv(t *testing.T) {
	// Set required env vars
	os.Setenv("GROWATT_API_KEY", "test-token")
	os.Setenv("DATABASE_URL", "postgres://localhost/test")
	defer os.Unsetenv("GROWATT_API_KEY")
	defer os.Unsetenv("DATABASE_URL")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.APIKey != "test-token" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "test-token")
	}
	if cfg.FetchInterval != 5*time.Minute {
		t.Errorf("FetchInterval = %v, want %v", cfg.FetchInterval, 5*time.Minute)
	}
	if cfg.BackfillDays != 7 {
		t.Errorf("BackfillDays = %d, want %d", cfg.BackfillDays, 7)
	}
}

func TestLoadConfigFromEnv_MissingAPIKey(t *testing.T) {
	os.Unsetenv("GROWATT_API_KEY")
	os.Setenv("DATABASE_URL", "postgres://localhost/test")
	defer os.Unsetenv("DATABASE_URL")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Error("expected error for missing API key")
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid defaults",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "missing API key",
			modify:  func(c *Config) { c.APIKey = "" },
			wantErr: true,
		},
		{
			name:    "interval too short",
			modify:  func(c *Config) { c.FetchInterval = 10 * time.Second },
			wantErr: true,
		},
		{
			name:    "backfill days too high",
			modify:  func(c *Config) { c.BackfillDays = 30 },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.APIKey = "test"
			cfg.DatabaseURL = "postgres://localhost/test"
			tt.modify(&cfg)

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
```

### Writer Tests: `internal/fetcher/writer_test.go`

```go
package fetcher

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestWriter_UpsertBatch(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	var capturedSQL []string
	var capturedArgs [][]any

	mockPool := &MockDBPool{
		ExecFunc: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			capturedSQL = append(capturedSQL, sql)
			capturedArgs = append(capturedArgs, args)
			return pgconn.NewCommandTag("INSERT 0 2"), nil
		},
	}

	writer := NewWriter(mockPool, logger)

	readings := []PowerReading{
		{
			TS:       time.Date(2026, 2, 15, 18, 5, 0, 0, time.UTC),
			DeviceSN: "DEV001",
			PacW:     2729.5,
			PpvW:     2810.0,
			Vpv1V:    312.4,
			Vpv2V:    310.1,
			Ipv1A:    4.5,
			Ipv2A:    4.6,
			Vac1V:    241.2,
			Iac1A:    11.3,
		},
		{
			TS:       time.Date(2026, 2, 15, 18, 10, 0, 0, time.UTC),
			DeviceSN: "DEV001",
			PacW:     5223.3,
			PpvW:     5380.0,
			Vpv1V:    315.7,
			Vpv2V:    313.2,
			Ipv1A:    8.5,
			Ipv2A:    8.7,
			Vac1V:    240.8,
			Iac1A:    21.7,
		},
	}

	affected, err := writer.UpsertBatch(context.Background(), readings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if affected != 2 {
		t.Errorf("affected = %d, want 2", affected)
	}

	if len(capturedSQL) != 1 {
		t.Fatalf("expected 1 SQL call, got %d", len(capturedSQL))
	}

	// Verify the SQL contains the correct table and conflict clause
	sql := capturedSQL[0]
	if !strings.Contains(sql, "growatt.power_readings") {
		t.Error("SQL does not reference growatt.power_readings")
	}
	if !strings.Contains(sql, "ON CONFLICT (ts, device_sn)") {
		t.Error("SQL does not contain correct ON CONFLICT clause")
	}
	if !strings.Contains(sql, "fetched_at = EXCLUDED.fetched_at") {
		t.Error("SQL does not update fetched_at on conflict")
	}

	// Verify args count: 2 rows * 10 cols = 20 args
	if len(capturedArgs[0]) != 20 {
		t.Errorf("expected 20 args, got %d", len(capturedArgs[0]))
	}
}

func TestWriter_UpsertBatch_Empty(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	mockPool := &MockDBPool{}
	writer := NewWriter(mockPool, logger)

	affected, err := writer.UpsertBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0", affected)
	}
}

func TestWriter_UpsertBatch_MultipleBatches(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	callCount := 0
	mockPool := &MockDBPool{
		ExecFunc: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			callCount++
			rowCount := len(args) / 10 // 10 params per row
			return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", rowCount)), nil
		},
	}

	writer := NewWriter(mockPool, logger)

	// Create 250 readings -> should produce 3 batch chunks (100+100+50)
	readings := make([]PowerReading, 250)
	for i := range readings {
		readings[i] = PowerReading{
			TS:       time.Date(2026, 2, 15, 0, i*5, 0, 0, time.UTC),
			DeviceSN: "DEV001",
			PacW:     float64(i * 100),
		}
	}

	affected, err := writer.UpsertBatch(context.Background(), readings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 3 {
		t.Errorf("expected 3 batch calls, got %d", callCount)
	}
	if affected != 250 {
		t.Errorf("affected = %d, want 250", affected)
	}
}
```

Note: The `TestWriter_UpsertBatch_MultipleBatches` test references `fmt.Sprintf` -- the test file should import `"fmt"`.

---

## 17. Integration Test Strategy

Integration tests require a running TimescaleDB instance and (optionally) a Growatt API simulator.

### Docker Compose for Tests

```yaml
# docker-compose.test.yml
services:
  timescaledb:
    image: timescale/timescaledb:latest-pg16
    environment:
      POSTGRES_DB: growatt_test
      POSTGRES_USER: growatt
      POSTGRES_PASSWORD: testpass
    ports:
      - "5433:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U growatt -d growatt_test"]
      interval: 2s
      timeout: 5s
      retries: 10
```

### Test Setup

```bash
# Start test database
docker compose -f docker-compose.test.yml up -d

# Wait for health
until docker compose -f docker-compose.test.yml exec timescaledb pg_isready -U growatt; do sleep 1; done

# Apply schema
psql -h localhost -p 5433 -U growatt -d growatt_test -f migrations/001_schema.sql

# Run integration tests
DATABASE_URL="postgres://growatt:testpass@localhost:5433/growatt_test" go test ./internal/fetcher/... -tags=integration -v

# Cleanup
docker compose -f docker-compose.test.yml down -v
```

### Integration Test Cases

#### 1. End-to-End Backfill

```go
//go:build integration

func TestBackfill_EmptyDatabase(t *testing.T) {
    pool := testPool(t) // helper that connects to the test database
    defer pool.Close()

    // Create a mock client that returns data for the last 3 days
    mockClient := &MockGrowattClient{
        HistoryResponses: map[string]*growatt.MINHistoryResponse{
            time.Now().AddDate(0, 0, -2).Format("2006-01-02"): makeTestResponse(12),
            time.Now().AddDate(0, 0, -1).Format("2006-01-02"): makeTestResponse(144),
            time.Now().Format("2006-01-02"):                     makeTestResponse(72),
        },
    }

    cfg := DefaultConfig()
    cfg.DeviceSN = "TEST001"
    cfg.BackfillDays = 3

    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    writer := NewWriter(pool, logger)
    meta := NewMetadataTracker(pool, logger)
    backfiller := NewBackfillManager(cfg, mockClient, writer, meta, pool, logger)

    err := backfiller.Run(context.Background())
    if err != nil {
        t.Fatalf("backfill failed: %v", err)
    }

    // Verify data was written
    var count int
    pool.QueryRow(context.Background(),
        "SELECT COUNT(*) FROM growatt.power_readings WHERE device_sn = $1", "TEST001",
    ).Scan(&count)

    if count != 228 { // 12 + 144 + 72
        t.Errorf("expected 228 readings, got %d", count)
    }

    // Verify collection_runs were recorded
    var runCount int
    pool.QueryRow(context.Background(),
        "SELECT COUNT(*) FROM growatt.collection_runs WHERE source_id = $1 AND status = 'success'", "TEST001",
    ).Scan(&runCount)

    if runCount != 3 {
        t.Errorf("expected 3 collection runs, got %d", runCount)
    }
}
```

#### 2. Upsert Overwrite Verification

```go
func TestUpsert_LatestWins(t *testing.T) {
    pool := testPool(t)
    defer pool.Close()

    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    writer := NewWriter(pool, logger)

    ts := time.Date(2026, 2, 15, 18, 5, 0, 0, time.UTC)

    // First insert: pac_w = 1000
    _, err := writer.UpsertBatch(context.Background(), []PowerReading{
        {TS: ts, DeviceSN: "TEST001", PacW: 1000.0, PpvW: 1100.0, Vpv1V: 300.0},
    })
    if err != nil {
        t.Fatalf("first upsert failed: %v", err)
    }

    // Second insert: pac_w = 1500 (should overwrite)
    _, err = writer.UpsertBatch(context.Background(), []PowerReading{
        {TS: ts, DeviceSN: "TEST001", PacW: 1500.0, PpvW: 1600.0, Vpv1V: 310.0},
    })
    if err != nil {
        t.Fatalf("second upsert failed: %v", err)
    }

    // Verify only one row exists and it has the latest value
    var pacW float64
    pool.QueryRow(context.Background(),
        "SELECT pac_w FROM growatt.power_readings WHERE ts = $1 AND device_sn = $2",
        ts, "TEST001",
    ).Scan(&pacW)

    if pacW != 1500.0 {
        t.Errorf("pac_w = %f, want 1500.0 (latest wins)", pacW)
    }

    var count int
    pool.QueryRow(context.Background(),
        "SELECT COUNT(*) FROM growatt.power_readings WHERE device_sn = $1", "TEST001",
    ).Scan(&count)

    if count != 1 {
        t.Errorf("expected 1 row (upsert), got %d", count)
    }
}
```

---

## 18. Dockerfile

### `Dockerfile.fetcher`

```dockerfile
# ---- Build stage ----
FROM golang:1.21-alpine AS builder

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /gogrowatt-fetcher \
    ./cmd/gogrowatt-fetcher/

# ---- Runtime stage ----
FROM alpine:3.19

# Install ca-certificates for HTTPS calls to Growatt API
# and tzdata for timezone support (time.LoadLocation)
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN adduser -D -u 1000 fetcher
USER fetcher

COPY --from=builder /gogrowatt-fetcher /usr/local/bin/gogrowatt-fetcher

# Health check endpoint
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD wget -q -O- http://localhost:8080/healthz || exit 1

ENTRYPOINT ["gogrowatt-fetcher"]
```

### Building and Running

```bash
# Build
docker build -f Dockerfile.fetcher -t gogrowatt-fetcher:latest .

# Run
docker run -d \
  --name gogrowatt-fetcher \
  -e GROWATT_API_KEY=your-token \
  -e GROWATT_DEVICE_SN=your-serial \
  -e DATABASE_URL=postgres://user:pass@host:5432/growatt \
  -e GROWATT_TIMEZONE=US/Central \
  -p 8080:8080 \
  gogrowatt-fetcher:latest
```

### Docker Compose (Production)

```yaml
# docker-compose.yml
services:
  timescaledb:
    image: timescale/timescaledb:latest-pg16
    environment:
      POSTGRES_DB: growatt
      POSTGRES_USER: growatt
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./migrations/001_schema.sql:/docker-entrypoint-initdb.d/001_schema.sql
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U growatt -d growatt"]
      interval: 5s
      timeout: 5s
      retries: 10

  fetcher:
    build:
      context: .
      dockerfile: Dockerfile.fetcher
    depends_on:
      timescaledb:
        condition: service_healthy
    environment:
      GROWATT_API_KEY: ${GROWATT_API_KEY}
      GROWATT_DEVICE_SN: ${GROWATT_DEVICE_SN}
      GROWATT_TIMEZONE: US/Central
      DATABASE_URL: postgres://growatt:${DB_PASSWORD}@timescaledb:5432/growatt
      LOG_LEVEL: info
    ports:
      - "8080:8080"
    restart: unless-stopped

volumes:
  pgdata:
```

---

## 19. Complete SQL Reference

This section consolidates every SQL statement the fetcher executes, in the exact form
used in `internal/db/queries.go`. All use the `growatt.` schema namespace, canonical column
names with unit suffixes, and PK order `(ts, device_sn)`.

### Upsert Power Reading (single row)

```sql
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (ts, device_sn) DO UPDATE SET
    pac_w      = EXCLUDED.pac_w,
    ppv_w      = EXCLUDED.ppv_w,
    vpv1_v     = EXCLUDED.vpv1_v,
    vpv2_v     = EXCLUDED.vpv2_v,
    ipv1_a     = EXCLUDED.ipv1_a,
    ipv2_a     = EXCLUDED.ipv2_a,
    vac1_v     = EXCLUDED.vac1_v,
    iac1_a     = EXCLUDED.iac1_a,
    fetched_at = EXCLUDED.fetched_at
```

### Upsert Power Reading (batch, dynamic N rows)

```sql
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now()),
       ($11, $12, $13, $14, $15, $16, $17, $18, $19, $20, now()),
       ...
ON CONFLICT (ts, device_sn) DO UPDATE SET
    pac_w      = EXCLUDED.pac_w,
    ppv_w      = EXCLUDED.ppv_w,
    vpv1_v     = EXCLUDED.vpv1_v,
    vpv2_v     = EXCLUDED.vpv2_v,
    ipv1_a     = EXCLUDED.ipv1_a,
    ipv2_a     = EXCLUDED.ipv2_a,
    vac1_v     = EXCLUDED.vac1_v,
    iac1_a     = EXCLUDED.iac1_a,
    fetched_at = EXCLUDED.fetched_at
```

### Query Last Reading Timestamp

```sql
SELECT MAX(ts)
FROM growatt.power_readings
WHERE device_sn = $1
```

### Query Readings for Gap Detection

```sql
SELECT ts
FROM growatt.power_readings
WHERE device_sn = $1
  AND ts >= $2
  AND ts < $3
ORDER BY ts
```

### Insert Collection Run (start)

```sql
INSERT INTO growatt.collection_runs (source_type, source_id, query_date_start, query_date_end, started_at, status)
VALUES ($1, $2, $3, $4, now(), 'running')
RETURNING id
```

### Update Collection Run (success)

```sql
UPDATE growatt.collection_runs
SET finished_at = now(),
    points_collected = $2,
    status = 'success'
WHERE id = $1
```

### Update Collection Run (error)

```sql
UPDATE growatt.collection_runs
SET finished_at = now(),
    status = 'error',
    error_message = $2
WHERE id = $1
```

### Upsert Collection Gap

```sql
INSERT INTO growatt.collection_gaps (device_sn, gap_date, expected_start, expected_end, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (device_sn, gap_date) DO UPDATE SET
    expected_start = EXCLUDED.expected_start,
    expected_end   = EXCLUDED.expected_end,
    reason         = EXCLUDED.reason,
    detected_at    = now()
```

### Resolve Collection Gap

```sql
UPDATE growatt.collection_gaps
SET resolved_at = now()
WHERE device_sn = $1
  AND gap_date = $2
  AND resolved_at IS NULL
```

---

## Summary of Consistency Fixes Applied

Per `00-consistency-analysis.md`, this design document incorporates:

| Issue | Fix Applied |
|---|---|
| Column naming (issue #1) | All columns use unit suffixes: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, `ipv1_a`, etc. |
| PK order (issue #2) | PK is `(ts, device_sn)` -- time-first for TimescaleDB |
| Metadata table names (issue #3) | Uses `growatt.collection_runs` and `growatt.collection_gaps` |
| Missing `fetched_at` (issue #4) | `fetched_at` column present, populated with `now()`, overwritten on upsert |
| Schema namespace (issue #11) | All SQL uses `growatt.` prefix consistently |
