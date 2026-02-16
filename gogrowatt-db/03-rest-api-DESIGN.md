# REST API Implementation Design

Detailed implementation design for the `gogrowatt-api` read-only REST service. This document translates the specification in `03-rest-api.md` into concrete Go code, incorporating all consistency fixes from `00-consistency-analysis.md` and referencing the canonical schema from `01-database-schema.md`.

**Consistency rules applied throughout:**
- Column names use unit suffixes: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, `ipv1_a`, etc.
- Primary key on `power_readings` is `(ts, device_sn)`.
- All SQL references use the `growatt.` schema prefix.
- JSON field names match DB column names: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, etc.
- Device-level energy queries use `growatt.mv_daily_production`; plant-level energy queries use `growatt.energy_summaries`.
- REST paths follow `/api/v1/...` canonical form.

---

## 1. Project Layout

```
gogrowatt-api/
  cmd/
    gogrowatt-api/
      main.go                  # Entrypoint: config, DB pool, wire handlers, start server
  internal/
    api/
      router.go                # Chi router setup + route registration
      handlers_health.go       # GET /api/v1/health
      handlers_plants.go       # Plant endpoints
      handlers_devices.go      # Device endpoints
      handlers_power.go        # Power reading endpoints (device + plant)
      handlers_energy.go       # Energy endpoints (device + plant)
      handlers_stats.go        # Stats endpoints (device + plant)
      middleware.go            # CORS, request logging, cache-control, request ID
      response.go              # Envelope types, writeJSON, writeError helpers
      params.go                # Query parameter parsing & validation
      params_test.go           # Unit tests for param parsing
      handlers_test.go         # Unit tests for handlers (mocked repo)
    repository/
      repository.go            # Interface definition + constructor
      plants.go                # Plant queries
      devices.go               # Device queries
      power.go                 # Power reading queries with aggregation
      energy.go                # Energy summary queries
      stats.go                 # Statistical aggregation queries
      health.go                # DB health + data freshness queries
      errors.go                # Sentinel errors (ErrNotFound, etc.)
    config/
      config.go                # Environment-based configuration
    testutil/
      server.go                # Test server factory
      seed.go                  # Test data seeding helpers
  Dockerfile
  go.mod
  go.sum
```

---

## 2. Configuration (`internal/config/config.go`)

```go
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration, populated from environment variables.
type Config struct {
	// Server
	Port         int           `json:"port"`
	ReadTimeout  time.Duration `json:"read_timeout"`
	WriteTimeout time.Duration `json:"write_timeout"`

	// Database
	DatabaseURL string `json:"-"` // never log
	DBMaxConns  int32  `json:"db_max_conns"`
	DBMinConns  int32  `json:"db_min_conns"`

	// CORS
	CORSOrigins []string `json:"cors_origins"`

	// Data freshness
	StaleThresholdSeconds int `json:"stale_threshold_seconds"`

	// Logging
	LogLevel slog.Level `json:"log_level"`

	// Version (set at build time)
	Version string `json:"version"`
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (*Config, error) {
	c := &Config{
		Port:                  envInt("API_PORT", 8080),
		ReadTimeout:           envDuration("API_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:          envDuration("API_WRITE_TIMEOUT", 30*time.Second),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		DBMaxConns:            int32(envInt("DB_MAX_CONNS", 10)),
		DBMinConns:            int32(envInt("DB_MIN_CONNS", 2)),
		CORSOrigins:          envStringSlice("CORS_ORIGINS", []string{"*"}),
		StaleThresholdSeconds: envInt("STALE_THRESHOLD", 900),
		LogLevel:              envLogLevel("LOG_LEVEL", slog.LevelInfo),
		Version:               envString("API_VERSION", "0.1.0"),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL environment variable is required")
	}

	return c, nil
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envStringSlice(key string, fallback []string) []string {
	if v := os.Getenv(key); v != "" {
		return strings.Split(v, ",")
	}
	return fallback
}

func envLogLevel(key string, fallback slog.Level) slog.Level {
	switch strings.ToLower(os.Getenv(key)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return fallback
	}
}
```

---

## 3. Main Entrypoint (`cmd/gogrowatt-api/main.go`)

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gogrowatt-api/internal/api"
	"gogrowatt-api/internal/config"
	"gogrowatt-api/internal/repository"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Structured logging
	logHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	})
	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	// Database connection pool
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to parse database URL", "error", err)
		os.Exit(1)
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		slog.Error("failed to create database pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Verify connectivity
	if err := pool.Ping(ctx); err != nil {
		slog.Error("failed to ping database", "error", err)
		os.Exit(1)
	}
	slog.Info("database connection established")

	// Wire up layers
	repo := repository.New(pool)
	handlers := api.NewHandlers(repo, cfg)
	router := api.NewRouter(handlers, cfg)

	// HTTP server
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("starting server", "port", cfg.Port, "version", cfg.Version)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	sig := <-shutdownCh
	slog.Info("shutdown signal received", "signal", sig)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped gracefully")
}
```

---

## 4. Response Envelope Types (`internal/api/response.go`)

```go
package api

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// Envelope is the standard success response wrapper.
type Envelope[T any] struct {
	Data       T           `json:"data"`
	Pagination *Pagination `json:"pagination,omitempty"`
	Meta       Meta        `json:"meta"`
}

// ErrorEnvelope is the standard error response wrapper.
type ErrorEnvelope struct {
	Error APIError `json:"error"`
	Meta  Meta     `json:"meta"`
}

// APIError describes a structured error.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Pagination describes offset-based pagination metadata.
type Pagination struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// Meta contains request-level metadata included in every response.
type Meta struct {
	Timestamp string `json:"timestamp"`
	RequestID string `json:"request_id"`
}

// newMeta builds a Meta from the current request context.
func newMeta(r *http.Request) Meta {
	return Meta{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		RequestID: middleware.GetReqID(r.Context()),
	}
}

// newPagination computes pagination metadata.
func newPagination(page, perPage, total int) *Pagination {
	if total == 0 {
		return &Pagination{
			Page:       page,
			PerPage:    perPage,
			Total:      0,
			TotalPages: 0,
		}
	}
	totalPages := int(math.Ceil(float64(total) / float64(perPage)))
	return &Pagination{
		Page:       page,
		PerPage:    perPage,
		Total:      total,
		TotalPages: totalPages,
	}
}

// writeJSON sends a success JSON response.
func writeJSON[T any](w http.ResponseWriter, r *http.Request, status int, envelope Envelope[T]) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	envelope.Meta = newMeta(r)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

// writeError sends an error JSON response.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	env := ErrorEnvelope{
		Error: APIError{
			Code:    code,
			Message: message,
		},
		Meta: newMeta(r),
	}
	if len(details) > 0 && details[0] != nil {
		env.Error.Details = details[0]
	}
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(env); err != nil {
		slog.Error("failed to encode error response", "error", err)
	}
}

// handleDBError maps database/context errors to appropriate HTTP error responses.
func handleDBError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case isContextDeadlineExceeded(err):
		writeError(w, r, http.StatusGatewayTimeout, "TIMEOUT", "Query exceeded deadline")
	case isContextCanceled(err):
		writeError(w, r, http.StatusGatewayTimeout, "TIMEOUT", "Request was cancelled")
	default:
		slog.Error("database error", "error", err,
			"request_id", middleware.GetReqID(r.Context()))
		writeError(w, r, http.StatusBadGateway, "DATABASE_ERROR", "Database query failed")
	}
}

func isContextDeadlineExceeded(err error) bool {
	return err != nil && err.Error() == "context deadline exceeded"
}

func isContextCanceled(err error) bool {
	return err != nil && err.Error() == "context canceled"
}
```

---

## 5. Repository Interface (`internal/repository/repository.go`)

```go
package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository defines all database access methods used by the API handlers.
type Repository interface {
	// Health
	Ping(ctx context.Context) (latencyMs int64, err error)
	LatestReadingTime(ctx context.Context) (*time.Time, error)

	// Plants
	ListPlants(ctx context.Context, page, perPage int) ([]Plant, int, error)
	GetPlant(ctx context.Context, plantID string) (*PlantDetail, error)

	// Devices
	ListPlantDevices(ctx context.Context, plantID string, page, perPage int) ([]Device, int, error)
	GetDevice(ctx context.Context, deviceSN string) (*DeviceDetail, error)

	// Power
	GetDevicePower(ctx context.Context, deviceSN string, p PowerParams) ([]PowerReading, int, error)
	GetDevicePowerLatest(ctx context.Context, deviceSN string) (*LatestReading, error)
	GetPlantPower(ctx context.Context, plantID string, p PowerParams) ([]PowerReading, int, error)

	// Energy
	GetDeviceEnergy(ctx context.Context, deviceSN string, p EnergyParams) (*EnergyResult, error)
	GetPlantEnergy(ctx context.Context, plantID string, p EnergyParams) (*EnergyResult, error)

	// Stats
	GetDeviceStats(ctx context.Context, deviceSN string, p StatsParams) (*StatsResult, error)
	GetPlantStats(ctx context.Context, plantID string, p StatsParams) (*StatsResult, error)
}

// pgxRepository implements Repository using pgxpool.
type pgxRepository struct {
	pool *pgxpool.Pool
}

// New creates a new Repository backed by the given pgxpool.
func New(pool *pgxpool.Pool) Repository {
	return &pgxRepository{pool: pool}
}
```

---

## 6. Repository Domain Types (`internal/repository/` - type definitions across files)

```go
package repository

import "time"

// --- Plants ---

type Plant struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Country        *string  `json:"country"`
	City           *string  `json:"city"`
	Latitude       *float64 `json:"latitude"`
	Longitude      *float64 `json:"longitude"`
	PeakPowerKW    *float64 `json:"peak_power_kw"`
	Status         int      `json:"status"`
	CurrentPowerW  *float64 `json:"current_power_w"`
	TodayEnergyKWH *float64 `json:"today_energy_kwh"`
	TotalEnergyKWH *float64 `json:"total_energy_kwh"`
	CreatedAt      string   `json:"created_at"`
	DeviceCount    int      `json:"device_count"`
}

type PlantDetail struct {
	Plant
	MonthEnergyKWH    *float64   `json:"month_energy_kwh"`
	YearEnergyKWH     *float64   `json:"year_energy_kwh"`
	PeakPowerTodayW   *float64   `json:"peak_power_today_w"`
	FormulaCoalKG     *float64   `json:"formula_coal_kg"`
	FormulaCO2KG      *float64   `json:"formula_co2_kg"`
	FormulaTrees      *int       `json:"formula_trees"`
	MoneySaved        *float64   `json:"money_saved"`
	MoneyUnit         *string    `json:"money_unit"`
	Devices           []Device   `json:"devices"`
}

// --- Devices ---

type Device struct {
	SerialNumber string  `json:"serial_number"`
	PlantID      string  `json:"plant_id"`
	Name         *string `json:"name"`
	Type         string  `json:"type"`
	Model        *string `json:"model"`
	Status       int     `json:"status"`
	LastUpdate   *string `json:"last_update"`
}

type DeviceDetail struct {
	Device
	Current *DeviceCurrentStatus `json:"current"`
}

type DeviceCurrentStatus struct {
	PacW          *float64 `json:"pac_w"`
	PpvW          *float64 `json:"ppv_w"`
	Vpv1V         *float64 `json:"vpv1_v"`
	Vpv2V         *float64 `json:"vpv2_v"`
	Ipv1A         *float64 `json:"ipv1_a"`
	Ipv2A         *float64 `json:"ipv2_a"`
	Vac1V         *float64 `json:"vac1_v"`
	Iac1A         *float64 `json:"iac1_a"`
	FrequencyHz   *float64 `json:"frequency_hz"`
	TemperatureC  *float64 `json:"temperature_c"`
	TodayEnergyKWH *float64 `json:"today_energy_kwh"`
	TotalEnergyKWH *float64 `json:"total_energy_kwh"`
}

// --- Power ---

// PowerParams holds validated query parameters for power endpoints.
type PowerParams struct {
	From     time.Time
	To       time.Time
	Interval string   // "5min", "15min", "1h", "1d"
	Fields   []string // e.g. ["pac_w", "vpv1_v"]
	Timezone string   // IANA timezone name
	Location *time.Location
	Page     int
	PerPage  int
}

// PowerReading represents a single data point or aggregated bucket.
// For raw (5min) queries, only scalar fields are populated.
// For aggregated queries (15min, 1h, 1d), AggField values are used.
type PowerReading struct {
	Time    string                   `json:"time"`
	// Raw scalar values (interval=5min only)
	PacW    *float64                 `json:"pac_w,omitempty"`
	PpvW    *float64                 `json:"ppv_w,omitempty"`
	Vpv1V   *float64                 `json:"vpv1_v,omitempty"`
	Vpv2V   *float64                 `json:"vpv2_v,omitempty"`
	Ipv1A   *float64                 `json:"ipv1_a,omitempty"`
	Ipv2A   *float64                 `json:"ipv2_a,omitempty"`
	Vac1V   *float64                 `json:"vac1_v,omitempty"`
	Iac1A   *float64                 `json:"iac1_a,omitempty"`
	// Aggregated values (interval=15min, 1h, 1d)
	AggPacW  *AggValue               `json:"pac_w_agg,omitempty"`
	AggPpvW  *AggValue               `json:"ppv_w_agg,omitempty"`
	AggVpv1V *AggValue               `json:"vpv1_v_agg,omitempty"`
	AggVpv2V *AggValue               `json:"vpv2_v_agg,omitempty"`
	AggIpv1A *AggValue               `json:"ipv1_a_agg,omitempty"`
	AggIpv2A *AggValue               `json:"ipv2_a_agg,omitempty"`
	AggVac1V *AggValue               `json:"vac1_v_agg,omitempty"`
	AggIac1A *AggValue               `json:"iac1_a_agg,omitempty"`
}

type AggValue struct {
	Avg     float64 `json:"avg"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Samples int     `json:"samples"`
}

// LatestReading represents the most recent power reading.
type LatestReading struct {
	SerialNumber string   `json:"serial_number"`
	Time         string   `json:"time"`
	PacW         *float64 `json:"pac_w"`
	PpvW         *float64 `json:"ppv_w"`
	Vpv1V        *float64 `json:"vpv1_v"`
	Vpv2V        *float64 `json:"vpv2_v"`
	Ipv1A        *float64 `json:"ipv1_a"`
	Ipv2A        *float64 `json:"ipv2_a"`
	Vac1V        *float64 `json:"vac1_v"`
	Iac1A        *float64 `json:"iac1_a"`
}

// --- Energy ---

type EnergyParams struct {
	From     time.Time
	To       time.Time
	Unit     string // "day" or "month"
	Timezone string
	Location *time.Location
}

type EnergyResult struct {
	Totals  []EnergyTotal  `json:"totals"`
	Summary *EnergySummary `json:"summary"`
}

type EnergyTotal struct {
	Date      string  `json:"date"`
	EnergyKWH float64 `json:"energy_kwh"`
}

type EnergySummary struct {
	TotalKWH      float64 `json:"total_kwh"`
	AverageKWH    float64 `json:"average_kwh"`
	MaxKWH        float64 `json:"max_kwh,omitempty"`
	MaxDate       string  `json:"max_date,omitempty"`
	MinKWH        float64 `json:"min_kwh,omitempty"`
	MinDate       string  `json:"min_date,omitempty"`
	DaysWithData  int     `json:"days_with_data,omitempty"`
	MonthsWithData int    `json:"months_with_data,omitempty"`
}

// --- Stats ---

type StatsParams struct {
	From     time.Time
	To       time.Time
	Field    string // "pac_w", "ppv_w", etc.
	Timezone string
	Location *time.Location
}

type StatsResult struct {
	DaysAnalyzed       int           `json:"days_analyzed"`
	TotalProductionKWH float64       `json:"total_production_kwh"`
	DailyAverageKWH    float64       `json:"daily_average_kwh"`
	PeakHour           int           `json:"peak_hour"`
	PeakPowerAvgW      float64       `json:"peak_power_avg_w"`
	ByHour             []HourlyStat  `json:"by_hour"`
}

type HourlyStat struct {
	Hour       int     `json:"hour"`
	MinW       float64 `json:"min_w"`
	MaxW       float64 `json:"max_w"`
	AvgW       float64 `json:"avg_w"`
	MedianW    float64 `json:"median_w"`
	StddevW    float64 `json:"stddev_w"`
	SampleDays int     `json:"sample_days"`
}
```
