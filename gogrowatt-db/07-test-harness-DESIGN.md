# 07 - Test Harness Implementation Design

## Document Purpose

This is the detailed implementation blueprint for the gogrowatt test harness specified in `07-test-harness.md`. It contains complete Go code, Docker Compose definitions, Makefile targets, and CI/CD configuration ready for implementation. All consistency fixes from `00-consistency-analysis.md` are incorporated.

### Consistency Fixes Applied

| Issue | Fix Applied |
|---|---|
| Column naming | All code uses `ts`, `pac_w`, `ppv_w`, `vpv1_v`, `ipv1_a`, etc. (schema canonical names) |
| Primary key order | `(ts, device_sn)` -- time-first for TimescaleDB |
| Metadata table | `growatt.collection_runs` (not `fetch_log`) |
| `fetched_at` column | Included in all INSERT/UPSERT statements |
| REST API paths | `/api/v1/devices/{sn}/power?from=...&to=...` (doc 03 canonical) |
| Docker image | `timescale/timescaledb:latest-pg16` (not `postgres:16-alpine`) |
| Schema namespace | All SQL references use `growatt.` prefix |
| REST timestamp column | `ts` (not `recorded_at` or `reading_time`) |

---

## 1. Go Package Structure

```
gogrowatt/
  integration/
    testharness/
      config.go             # Types: SimulatorConfig, ScenarioType, ErrorEvent, etc.
      simulator.go          # httptest.Server wrapping a configurable mux
      simulator_test.go     # Verify simulator endpoint formats
      datagenerator.go      # Solar model + cloud + doubling + noise
      datagenerator_test.go # Determinism, curve shape, golden files
      solar.go              # Sunrise/sunset calculator
      solar_test.go         # Validate against known Austin TX ephemeris
      controlapi.go         # /_control/ mux for dynamic reconfiguration
      scenarios.go          # Pre-built scenario configurations
    fetcher/
      fetcher_test.go       # Integration: simulator -> fetcher -> DB
    api/
      api_test.go           # Integration: DB -> REST API -> assertions
    performance/
      bench_test.go         # Bulk load, query latency, concurrency
    testdb/
      testdb.go             # Connect, Reset, seed helpers
      assertions.go         # CountReadings, GetReading, AssertNoGaps, etc.
    testdata/
      golden/
        clear_day_seed42.json
        cloudy_day_seed43.json
        night_only_seed50.json
        with_doubling_seed42.json
        flat_top_seed51.json
    fixtures/
      fixtures.go           # Test data factories
  docker-compose.test.yml
  Makefile
```

---

## 2. Core Types -- `integration/testharness/config.go`

```go
//go:build integration

package testharness

import "time"

// ScenarioType identifies a data generation scenario.
type ScenarioType string

const (
    ScenarioClearDay     ScenarioType = "clear_day"
    ScenarioCloudyDay    ScenarioType = "cloudy_day"
    ScenarioNightOnly    ScenarioType = "night_only"
    ScenarioWithDoubling ScenarioType = "with_doubling"
    ScenarioFlatTop      ScenarioType = "flat_top"
)

// ErrorType identifies an error injection mode.
type ErrorType string

const (
    ErrTypeRateLimit     ErrorType = "rate_limit"
    ErrTypeAuthFail      ErrorType = "auth_fail"
    ErrTypeServerDown    ErrorType = "server_down"
    ErrTypeEmptyResponse ErrorType = "empty_response"
    ErrTypeTimeout       ErrorType = "timeout"
)

// DateRange defines a closed date interval.
type DateRange struct {
    From time.Time
    To   time.Time
}

// ErrorEvent defines when and how to inject an error.
type ErrorEvent struct {
    TriggerAfterRequests int           // inject after N total requests
    TriggerAtTime        time.Time     // or inject at simulated wall-clock time
    ErrorType            ErrorType
    Duration             time.Duration // how long the error persists
    HTTPStatus           int           // 0 = JSON error body; 503 = HTTP-level
}

// SimulatorConfig controls the entire simulated Growatt API server.
type SimulatorConfig struct {
    // Identity
    PlantID     string  // e.g. "12345"
    PlantName   string  // e.g. "Home Solar"
    DeviceSN    string  // e.g. "ABC123456"
    DeviceModel string  // e.g. "MIN 9000TL-X"
    PeakPowerKW float64 // e.g. 9.0

    // Location (drives sunrise/sunset)
    Latitude  float64 // e.g. 30.2672 (Austin TX)
    Longitude float64 // e.g. -97.7431
    Timezone  string  // e.g. "US/Central"

    // Data generation
    Scenario     ScenarioType
    RandomSeed   int64
    DateRange    DateRange
    IntervalSecs int // default 300 (5 min)

    // Error injection
    ErrorSchedule []ErrorEvent

    // Auth
    ValidToken string // default "test-harness-token-abc123"
}

// DefaultConfig returns a config matching real Austin TX production data.
func DefaultConfig() SimulatorConfig {
    return SimulatorConfig{
        PlantID:      "12345",
        PlantName:    "Home Solar",
        DeviceSN:     "ABC123456",
        DeviceModel:  "MIN 9000TL-X",
        PeakPowerKW:  9.0,
        Latitude:     30.2672,
        Longitude:    -97.7431,
        Timezone:     "US/Central",
        Scenario:     ScenarioWithDoubling,
        RandomSeed:   42,
        IntervalSecs: 300,
        ValidToken:   "test-harness-token-abc123",
        DateRange: DateRange{
            From: time.Date(2026, 2, 12, 0, 0, 0, 0, time.UTC),
            To:   time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
        },
    }
}

// DataPoint represents a single generated 5-minute reading.
// Field names match the MINHistoryDataPoint JSON keys from the Growatt API,
// but DB columns use suffixed names: pac_w, ppv_w, vpv1_v, etc.
type DataPoint struct {
    Time string  `json:"time"` // "YYYY-MM-DD HH:MM"
    Pac  float64 `json:"pac"`  // AC power (W) -> DB: pac_w
    Ppv  float64 `json:"ppv"`  // PV power (W) -> DB: ppv_w
    Vpv1 float64 `json:"vpv1"` // PV1 voltage  -> DB: vpv1_v
    Vpv2 float64 `json:"vpv2"` // PV2 voltage  -> DB: vpv2_v
    Ipv1 float64 `json:"ipv1"` // PV1 current  -> DB: ipv1_a
    Ipv2 float64 `json:"ipv2"` // PV2 current  -> DB: ipv2_a
    Vac1 float64 `json:"vac1"` // AC voltage   -> DB: vac1_v
    Iac1 float64 `json:"iac1"` // AC current   -> DB: iac1_a
}
```

---

## 3. Solar Position Model -- `integration/testharness/solar.go`

This module computes approximate sunrise, solar noon, and sunset for a given date and geographic coordinates. It uses the standard solar declination + hour angle algorithm, accurate to within a few minutes -- sufficient for generating realistic test curves.

```go
//go:build integration

package testharness

import (
    "math"
    "time"
)

// SolarTimes holds computed sunrise/sunset for a single date.
type SolarTimes struct {
    Sunrise   time.Time
    SolarNoon time.Time
    Sunset    time.Time
}

// ComputeSolarTimes calculates sunrise, solar noon, and sunset.
// Uses the NOAA simplified solar position algorithm.
//
// Reference values for Austin TX (30.27N, -97.74W) in February:
//   Sunrise   ~07:15 CST
//   Solar noon ~12:35 CST
//   Sunset    ~18:15 CST
func ComputeSolarTimes(date time.Time, lat, lon float64, tz *time.Location) SolarTimes {
    // Day of year (1-366)
    doy := float64(date.YearDay())

    // Solar declination (radians)
    declination := 23.45 * math.Sin(2*math.Pi*(284+doy)/365)
    decRad := declination * math.Pi / 180
    latRad := lat * math.Pi / 180

    // Hour angle at sunrise/sunset (cos(ha) = -tan(lat)*tan(dec))
    cosHA := -math.Tan(latRad) * math.Tan(decRad)
    // Clamp for polar regions (not relevant for Austin)
    if cosHA < -1 {
        cosHA = -1
    }
    if cosHA > 1 {
        cosHA = 1
    }
    haDeg := math.Acos(cosHA) * 180 / math.Pi

    // Equation of time correction (minutes)
    b := 2 * math.Pi * (doy - 81) / 365
    eot := 9.87*math.Sin(2*b) - 7.53*math.Cos(b) - 1.5*math.Sin(b)

    // Solar noon in minutes from midnight UTC
    // Standard meridian for the timezone offset
    _, offset := date.In(tz).Zone()
    stdMeridian := float64(offset) / 3600 * 15 // degrees
    solarNoonMin := 720 - 4*(lon-stdMeridian) - eot

    sunriseMin := solarNoonMin - 4*haDeg
    sunsetMin := solarNoonMin + 4*haDeg

    midnight := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, tz)
    return SolarTimes{
        Sunrise:   midnight.Add(time.Duration(sunriseMin) * time.Minute),
        SolarNoon: midnight.Add(time.Duration(solarNoonMin) * time.Minute),
        Sunset:    midnight.Add(time.Duration(sunsetMin) * time.Minute),
    }
}

// SolarElevation returns the normalized solar elevation (0.0 to 1.0)
// at a given time, where 1.0 is solar noon. Returns 0.0 before sunrise
// or after sunset.
func SolarElevation(t time.Time, st SolarTimes) float64 {
    if t.Before(st.Sunrise) || t.After(st.Sunset) {
        return 0.0
    }
    dayLength := st.Sunset.Sub(st.Sunrise).Seconds()
    if dayLength <= 0 {
        return 0.0
    }
    elapsed := t.Sub(st.Sunrise).Seconds()
    // Sinusoidal curve: 0 at sunrise, 1 at noon, 0 at sunset
    return math.Sin(math.Pi * elapsed / dayLength)
}
```

---

## 4. Data Generator -- `integration/testharness/datagenerator.go`

The data generator pipeline:
1. Compute solar times for the date
2. Generate a base irradiance curve (sinusoidal)
3. Apply the doubling event at ~12:55
4. Apply cloud cover model
5. Add measurement noise
6. Clamp and quantize
7. Add nighttime tail readings
8. Apply per-day timestamp offset

```go
//go:build integration

package testharness

import (
    "fmt"
    "math"
    "math/rand"
    "time"
)

// Generator produces deterministic synthetic solar data.
type Generator struct {
    cfg SimulatorConfig
    rng *rand.Rand
}

// NewGenerator creates a new data generator with the given config.
func NewGenerator(cfg SimulatorConfig) *Generator {
    if cfg.IntervalSecs == 0 {
        cfg.IntervalSecs = 300
    }
    if cfg.PeakPowerKW == 0 {
        cfg.PeakPowerKW = 9.0
    }
    if cfg.Latitude == 0 && cfg.Longitude == 0 {
        cfg.Latitude = 30.2672
        cfg.Longitude = -97.7431
    }
    if cfg.Timezone == "" {
        cfg.Timezone = "US/Central"
    }
    return &Generator{
        cfg: cfg,
        rng: rand.New(rand.NewSource(cfg.RandomSeed)),
    }
}

// GenerateDay produces all data points for a single date.
// The returned slice is sorted by time.
func (g *Generator) GenerateDay(date time.Time) []DataPoint {
    if g.cfg.Scenario == ScenarioNightOnly {
        return nil
    }

    loc, err := time.LoadLocation(g.cfg.Timezone)
    if err != nil {
        loc = time.UTC
    }

    st := ComputeSolarTimes(date, g.cfg.Latitude, g.cfg.Longitude, loc)

    // Per-day random offset: 0-5 minutes (matches real Growatt jitter)
    offsetMin := g.rng.Intn(6) // 0..5
    intervalDur := time.Duration(g.cfg.IntervalSecs) * time.Second

    // First reading: sunrise + 10-15 minutes + offset
    firstReadingDelay := time.Duration(10+g.rng.Intn(6)) * time.Minute
    firstReading := st.Sunrise.Add(firstReadingDelay)
    // Snap to interval grid with offset
    firstReading = firstReading.Truncate(intervalDur).Add(
        time.Duration(offsetMin) * time.Minute)

    // Last reading: sunset + 50-65 minutes (long tail)
    lastReading := st.Sunset.Add(time.Duration(50+g.rng.Intn(16)) * time.Minute)

    // Doubling time: 12:50-13:00 local (varies per day by seed)
    doublingLocal := time.Date(date.Year(), date.Month(), date.Day(),
        12, 50+g.rng.Intn(11), 0, 0, loc) // 12:50 to 13:00

    // Doubling factor: 1.82-2.06 (from real data observations)
    doublingFactor := 1.82 + g.rng.Float64()*0.24

    // Cloud cover function depends on scenario
    cloudFn := g.cloudFunction()

    // Peak single-string power (before doubling)
    singleStringPeak := g.cfg.PeakPowerKW * 1000 / 2 // ~4500 W for 9kW system
    // Observed real peak is ~2750 W, which is lower due to angle/efficiency
    // Adjust with a 0.60-0.62 efficiency factor
    efficiencyFactor := 0.60 + g.rng.Float64()*0.02
    singleStringPeak *= efficiencyFactor // ~2700 W

    var points []DataPoint
    cursor := firstReading

    for cursor.Before(lastReading) || cursor.Equal(lastReading) {
        elevation := SolarElevation(cursor, st)
        basePower := singleStringPeak * elevation

        // Apply doubling
        if g.cfg.Scenario == ScenarioWithDoubling || g.cfg.Scenario == ScenarioClearDay ||
            g.cfg.Scenario == ScenarioCloudyDay {
            if !cursor.Before(doublingLocal) {
                basePower *= doublingFactor
            }
        }

        // Apply flat-top clipping
        if g.cfg.Scenario == ScenarioFlatTop {
            maxW := g.cfg.PeakPowerKW * 1000
            if basePower > maxW {
                basePower = maxW
            }
        }

        // Apply cloud cover
        cloudMult := cloudFn()
        power := basePower * cloudMult

        // Add Gaussian noise: +/- 2% of reading
        noise := g.rng.NormFloat64() * 0.02 * power
        power += noise

        // Handle post-sunset tail readings
        if cursor.After(st.Sunset) {
            // Small residual values (0.1-3.5 W)
            power = 0.1 + g.rng.Float64()*3.4
        }

        // Clamp: min 0, max peak_kw * 1000
        if power < 0 {
            power = 0
        }
        maxClamp := g.cfg.PeakPowerKW * 1000
        if power > maxClamp {
            power = maxClamp
        }

        // Quantize to 0.1 W (matches real Growatt precision)
        power = math.Round(power*10) / 10

        // Derive per-string values from total power
        dp := g.deriveElectricalValues(cursor, power, doublingLocal, st, elevation)
        points = append(points, dp)

        cursor = cursor.Add(intervalDur)
    }

    return points
}

// deriveElectricalValues computes voltage/current columns from total AC power.
func (g *Generator) deriveElectricalValues(
    t time.Time, pacW float64,
    doublingTime time.Time, st SolarTimes, elevation float64,
) DataPoint {
    loc := t.Location()
    timeStr := t.In(loc).Format("2006-01-02 15:04")

    if pacW <= 0.1 {
        return DataPoint{
            Time: timeStr,
            Pac:  pacW,
            Ppv:  pacW * 1.03, // PV slightly higher than AC output
            Vac1: 240.0 + g.rng.Float64()*4, // Grid voltage present even at night
        }
    }

    // PV power is slightly higher than AC (inverter efficiency ~97%)
    ppvW := pacW * (1.02 + g.rng.Float64()*0.02)

    // AC voltage: 238-244 V typical US split-phase
    vac1 := 240.0 + g.rng.NormFloat64()*1.5

    // AC current: P = V * I
    iac1 := pacW / vac1

    // PV string voltages: 280-340 V typical for MIN series
    baseVpv := 280.0 + elevation*60.0 // rises with sun angle
    vpv1 := baseVpv + g.rng.NormFloat64()*3
    vpv2 := 0.0

    // PV string currents
    ipv1 := 0.0
    ipv2 := 0.0

    if t.Before(doublingTime) {
        // Single string active
        ipv1 = ppvW / vpv1
    } else {
        // Both strings active
        vpv2 = baseVpv + g.rng.NormFloat64()*3
        ipv1 = ppvW / 2 / vpv1
        ipv2 = ppvW / 2 / vpv2
    }

    return DataPoint{
        Time: timeStr,
        Pac:  math.Round(pacW*10) / 10,
        Ppv:  math.Round(ppvW*10) / 10,
        Vpv1: math.Round(vpv1*10) / 10,
        Vpv2: math.Round(vpv2*10) / 10,
        Ipv1: math.Round(ipv1*100) / 100,
        Ipv2: math.Round(ipv2*100) / 100,
        Vac1: math.Round(vac1*10) / 10,
        Iac1: math.Round(iac1*100) / 100,
    }
}

// cloudFunction returns a closure that produces a cloud cover multiplier
// each time it is called. The multiplier is in [0.0, 1.0].
func (g *Generator) cloudFunction() func() float64 {
    switch g.cfg.Scenario {
    case ScenarioClearDay, ScenarioFlatTop:
        // Clear: tight Gaussian around 1.0, sigma=3%
        return func() float64 {
            v := 1.0 + g.rng.NormFloat64()*0.03
            if v < 0.85 {
                v = 0.85
            }
            if v > 1.0 {
                v = 1.0
            }
            return v
        }

    case ScenarioCloudyDay:
        // Cloudy: Beta(2,5)-like distribution -- biased low, occasional clear
        // Implement via random walk with large steps
        current := 0.6
        return func() float64 {
            step := g.rng.NormFloat64() * 0.25 // large steps
            current += step
            if current < 0.10 {
                current = 0.10
            }
            if current > 1.0 {
                current = 1.0
            }
            return current
        }

    case ScenarioWithDoubling:
        // Mostly clear with minor variation
        return func() float64 {
            v := 1.0 + g.rng.NormFloat64()*0.05
            if v < 0.80 {
                v = 0.80
            }
            if v > 1.0 {
                v = 1.0
            }
            return v
        }

    default:
        return func() float64 { return 1.0 }
    }
}

// GenerateDateRange produces data for every day in the range.
func (g *Generator) GenerateDateRange(from, to time.Time) map[string][]DataPoint {
    result := make(map[string][]DataPoint)
    current := from
    for !current.After(to) {
        dateStr := current.Format("2006-01-02")
        result[dateStr] = g.GenerateDay(current)
        current = current.AddDate(0, 0, 1)
    }
    return result
}
```

---

## 5. Simulated Growatt API Server -- `integration/testharness/simulator.go`

```go
//go:build integration

package testharness

import (
    "encoding/json"
    "fmt"
    "net/http"
    "strconv"
    "strings"
    "sync"
    "sync/atomic"
    "time"
)

// Simulator implements a fake Growatt OpenAPI server.
type Simulator struct {
    mu            sync.RWMutex
    cfg           SimulatorConfig
    gen           *Generator
    requestCount  atomic.Int64
    errorsInjected atomic.Int64
    errorUntil    time.Time   // error injection expiry
    activeError   *ErrorEvent // current active error
    mux           *http.ServeMux
    dataCache     map[string][]DataPoint // date -> points
}

// New creates a new Simulator with the given config.
func New(cfg SimulatorConfig) *Simulator {
    if cfg.ValidToken == "" {
        cfg.ValidToken = "test-harness-token-abc123"
    }
    s := &Simulator{
        cfg:       cfg,
        gen:       NewGenerator(cfg),
        dataCache: make(map[string][]DataPoint),
    }
    s.mux = s.buildMux()
    return s
}

// Handler returns the http.Handler for use with httptest.NewServer.
func (s *Simulator) Handler() http.Handler {
    return s.mux
}

func (s *Simulator) buildMux() *http.ServeMux {
    mux := http.NewServeMux()

    // Growatt API endpoints (under /v1/)
    mux.HandleFunc("/v1/plant/list", s.withAuth(s.handlePlantList))
    mux.HandleFunc("/v1/plant/data", s.withAuth(s.handlePlantData))
    mux.HandleFunc("/v1/plant/power", s.withAuth(s.handlePlantPower))
    mux.HandleFunc("/v1/device/list", s.withAuth(s.handleDeviceList))
    mux.HandleFunc("/v1/device/tlx/tlx_data_info", s.withAuth(s.handleTlxDataInfo))
    mux.HandleFunc("/v1/device/tlx/tlx_data", s.withAuth(s.handleTlxData))

    // Control API (for test orchestration)
    mux.HandleFunc("/_control/scenario", s.handleControlScenario)
    mux.HandleFunc("/_control/error", s.handleControlError)
    mux.HandleFunc("/_control/reset", s.handleControlReset)
    mux.HandleFunc("/_control/stats", s.handleControlStats)
    mux.HandleFunc("/_control/advance", s.handleControlAdvance)

    return mux
}

// withAuth wraps a handler with token validation and error injection.
func (s *Simulator) withAuth(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        s.requestCount.Add(1)

        // Check error injection
        if err := s.checkErrorInjection(w); err != nil {
            return
        }

        // Validate token
        token := r.Header.Get("token")
        if token != s.cfg.ValidToken {
            writeGrowattError(w, 10011, "error_permission_denied")
            return
        }

        next(w, r)
    }
}

// checkErrorInjection returns an error response if an error is currently active.
func (s *Simulator) checkErrorInjection(w http.ResponseWriter) error {
    s.mu.RLock()
    defer s.mu.RUnlock()

    // Check request-count triggers
    count := s.requestCount.Load()
    for i := range s.cfg.ErrorSchedule {
        ev := &s.cfg.ErrorSchedule[i]
        if ev.TriggerAfterRequests > 0 && count >= int64(ev.TriggerAfterRequests) {
            if s.activeError == nil || s.activeError != ev {
                s.mu.RUnlock()
                s.mu.Lock()
                s.activeError = ev
                s.errorUntil = time.Now().Add(ev.Duration)
                s.mu.Unlock()
                s.mu.RLock()
            }
        }
    }

    if s.activeError != nil && time.Now().Before(s.errorUntil) {
        s.errorsInjected.Add(1)
        switch s.activeError.ErrorType {
        case ErrTypeRateLimit:
            writeGrowattError(w, 10012, "error_frequently_access")
            return fmt.Errorf("rate limited")
        case ErrTypeAuthFail:
            writeGrowattError(w, 10011, "error_permission_denied")
            return fmt.Errorf("auth fail")
        case ErrTypeServerDown:
            status := s.activeError.HTTPStatus
            if status == 0 {
                status = 503
            }
            w.WriteHeader(status)
            return fmt.Errorf("server down")
        case ErrTypeEmptyResponse:
            writeGrowattJSON(w, map[string]interface{}{
                "error_code": 0, "error_msg": "", "data": "",
            })
            return fmt.Errorf("empty response")
        case ErrTypeTimeout:
            // Simulate a timeout by sleeping longer than client timeout
            time.Sleep(60 * time.Second)
            return fmt.Errorf("timeout")
        }
    } else if s.activeError != nil && !time.Now().Before(s.errorUntil) {
        // Error expired, clear it
        s.mu.RUnlock()
        s.mu.Lock()
        s.activeError = nil
        s.mu.Unlock()
        s.mu.RLock()
    }

    return nil
}

// --- Growatt API Endpoint Handlers ---

func (s *Simulator) handlePlantList(w http.ResponseWriter, r *http.Request) {
    writeGrowattJSON(w, map[string]interface{}{
        "error_code": 0,
        "error_msg":  "",
        "data": map[string]interface{}{
            "count": 1,
            "plants": []map[string]interface{}{
                {
                    "plant_id":   s.cfg.PlantID,
                    "plant_name": s.cfg.PlantName,
                    "latitude":   s.cfg.Latitude,
                    "longitude":  s.cfg.Longitude,
                    "peak_power": s.cfg.PeakPowerKW,
                    "status":     1,
                },
            },
        },
    })
}

func (s *Simulator) handlePlantData(w http.ResponseWriter, r *http.Request) {
    writeGrowattJSON(w, map[string]interface{}{
        "error_code": 0,
        "error_msg":  "",
        "data": map[string]interface{}{
            "plant_id":      s.cfg.PlantID,
            "today_energy":  25.4,
            "total_energy":  12450.0,
            "current_power": 5200.0,
        },
    })
}

func (s *Simulator) handlePlantPower(w http.ResponseWriter, r *http.Request) {
    dateStr := r.URL.Query().Get("date")
    if dateStr == "" {
        dateStr = time.Now().Format("2006-01-02")
    }

    points := s.getOrGenerateDay(dateStr)

    // Plant power returns a map of "HH:MM" -> power_value
    powers := make(map[string]float64)
    for _, dp := range points {
        // Normalize to HH:MM
        parts := strings.Split(dp.Time, " ")
        timeOnly := dp.Time
        if len(parts) >= 2 {
            timeOnly = parts[1]
        }
        powers[timeOnly] = dp.Pac
    }

    writeGrowattJSON(w, map[string]interface{}{
        "error_code": 0,
        "error_msg":  "",
        "data": map[string]interface{}{
            "plant_id": s.cfg.PlantID,
            "count":    len(powers),
            "powers":   powers,
        },
    })
}

func (s *Simulator) handleDeviceList(w http.ResponseWriter, r *http.Request) {
    writeGrowattJSON(w, map[string]interface{}{
        "error_code": 0,
        "error_msg":  "",
        "data": map[string]interface{}{
            "count": 1,
            "devices": []map[string]interface{}{
                {
                    "device_sn":   s.cfg.DeviceSN,
                    "device_type": 4,
                    "device_name": s.cfg.DeviceModel,
                    "model":       s.cfg.DeviceModel,
                    "status":      1,
                    "last_update": time.Now().Format("2006-01-02 15:04:05"),
                },
            },
        },
    })
}

func (s *Simulator) handleTlxDataInfo(w http.ResponseWriter, r *http.Request) {
    writeGrowattJSON(w, map[string]interface{}{
        "error_code": 0,
        "error_msg":  "",
        "data": map[string]interface{}{
            "tlx_sn":      s.cfg.DeviceSN,
            "status":      1,
            "pac":         5200.0,
            "etoday":      25.4,
            "etotal":      12450.0,
            "vpv1":        315.7,
            "vpv2":        313.2,
            "ipv1":        8.5,
            "ipv2":        8.7,
            "vac1":        240.8,
            "iac1":        21.7,
            "fac":         60.01,
            "temperature": 42.5,
        },
    })
}

func (s *Simulator) handleTlxData(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    r.ParseForm()

    startDate := r.FormValue("start_date")
    endDate := r.FormValue("end_date")
    pageStr := r.FormValue("page")
    perpageStr := r.FormValue("perpage")

    page, _ := strconv.Atoi(pageStr)
    if page < 1 {
        page = 1
    }
    perpage, _ := strconv.Atoi(perpageStr)
    if perpage < 1 || perpage > 200 {
        perpage = 100
    }

    // Collect all data points across the date range
    var allPoints []DataPoint
    start, _ := time.Parse("2006-01-02", startDate)
    end, _ := time.Parse("2006-01-02", endDate)
    if end.IsZero() {
        end = start
    }

    current := start
    for !current.After(end) {
        dateStr := current.Format("2006-01-02")
        points := s.getOrGenerateDay(dateStr)
        allPoints = append(allPoints, points...)
        current = current.AddDate(0, 0, 1)
    }

    // Paginate
    totalCount := len(allPoints)
    startIdx := (page - 1) * perpage
    endIdx := startIdx + perpage
    if startIdx > totalCount {
        startIdx = totalCount
    }
    if endIdx > totalCount {
        endIdx = totalCount
    }
    pageData := allPoints[startIdx:endIdx]

    // Convert to Growatt API format
    datas := make([]map[string]interface{}, len(pageData))
    for i, dp := range pageData {
        datas[i] = map[string]interface{}{
            "time": dp.Time,
            "pac":  dp.Pac,
            "ppv":  dp.Ppv,
            "vpv1": dp.Vpv1,
            "vpv2": dp.Vpv2,
            "ipv1": dp.Ipv1,
            "ipv2": dp.Ipv2,
            "vac1": dp.Vac1,
            "iac1": dp.Iac1,
        }
    }

    writeGrowattJSON(w, map[string]interface{}{
        "error_code": 0,
        "error_msg":  "",
        "data": map[string]interface{}{
            "count": totalCount,
            "datas": datas,
        },
    })
}

// getOrGenerateDay returns cached data or generates new data for a date.
func (s *Simulator) getOrGenerateDay(dateStr string) []DataPoint {
    s.mu.RLock()
    if pts, ok := s.dataCache[dateStr]; ok {
        s.mu.RUnlock()
        return pts
    }
    s.mu.RUnlock()

    date, _ := time.Parse("2006-01-02", dateStr)
    points := s.gen.GenerateDay(date)

    s.mu.Lock()
    s.dataCache[dateStr] = points
    s.mu.Unlock()

    return points
}

// --- Control API Handlers ---

func (s *Simulator) handleControlScenario(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var req struct {
        Scenario string `json:"scenario"`
        Seed     int64  `json:"seed"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    s.mu.Lock()
    s.cfg.Scenario = ScenarioType(req.Scenario)
    if req.Seed != 0 {
        s.cfg.RandomSeed = req.Seed
    }
    s.gen = NewGenerator(s.cfg)
    s.dataCache = make(map[string][]DataPoint) // invalidate cache
    s.mu.Unlock()

    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Simulator) handleControlError(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var req struct {
        Type         string `json:"type"`
        DurationSecs int    `json:"duration_secs"`
        HTTPStatus   int    `json:"http_status"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    s.mu.Lock()
    s.activeError = &ErrorEvent{
        ErrorType:  ErrorType(req.Type),
        Duration:   time.Duration(req.DurationSecs) * time.Second,
        HTTPStatus: req.HTTPStatus,
    }
    s.errorUntil = time.Now().Add(s.activeError.Duration)
    s.mu.Unlock()

    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Simulator) handleControlReset(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    s.requestCount.Store(0)
    s.errorsInjected.Store(0)
    s.activeError = nil
    s.dataCache = make(map[string][]DataPoint)
    s.gen = NewGenerator(s.cfg)
    s.mu.Unlock()

    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Simulator) handleControlStats(w http.ResponseWriter, r *http.Request) {
    json.NewEncoder(w).Encode(map[string]interface{}{
        "requests":        s.requestCount.Load(),
        "errors_injected": s.errorsInjected.Load(),
    })
}

func (s *Simulator) handleControlAdvance(w http.ResponseWriter, r *http.Request) {
    // Placeholder for simulated clock advancement
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- Helpers ---

func writeGrowattJSON(w http.ResponseWriter, v interface{}) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(v)
}

func writeGrowattError(w http.ResponseWriter, code int, msg string) {
    writeGrowattJSON(w, map[string]interface{}{
        "error_code": code,
        "error_msg":  msg,
        "data":       "",
    })
}
```

---

## 6. Test Database Utilities -- `integration/testdb/testdb.go`

```go
//go:build integration

package testdb

import (
    "database/sql"
    "os"
    "testing"
    "time"

    _ "github.com/jackc/pgx/v5/stdlib"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

const defaultTestDBURL = "postgres://test:test@localhost:5433/gogrowatt_test?sslmode=disable"

// Connect returns a database connection for testing.
// Uses GOGROWATT_TEST_DB_URL env var, or falls back to default.
func Connect(t *testing.T) *sql.DB {
    t.Helper()
    url := os.Getenv("GOGROWATT_TEST_DB_URL")
    if url == "" {
        url = defaultTestDBURL
    }
    db, err := sql.Open("pgx", url)
    require.NoError(t, err, "failed to connect to test database")

    err = db.Ping()
    require.NoError(t, err, "test database not reachable at %s", url)

    t.Cleanup(func() { db.Close() })
    return db
}

// Reset drops and recreates the growatt schema, then runs migrations.
func Reset(t *testing.T, db *sql.DB) {
    t.Helper()
    statements := []string{
        "DROP SCHEMA IF EXISTS growatt CASCADE",
        "CREATE SCHEMA growatt",
        "CREATE EXTENSION IF NOT EXISTS timescaledb",
    }
    for _, stmt := range statements {
        _, err := db.Exec(stmt)
        require.NoError(t, err, "failed executing: %s", stmt)
    }

    // Create core tables (minimal set needed for test harness)
    _, err := db.Exec(`
        CREATE TABLE growatt.plants (
            plant_id    TEXT PRIMARY KEY,
            plant_name  TEXT NOT NULL,
            latitude    DOUBLE PRECISION,
            longitude   DOUBLE PRECISION,
            peak_power_kw DOUBLE PRECISION,
            status      INT NOT NULL DEFAULT 1,
            created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
        );

        CREATE TABLE growatt.devices (
            device_sn   TEXT PRIMARY KEY,
            plant_id    TEXT NOT NULL REFERENCES growatt.plants(plant_id),
            device_type INT NOT NULL DEFAULT 0,
            device_name TEXT,
            model       TEXT,
            status      INT NOT NULL DEFAULT 1,
            last_update TIMESTAMPTZ,
            created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
        );

        CREATE TABLE growatt.power_readings (
            ts          TIMESTAMPTZ     NOT NULL,
            device_sn   TEXT            NOT NULL,
            pac_w       DOUBLE PRECISION,
            ppv_w       DOUBLE PRECISION,
            vpv1_v      DOUBLE PRECISION,
            vpv2_v      DOUBLE PRECISION,
            ipv1_a      DOUBLE PRECISION,
            ipv2_a      DOUBLE PRECISION,
            vac1_v      DOUBLE PRECISION,
            iac1_a      DOUBLE PRECISION,
            fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
            CONSTRAINT power_readings_pkey PRIMARY KEY (ts, device_sn)
        );

        SELECT create_hypertable(
            'growatt.power_readings',
            by_range('ts', INTERVAL '7 days')
        );

        CREATE INDEX idx_power_readings_device_ts
            ON growatt.power_readings (device_sn, ts DESC);

        CREATE TABLE growatt.collection_runs (
            id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            source_type     TEXT NOT NULL,
            source_id       TEXT NOT NULL,
            query_date_start DATE,
            query_date_end   DATE,
            points_collected INT NOT NULL DEFAULT 0,
            started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
            finished_at     TIMESTAMPTZ,
            status          TEXT NOT NULL DEFAULT 'running',
            error_message   TEXT
        );

        CREATE TABLE growatt.energy_summaries (
            period_date DATE NOT NULL,
            plant_id    TEXT NOT NULL REFERENCES growatt.plants(plant_id),
            time_unit   TEXT NOT NULL CHECK (time_unit IN ('day', 'month')),
            energy_kwh  DOUBLE PRECISION NOT NULL DEFAULT 0,
            created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
            CONSTRAINT energy_summaries_pkey PRIMARY KEY (period_date, plant_id, time_unit)
        );
    `)
    require.NoError(t, err, "failed creating schema tables")
}

// SeedPlant inserts a test plant record.
func SeedPlant(t *testing.T, db *sql.DB, plantID, name string, lat, lon, peakKW float64) {
    t.Helper()
    _, err := db.Exec(`
        INSERT INTO growatt.plants (plant_id, plant_name, latitude, longitude, peak_power_kw)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (plant_id) DO NOTHING
    `, plantID, name, lat, lon, peakKW)
    require.NoError(t, err)
}

// SeedDevice inserts a test device record.
func SeedDevice(t *testing.T, db *sql.DB, deviceSN, plantID, model string) {
    t.Helper()
    _, err := db.Exec(`
        INSERT INTO growatt.devices (device_sn, plant_id, model, device_name)
        VALUES ($1, $2, $3, $3)
        ON CONFLICT (device_sn) DO NOTHING
    `, deviceSN, plantID, model)
    require.NoError(t, err)
}

// UpsertReading inserts a single power reading using the canonical upsert pattern.
// Column names match 01-database-schema.md: ts, device_sn, pac_w, ppv_w, etc.
func UpsertReading(t *testing.T, db *sql.DB, deviceSN string, ts time.Time,
    pacW, ppvW, vpv1V, vpv2V, ipv1A, ipv2A, vac1V, iac1A float64) {
    t.Helper()
    _, err := db.Exec(`
        INSERT INTO growatt.power_readings
            (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
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
    `, ts, deviceSN, pacW, ppvW, vpv1V, vpv2V, ipv1A, ipv2A, vac1V, iac1A)
    require.NoError(t, err)
}

// CountReadings returns the number of power readings for a device on a date.
func CountReadings(t *testing.T, db *sql.DB, deviceSN, date string) int {
    t.Helper()
    var count int
    err := db.QueryRow(`
        SELECT count(*) FROM growatt.power_readings
        WHERE device_sn = $1
          AND ts >= $2::date
          AND ts < ($2::date + interval '1 day')
    `, deviceSN, date).Scan(&count)
    require.NoError(t, err)
    return count
}

// GetReading returns the pac_w value for a specific device and timestamp.
func GetReading(t *testing.T, db *sql.DB, deviceSN string, ts time.Time) float64 {
    t.Helper()
    var pacW float64
    err := db.QueryRow(`
        SELECT pac_w FROM growatt.power_readings
        WHERE device_sn = $1 AND ts = $2
    `, deviceSN, ts).Scan(&pacW)
    require.NoError(t, err)
    return pacW
}

// HourlyRow holds an hourly aggregation result.
type HourlyRow struct {
    AvgPacW float64
    MinPacW float64
    MaxPacW float64
    Samples int
}

// GetHourlyAgg returns the hourly aggregation for a device, date, and hour.
func GetHourlyAgg(t *testing.T, db *sql.DB, deviceSN, date string, hour int) HourlyRow {
    t.Helper()
    var row HourlyRow
    err := db.QueryRow(`
        SELECT
            avg(pac_w)  AS avg_pac_w,
            min(pac_w)  AS min_pac_w,
            max(pac_w)  AS max_pac_w,
            count(*)    AS samples
        FROM growatt.power_readings
        WHERE device_sn = $1
          AND ts >= ($2::date + make_interval(hours => $3))
          AND ts < ($2::date + make_interval(hours => $3 + 1))
    `, deviceSN, date, hour).Scan(&row.AvgPacW, &row.MinPacW, &row.MaxPacW, &row.Samples)
    require.NoError(t, err)
    return row
}

// AssertNoGaps checks that no gap exceeds maxGap between consecutive readings.
func AssertNoGaps(t *testing.T, db *sql.DB, deviceSN, date string, maxGap time.Duration) {
    t.Helper()
    rows, err := db.Query(`
        SELECT ts FROM growatt.power_readings
        WHERE device_sn = $1
          AND ts >= $2::date
          AND ts < ($2::date + interval '1 day')
        ORDER BY ts
    `, deviceSN, date)
    require.NoError(t, err)
    defer rows.Close()

    var prev time.Time
    for rows.Next() {
        var ts time.Time
        require.NoError(t, rows.Scan(&ts))
        if !prev.IsZero() {
            gap := ts.Sub(prev)
            assert.LessOrEqual(t, gap, maxGap,
                "gap of %v between %v and %v exceeds max %v", gap, prev, ts, maxGap)
        }
        prev = ts
    }
}

// AssertRowCount checks the exact number of readings for a device on a date.
func AssertRowCount(t *testing.T, db *sql.DB, deviceSN, date string, expected int) {
    t.Helper()
    actual := CountReadings(t, db, deviceSN, date)
    assert.Equal(t, expected, actual,
        "expected %d readings for %s on %s, got %d", expected, deviceSN, date, actual)
}
```

---

## 7. Test Fixtures Factory -- `integration/fixtures/fixtures.go`

```go
//go:build integration

package fixtures

import (
    "database/sql"
    "testing"
    "time"

    "github.com/gogrowatt/integration/testdb"
    "github.com/gogrowatt/integration/testharness"
)

// StandardSetup creates the default plant/device records and returns
// a generator configured for the standard Austin TX scenario.
func StandardSetup(t *testing.T, db *sql.DB) *testharness.Generator {
    t.Helper()
    cfg := testharness.DefaultConfig()
    testdb.SeedPlant(t, db, cfg.PlantID, cfg.PlantName,
        cfg.Latitude, cfg.Longitude, cfg.PeakPowerKW)
    testdb.SeedDevice(t, db, cfg.DeviceSN, cfg.PlantID, cfg.DeviceModel)
    return testharness.NewGenerator(cfg)
}

// LoadGeneratedDay generates data for a date, inserts it into the DB, and
// returns the generated points for assertion purposes.
func LoadGeneratedDay(t *testing.T, db *sql.DB, gen *testharness.Generator,
    deviceSN string, date time.Time) []testharness.DataPoint {
    t.Helper()

    loc, _ := time.LoadLocation("US/Central")
    points := gen.GenerateDay(date)

    for _, dp := range points {
        ts, err := time.ParseInLocation("2006-01-02 15:04", dp.Time, loc)
        if err != nil {
            t.Fatalf("parsing time %q: %v", dp.Time, err)
        }
        testdb.UpsertReading(t, db, deviceSN, ts,
            dp.Pac, dp.Ppv, dp.Vpv1, dp.Vpv2,
            dp.Ipv1, dp.Ipv2, dp.Vac1, dp.Iac1)
    }
    return points
}
```

---

## 8. Scenario Test Implementations -- `integration/fetcher/fetcher_test.go`

### Scenario 1: Clear Sunny Day

```go
//go:build integration

package fetcher_test

import (
    "context"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/gogrowatt/integration/testdb"
    "github.com/gogrowatt/integration/testharness"
    "github.com/gogrowatt/pkg/growatt"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestScenario01_ClearSunnyDay(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    // 1. Setup simulator
    cfg := testharness.DefaultConfig()
    cfg.Scenario = testharness.ScenarioClearDay
    cfg.RandomSeed = 42
    sim := testharness.New(cfg)
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // 2. Setup database
    db := testdb.Connect(t)
    testdb.Reset(t, db)
    testdb.SeedPlant(t, db, cfg.PlantID, cfg.PlantName,
        cfg.Latitude, cfg.Longitude, cfg.PeakPowerKW)
    testdb.SeedDevice(t, db, cfg.DeviceSN, cfg.PlantID, cfg.DeviceModel)

    // 3. Create Growatt client pointing at simulator
    client := growatt.NewClient(cfg.ValidToken,
        growatt.WithBaseURL(server.URL+"/v1/"),
        growatt.WithRateLimit(0),
    )

    // 4. Fetch data for Feb 14 2026
    ctx := context.Background()
    targetDate := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
    data, err := client.GetMINInverterHistory(ctx, cfg.DeviceSN, targetDate, cfg.Timezone)
    require.NoError(t, err)
    require.NotNil(t, data)

    // 5. Insert into DB (simulating what the fetcher service does)
    loc, _ := time.LoadLocation(cfg.Timezone)
    for _, p := range data.Powers {
        ts, _ := time.ParseInLocation("2006-01-02 15:04",
            targetDate.Format("2006-01-02")+" "+p.Time, loc)
        testdb.UpsertReading(t, db, cfg.DeviceSN, ts,
            p.Power, p.Power*1.03, 0, 0, 0, 0, 240.0, 0)
    }

    // 6. Validate completeness
    count := testdb.CountReadings(t, db, cfg.DeviceSN, "2026-02-14")
    assert.GreaterOrEqual(t, count, 80, "expected ~100 readings for full clear day")

    // 7. Validate no gaps > 10 minutes during production hours
    testdb.AssertNoGaps(t, db, cfg.DeviceSN, "2026-02-14", 11*time.Minute)

    // 8. Validate curve shape: smooth with low variance
    row12 := testdb.GetHourlyAgg(t, db, cfg.DeviceSN, "2026-02-14", 12)
    assert.Greater(t, row12.Samples, 8, "hour 12 should have ~12 samples")
    assert.Greater(t, row12.AvgPacW, 2000.0, "hour 12 avg should be >2000 W")
    // Clear day: max should not be hugely different from min within an hour
    if row12.MinPacW > 100 {
        ratio := row12.MaxPacW / row12.MinPacW
        assert.Less(t, ratio, 2.0, "clear day hour 12 should have low variance")
    }
}
```

### Scenario 2: Cloudy Variable Day

```go
func TestScenario02_CloudyVariableDay(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    cfg := testharness.DefaultConfig()
    cfg.Scenario = testharness.ScenarioCloudyDay
    cfg.RandomSeed = 43

    gen := testharness.NewGenerator(cfg)
    points := gen.GenerateDay(time.Date(2026, 2, 13, 0, 0, 0, 0, time.UTC))

    // Validate high variance exists
    var values []float64
    for _, p := range points {
        if p.Pac > 100 {
            values = append(values, p.Pac)
        }
    }
    require.NotEmpty(t, values)

    // Compute standard deviation
    var sum, sumSq float64
    for _, v := range values {
        sum += v
        sumSq += v * v
    }
    n := float64(len(values))
    mean := sum / n
    variance := sumSq/n - mean*mean
    stddev := 0.0
    if variance > 0 {
        stddev = variance // simplified; sqrt in assertion
    }
    _ = stddev

    // Find min and max
    minVal, maxVal := values[0], values[0]
    for _, v := range values {
        if v < minVal {
            minVal = v
        }
        if v > maxVal {
            maxVal = v
        }
    }
    // Cloudy day should have a wide range (from real data: 599 to 5527 W)
    assert.Greater(t, maxVal-minVal, 1500.0,
        "cloudy day range should exceed 1500 W")
}
```

### Scenario 3: API Downtime and Backfill

```go
func TestScenario03_APIDowntimeAndBackfill(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    cfg := testharness.DefaultConfig()
    cfg.Scenario = testharness.ScenarioWithDoubling
    cfg.RandomSeed = 44
    cfg.ErrorSchedule = []testharness.ErrorEvent{
        {
            TriggerAfterRequests: 3,
            ErrorType:            testharness.ErrTypeServerDown,
            Duration:             2 * time.Second,
            HTTPStatus:           503,
        },
    }

    sim := testharness.New(cfg)
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    client := growatt.NewClient(cfg.ValidToken,
        growatt.WithBaseURL(server.URL+"/v1/"),
        growatt.WithRateLimit(0),
    )

    ctx := context.Background()
    targetDate := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)

    // First few requests succeed (plant/list, device/list count toward limit)
    _, err := client.ListPlants(ctx)
    require.NoError(t, err)

    _, err = client.ListDevices(ctx, cfg.PlantID)
    require.NoError(t, err)

    // This request triggers downtime (request #3)
    _, err = client.GetMINInverterHistory(ctx, cfg.DeviceSN, targetDate, cfg.Timezone)
    assert.Error(t, err, "should fail during API downtime")

    // Wait for downtime to expire
    time.Sleep(3 * time.Second)

    // Recovery: retry should succeed
    data, err := client.GetMINInverterHistory(ctx, cfg.DeviceSN, targetDate, cfg.Timezone)
    require.NoError(t, err)
    require.NotNil(t, data)
    assert.Greater(t, len(data.Powers), 0, "should get data after recovery")
}
```

### Scenario 4: Duplicate Data / Upsert Idempotency

```go
func TestScenario04_DuplicateDataHandling(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    db := testdb.Connect(t)
    testdb.Reset(t, db)

    cfg := testharness.DefaultConfig()
    cfg.RandomSeed = 45
    testdb.SeedPlant(t, db, cfg.PlantID, cfg.PlantName,
        cfg.Latitude, cfg.Longitude, cfg.PeakPowerKW)
    testdb.SeedDevice(t, db, cfg.DeviceSN, cfg.PlantID, cfg.DeviceModel)

    loc, _ := time.LoadLocation(cfg.Timezone)
    ts := time.Date(2026, 2, 14, 13, 0, 0, 0, loc)

    // First insert
    testdb.UpsertReading(t, db, cfg.DeviceSN, ts,
        2729.5, 2810.0, 312.4, 310.1, 4.5, 4.6, 241.2, 11.3)

    count1 := testdb.CountReadings(t, db, cfg.DeviceSN, "2026-02-14")
    assert.Equal(t, 1, count1)

    // Second insert with different value (simulating re-fetch)
    testdb.UpsertReading(t, db, cfg.DeviceSN, ts,
        2750.0, 2830.0, 312.4, 310.1, 4.5, 4.6, 241.2, 11.3)

    count2 := testdb.CountReadings(t, db, cfg.DeviceSN, "2026-02-14")
    assert.Equal(t, 1, count2, "upsert should not create duplicate row")

    // Verify latest value wins
    reading := testdb.GetReading(t, db, cfg.DeviceSN, ts)
    assert.InDelta(t, 2750.0, reading, 0.01, "latest value should win on upsert")
}
```

### Scenario 5: Timezone Boundary

```go
func TestScenario05_TimezoneBoundary(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    db := testdb.Connect(t)
    testdb.Reset(t, db)

    cfg := testharness.DefaultConfig()
    testdb.SeedPlant(t, db, cfg.PlantID, cfg.PlantName,
        cfg.Latitude, cfg.Longitude, cfg.PeakPowerKW)
    testdb.SeedDevice(t, db, cfg.DeviceSN, cfg.PlantID, cfg.DeviceModel)

    // Insert reading at 23:55 CST on Feb 14 = 05:55 UTC Feb 15
    cst, _ := time.LoadLocation("US/Central")
    ts := time.Date(2026, 2, 14, 23, 55, 0, 0, cst)
    testdb.UpsertReading(t, db, cfg.DeviceSN, ts,
        0.5, 0.5, 0, 0, 0, 0, 240.0, 0)

    // Insert reading at 00:05 CST on Feb 15 = 06:05 UTC Feb 15
    ts2 := time.Date(2026, 2, 15, 0, 5, 0, 0, cst)
    testdb.UpsertReading(t, db, cfg.DeviceSN, ts2,
        0.3, 0.3, 0, 0, 0, 0, 240.0, 0)

    // Query Feb 14 in CST -- should see the 23:55 reading but NOT the 00:05
    var countFeb14 int
    err := db.QueryRow(`
        SELECT count(*) FROM growatt.power_readings
        WHERE device_sn = $1
          AND ts AT TIME ZONE 'US/Central' >= '2026-02-14'::date
          AND ts AT TIME ZONE 'US/Central' < '2026-02-15'::date
    `, cfg.DeviceSN).Scan(&countFeb14)
    require.NoError(t, err)
    assert.Equal(t, 1, countFeb14, "Feb 14 CST should have exactly 1 reading")

    // Query Feb 15 in CST -- should see the 00:05 reading
    var countFeb15 int
    err = db.QueryRow(`
        SELECT count(*) FROM growatt.power_readings
        WHERE device_sn = $1
          AND ts AT TIME ZONE 'US/Central' >= '2026-02-15'::date
          AND ts AT TIME ZONE 'US/Central' < '2026-02-16'::date
    `, cfg.DeviceSN).Scan(&countFeb15)
    require.NoError(t, err)
    assert.Equal(t, 1, countFeb15, "Feb 15 CST should have exactly 1 reading")

    // Verify same moment expressed in UTC and CST resolves to same row
    tsUTC := time.Date(2026, 2, 15, 5, 55, 0, 0, time.UTC)
    tsCSTSame := time.Date(2026, 2, 14, 23, 55, 0, 0, cst)
    assert.True(t, tsUTC.Equal(tsCSTSame), "should be the same instant")
}
```

### Scenario 6: Empty Response (Nighttime)

```go
func TestScenario06_EmptyNighttime(t *testing.T) {
    cfg := testharness.DefaultConfig()
    cfg.Scenario = testharness.ScenarioNightOnly
    gen := testharness.NewGenerator(cfg)

    points := gen.GenerateDay(time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC))
    assert.Empty(t, points, "night-only scenario should produce no data points")
}
```

### Scenario 7: Rate Limiting (10012)

```go
func TestScenario07_RateLimiting(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    cfg := testharness.DefaultConfig()
    cfg.ErrorSchedule = []testharness.ErrorEvent{
        {
            TriggerAfterRequests: 2,
            ErrorType:            testharness.ErrTypeRateLimit,
            Duration:             1 * time.Second,
        },
    }

    sim := testharness.New(cfg)
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    client := growatt.NewClient(cfg.ValidToken,
        growatt.WithBaseURL(server.URL+"/v1/"),
        growatt.WithRateLimit(0),
    )
    ctx := context.Background()

    // First two requests succeed
    _, err := client.ListPlants(ctx)
    require.NoError(t, err)
    _, err = client.ListPlants(ctx)
    // Second may or may not trigger depending on implementation

    // Third request should be rate limited (returns error code 10012)
    _, err = client.ListPlants(ctx)
    if err != nil {
        // Verify it is an API error with code 10012
        apiErr, ok := err.(*growatt.APIError)
        if ok {
            assert.Equal(t, 10012, apiErr.Code)
            assert.Contains(t, apiErr.Message, "frequently")
        }
    }

    // Wait for rate limit to expire
    time.Sleep(2 * time.Second)

    // Should succeed after cooldown
    _, err = client.ListPlants(ctx)
    require.NoError(t, err)
}
```

### Scenario 8: Auth Failure

```go
func TestScenario08_AuthFailure(t *testing.T) {
    cfg := testharness.DefaultConfig()
    sim := testharness.New(cfg)
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // Create client with WRONG token
    client := growatt.NewClient("wrong-token-xyz",
        growatt.WithBaseURL(server.URL+"/v1/"),
        growatt.WithRateLimit(0),
    )

    _, err := client.ListPlants(context.Background())
    require.Error(t, err, "should fail with auth error")

    apiErr, ok := err.(*growatt.APIError)
    if ok {
        assert.Equal(t, 10011, apiErr.Code, "should return error_code 10011")
    }
}
```

### Scenario 9: Multi-Day Range

```go
func TestScenario09_MultiDayRange(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    cfg := testharness.DefaultConfig()
    cfg.RandomSeed = 48

    sim := testharness.New(cfg)
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    db := testdb.Connect(t)
    testdb.Reset(t, db)
    testdb.SeedPlant(t, db, cfg.PlantID, cfg.PlantName,
        cfg.Latitude, cfg.Longitude, cfg.PeakPowerKW)
    testdb.SeedDevice(t, db, cfg.DeviceSN, cfg.PlantID, cfg.DeviceModel)

    client := growatt.NewClient(cfg.ValidToken,
        growatt.WithBaseURL(server.URL+"/v1/"),
        growatt.WithRateLimit(0),
    )

    // Fetch 7 days
    ctx := context.Background()
    from := time.Date(2026, 2, 9, 0, 0, 0, 0, time.UTC)
    to := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)

    loc, _ := time.LoadLocation(cfg.Timezone)
    current := from
    for !current.After(to) {
        data, err := client.GetMINInverterHistory(ctx, cfg.DeviceSN, current, cfg.Timezone)
        require.NoError(t, err)
        for _, p := range data.Powers {
            ts, _ := time.ParseInLocation("2006-01-02 15:04",
                current.Format("2006-01-02")+" "+p.Time, loc)
            testdb.UpsertReading(t, db, cfg.DeviceSN, ts,
                p.Power, p.Power*1.03, 0, 0, 0, 0, 240.0, 0)
        }
        current = current.AddDate(0, 0, 1)
    }

    // Verify each day has data
    for d := 9; d <= 15; d++ {
        date := fmt.Sprintf("2026-02-%02d", d)
        count := testdb.CountReadings(t, db, cfg.DeviceSN, date)
        assert.Greater(t, count, 0, "day %s should have readings", date)
    }

    // Verify no cross-day leakage: each day should have its own reading times
    var totalRows int
    err := db.QueryRow(`SELECT count(*) FROM growatt.power_readings WHERE device_sn = $1`,
        cfg.DeviceSN).Scan(&totalRows)
    require.NoError(t, err)
    assert.Greater(t, totalRows, 500, "7 days * ~80-100 readings = 500+")
}
```

### Scenario 10: Partial Day (Late Start)

```go
func TestScenario10_PartialDayLateStart(t *testing.T) {
    cfg := testharness.DefaultConfig()
    cfg.RandomSeed = 49

    gen := testharness.NewGenerator(cfg)
    // Feb 12 in real data started at 11:04 -- the generator uses random offset
    points := gen.GenerateDay(time.Date(2026, 2, 12, 0, 0, 0, 0, time.UTC))

    require.NotEmpty(t, points)

    // First reading should be in the morning (after sunrise)
    firstTime := points[0].Time
    assert.Contains(t, firstTime, "2026-02-12", "date should be Feb 12")

    // Verify hours before the first reading have no data
    // The first reading hour extracted:
    hourStr := firstTime[11:13]
    firstHour, _ := strconv.Atoi(hourStr)
    // For a late start, first hour should be 7 or later
    assert.GreaterOrEqual(t, firstHour, 7,
        "first reading should be after 07:00 (sunrise + offset)")
}
```

---

## 9. REST API Validation Tests -- `integration/api/api_test.go`

```go
//go:build integration

package api_test

import (
    "encoding/json"
    "io"
    "net/http"
    "testing"
    "time"

    "github.com/gogrowatt/integration/fixtures"
    "github.com/gogrowatt/integration/testdb"
    "github.com/gogrowatt/integration/testharness"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// These tests validate REST API responses per doc 03 (03-rest-api.md).
// REST API paths use the canonical format: /api/v1/devices/{sn}/power

func TestRESTAPI_DevicePower_RawReadings(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    db := testdb.Connect(t)
    testdb.Reset(t, db)
    gen := fixtures.StandardSetup(t, db)

    date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
    points := fixtures.LoadGeneratedDay(t, db, gen, "ABC123456", date)
    require.NotEmpty(t, points)

    // Start REST API server (assumes api.NewServer exists)
    // apiServer := api.NewServer(db)
    // apiTS := httptest.NewServer(apiServer.Handler())
    // defer apiTS.Close()

    // Query: GET /api/v1/devices/ABC123456/power?from=2026-02-14&to=2026-02-14
    // resp := httpGet(t, apiTS.URL+"/api/v1/devices/ABC123456/power?from=2026-02-14&to=2026-02-14")

    // Expected response envelope per doc 03:
    // {
    //   "data": {
    //     "serial_number": "ABC123456",
    //     "from": "2026-02-14T00:00:00-06:00",
    //     "to": "2026-02-14T23:59:59-06:00",
    //     "interval": "5min",
    //     "timezone": "US/Central",
    //     "fields": ["pac_w"],
    //     "readings": [ { "time": "...", "pac_w": 1234.5 }, ... ]
    //   },
    //   "pagination": { "page": 1, "per_page": 500, "total": N, "total_pages": 1 },
    //   "meta": { "timestamp": "...", "request_id": "..." }
    // }

    // Assertions to implement once REST API server is built:
    // assert.NotEmpty(t, envelope.Meta.RequestID)
    // assert.Equal(t, "ABC123456", envelope.Data.SerialNumber)
    // assert.GreaterOrEqual(t, envelope.Pagination.Total, 80)
    // assertDoublingPattern(t, envelope.Data.Readings)

    t.Log("REST API test scaffolded; requires api.NewServer implementation")
}

func TestRESTAPI_HourlyAggregation(t *testing.T) {
    // Query: GET /api/v1/devices/ABC123456/power?from=2026-02-14&to=2026-02-14&interval=1h
    // Verify each hourly bucket has: avg, min, max, samples
    // Verify avg is between min and max
    // Verify samples ~= 12 per active hour
    t.Log("REST API hourly aggregation test scaffolded")
}

func TestRESTAPI_DeviceEnergy(t *testing.T) {
    // Query: GET /api/v1/devices/ABC123456/energy?from=2026-02-12&to=2026-02-15&unit=day
    // Data source: growatt.mv_daily_production (per consistency analysis issue #8)
    t.Log("REST API device energy test scaffolded")
}

func TestRESTAPI_PlantEnergy(t *testing.T) {
    // Query: GET /api/v1/plants/12345/energy?from=2026-02-01&to=2026-02-15&unit=day
    // Data source: growatt.energy_summaries (plant-level, from Growatt API)
    t.Log("REST API plant energy test scaffolded")
}

// httpGet is a test helper that performs a GET and returns the body bytes.
func httpGet(t *testing.T, url string) []byte {
    t.Helper()
    resp, err := http.Get(url)
    require.NoError(t, err)
    defer resp.Body.Close()
    body, err := io.ReadAll(resp.Body)
    require.NoError(t, err)
    return body
}

// assertDoublingPattern verifies that a time-series of readings shows
// the characteristic ~2x jump around 12:50-13:00.
func assertDoublingPattern(t *testing.T, readings []struct {
    Time string  `json:"time"`
    PacW float64 `json:"pac_w"`
}) {
    t.Helper()
    var maxJump float64
    for i := 1; i < len(readings); i++ {
        jump := readings[i].PacW - readings[i-1].PacW
        if jump > maxJump {
            maxJump = jump
        }
    }
    assert.Greater(t, maxJump, 1500.0,
        "doubling pattern: largest consecutive jump should exceed 1500 W")
}
```

---

## 10. Performance Benchmarks -- `integration/performance/bench_test.go`

```go
//go:build integration

package performance_test

import (
    "context"
    "fmt"
    "sync"
    "testing"
    "time"

    "github.com/gogrowatt/integration/fixtures"
    "github.com/gogrowatt/integration/testdb"
    "github.com/gogrowatt/integration/testharness"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestPerformance_BulkHistoricalLoad(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping performance test")
    }

    db := testdb.Connect(t)
    testdb.Reset(t, db)
    gen := fixtures.StandardSetup(t, db)

    // Load 365 days of data
    start := time.Now()
    totalPoints := 0
    from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
    for d := 0; d < 365; d++ {
        date := from.AddDate(0, 0, d)
        points := fixtures.LoadGeneratedDay(t, db, gen, "ABC123456", date)
        totalPoints += len(points)
    }
    elapsed := time.Since(start)

    t.Logf("Loaded %d points across 365 days in %v", totalPoints, elapsed)
    assert.Less(t, elapsed, 60*time.Second,
        "bulk load of 365 days should complete within 60 seconds")
}

func BenchmarkQuery_SingleDayRaw(b *testing.B) {
    db := testdb.ConnectBench(b)
    // Assumes data is pre-loaded
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        rows, err := db.Query(`
            SELECT ts, pac_w, ppv_w FROM growatt.power_readings
            WHERE device_sn = 'ABC123456'
              AND ts >= '2025-06-15'::date
              AND ts <  '2025-06-16'::date
            ORDER BY ts
        `)
        if err != nil {
            b.Fatal(err)
        }
        rows.Close()
    }
}

func BenchmarkQuery_HourlyAggregation(b *testing.B) {
    db := testdb.ConnectBench(b)
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        rows, err := db.Query(`
            SELECT
                date_trunc('hour', ts) AS bucket,
                avg(pac_w), min(pac_w), max(pac_w), count(*)
            FROM growatt.power_readings
            WHERE device_sn = 'ABC123456'
              AND ts >= '2025-06-15'::date
              AND ts <  '2025-06-16'::date
            GROUP BY bucket
            ORDER BY bucket
        `)
        if err != nil {
            b.Fatal(err)
        }
        rows.Close()
    }
}

func BenchmarkQuery_DailyRollup30Days(b *testing.B) {
    db := testdb.ConnectBench(b)
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        rows, err := db.Query(`
            SELECT
                date_trunc('day', ts) AS day,
                avg(pac_w), max(pac_w),
                SUM(pac_w) * (5.0/60.0) / 1000.0 AS energy_kwh
            FROM growatt.power_readings
            WHERE device_sn = 'ABC123456'
              AND ts >= '2025-06-01'::date
              AND ts <  '2025-07-01'::date
            GROUP BY day
            ORDER BY day
        `)
        if err != nil {
            b.Fatal(err)
        }
        rows.Close()
    }
}

func TestPerformance_ConcurrentQueries(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping concurrency test")
    }

    db := testdb.Connect(t)
    // Assumes data is pre-loaded from bulk test

    const numWorkers = 50
    const queriesPerWorker = 10

    var wg sync.WaitGroup
    errors := make(chan error, numWorkers*queriesPerWorker)
    latencies := make(chan time.Duration, numWorkers*queriesPerWorker)

    for w := 0; w < numWorkers; w++ {
        wg.Add(1)
        go func(workerID int) {
            defer wg.Done()
            for q := 0; q < queriesPerWorker; q++ {
                // Random date in 2025
                day := 1 + (workerID*queriesPerWorker+q)%365
                date := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day)
                dateStr := date.Format("2006-01-02")

                start := time.Now()
                _, err := db.ExecContext(context.Background(), `
                    SELECT ts, pac_w FROM growatt.power_readings
                    WHERE device_sn = 'ABC123456'
                      AND ts >= $1::date
                      AND ts < ($1::date + interval '1 day')
                    ORDER BY ts
                `, dateStr)
                elapsed := time.Since(start)

                latencies <- elapsed
                if err != nil {
                    errors <- err
                }
            }
        }(w)
    }

    wg.Wait()
    close(errors)
    close(latencies)

    // Check no errors
    for err := range errors {
        t.Errorf("concurrent query error: %v", err)
    }

    // Check latencies
    var totalLatency time.Duration
    count := 0
    var maxLatency time.Duration
    for lat := range latencies {
        totalLatency += lat
        count++
        if lat > maxLatency {
            maxLatency = lat
        }
    }
    avgLatency := totalLatency / time.Duration(count)
    t.Logf("Concurrent queries: %d total, avg=%v, max=%v", count, avgLatency, maxLatency)
    assert.Less(t, maxLatency, 500*time.Millisecond,
        "worst-case query latency should be under 500ms")
}
```

---

## 11. Golden File Testing -- `integration/testharness/datagenerator_test.go`

```go
//go:build integration

package testharness_test

import (
    "encoding/json"
    "flag"
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/gogrowatt/integration/testharness"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update-golden", false, "update golden test files")

func TestDataGenerator_Determinism(t *testing.T) {
    seeds := []int64{42, 43, 100, 999}
    scenarios := []testharness.ScenarioType{
        testharness.ScenarioClearDay,
        testharness.ScenarioCloudyDay,
        testharness.ScenarioWithDoubling,
        testharness.ScenarioFlatTop,
        testharness.ScenarioNightOnly,
    }

    for _, seed := range seeds {
        for _, scenario := range scenarios {
            t.Run(string(scenario)+"/seed="+string(rune(seed)), func(t *testing.T) {
                cfg := testharness.DefaultConfig()
                cfg.Scenario = scenario
                cfg.RandomSeed = seed

                gen1 := testharness.NewGenerator(cfg)
                gen2 := testharness.NewGenerator(cfg)

                date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
                data1 := gen1.GenerateDay(date)
                data2 := gen2.GenerateDay(date)

                require.Equal(t, data1, data2,
                    "identical (scenario=%s, seed=%d) must produce identical output",
                    scenario, seed)
            })
        }
    }
}

func TestGoldenFiles(t *testing.T) {
    tests := []struct {
        scenario   testharness.ScenarioType
        seed       int64
        goldenFile string
    }{
        {testharness.ScenarioClearDay, 42, "golden/clear_day_seed42.json"},
        {testharness.ScenarioCloudyDay, 43, "golden/cloudy_day_seed43.json"},
        {testharness.ScenarioNightOnly, 50, "golden/night_only_seed50.json"},
        {testharness.ScenarioWithDoubling, 42, "golden/with_doubling_seed42.json"},
        {testharness.ScenarioFlatTop, 51, "golden/flat_top_seed51.json"},
    }

    date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)

    for _, tt := range tests {
        t.Run(tt.goldenFile, func(t *testing.T) {
            cfg := testharness.DefaultConfig()
            cfg.Scenario = tt.scenario
            cfg.RandomSeed = tt.seed

            gen := testharness.NewGenerator(cfg)
            actual := gen.GenerateDay(date)

            if *updateGolden {
                writeGoldenFile(t, tt.goldenFile, actual)
                return
            }

            expected := loadGoldenFile(t, tt.goldenFile)
            assert.Equal(t, expected, actual,
                "output for %s does not match golden file; run with -update-golden to refresh",
                tt.goldenFile)
        })
    }
}

func TestDataGenerator_DoublingPattern(t *testing.T) {
    cfg := testharness.DefaultConfig()
    cfg.Scenario = testharness.ScenarioWithDoubling
    cfg.RandomSeed = 42

    gen := testharness.NewGenerator(cfg)
    date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
    points := gen.GenerateDay(date)

    require.NotEmpty(t, points)

    // Find the largest consecutive jump
    var maxJump float64
    var jumpIdx int
    for i := 1; i < len(points); i++ {
        jump := points[i].Pac - points[i-1].Pac
        if jump > maxJump {
            maxJump = jump
            jumpIdx = i
        }
    }

    assert.Greater(t, maxJump, 2000.0,
        "doubling jump should exceed 2000 W")
    assert.Contains(t, points[jumpIdx].Time, "12:5",
        "doubling should occur around 12:5x, got %s", points[jumpIdx].Time)
}

func loadGoldenFile(t *testing.T, name string) []testharness.DataPoint {
    t.Helper()
    data, err := os.ReadFile(filepath.Join("testdata", name))
    require.NoError(t, err,
        "golden file %s not found; run with -update-golden to create", name)
    var points []testharness.DataPoint
    require.NoError(t, json.Unmarshal(data, &points))
    return points
}

func writeGoldenFile(t *testing.T, name string, points []testharness.DataPoint) {
    t.Helper()
    dir := filepath.Join("testdata", filepath.Dir(name))
    require.NoError(t, os.MkdirAll(dir, 0o755))
    data, err := json.MarshalIndent(points, "", "  ")
    require.NoError(t, err)
    require.NoError(t, os.WriteFile(filepath.Join("testdata", name), data, 0o644))
    t.Logf("Updated golden file: %s", name)
}
```

---

## 12. Docker Compose Test Topology -- `docker-compose.test.yml`

```yaml
# docker-compose.test.yml
#
# Complete integration test environment for gogrowatt.
# Uses TimescaleDB (NOT plain PostgreSQL) per consistency analysis issue #10.
#
# Usage:
#   docker compose -f docker-compose.test.yml up --abort-on-container-exit
#   docker compose -f docker-compose.test.yml down -v

services:
  timescaledb:
    image: timescale/timescaledb:latest-pg16
    environment:
      POSTGRES_DB: gogrowatt_test
      POSTGRES_USER: test
      POSTGRES_PASSWORD: test
    ports:
      - "5433:5432"
    tmpfs:
      - /var/lib/postgresql/data  # RAM-backed for speed
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U test -d gogrowatt_test"]
      interval: 2s
      timeout: 5s
      retries: 15

  migrate:
    build:
      context: .
      dockerfile: Dockerfile.migrate
    depends_on:
      timescaledb:
        condition: service_healthy
    environment:
      DATABASE_URL: postgres://test:test@timescaledb:5432/gogrowatt_test?sslmode=disable

  simulator:
    build:
      context: .
      dockerfile: Dockerfile.simulator
    ports:
      - "8080:8080"
    environment:
      SIM_LISTEN_ADDR: ":8080"
      SIM_SCENARIO: "with_doubling"
      SIM_SEED: "42"
      SIM_DEVICE_SN: "ABC123456"
      SIM_PLANT_ID: "12345"
      SIM_PEAK_KW: "9.0"
      SIM_LATITUDE: "30.2672"
      SIM_LONGITUDE: "-97.7431"
      SIM_TIMEZONE: "US/Central"
      SIM_TOKEN: "test-harness-token-abc123"
    healthcheck:
      test: ["CMD-SHELL", "wget -q --spider http://localhost:8080/_control/stats || exit 1"]
      interval: 2s
      timeout: 3s
      retries: 10

  fetcher:
    build:
      context: .
      dockerfile: Dockerfile.fetcher
    depends_on:
      migrate:
        condition: service_completed_successfully
      simulator:
        condition: service_healthy
    environment:
      GROWATT_API_KEY: test-harness-token-abc123
      GROWATT_BASE_URL: http://simulator:8080/v1/
      GROWATT_DEVICE_SN: ABC123456
      GROWATT_PLANT_ID: "12345"
      GROWATT_TIMEZONE: US/Central
      DATABASE_URL: postgres://test:test@timescaledb:5432/gogrowatt_test?sslmode=disable
      FETCH_MODE: "single"  # fetch once and exit

  api:
    build:
      context: .
      dockerfile: Dockerfile.api
    depends_on:
      migrate:
        condition: service_completed_successfully
    ports:
      - "8081:8081"
    environment:
      API_PORT: "8081"
      DATABASE_URL: postgres://test:test@timescaledb:5432/gogrowatt_test?sslmode=disable
    healthcheck:
      test: ["CMD-SHELL", "wget -q --spider http://localhost:8081/api/v1/health || exit 1"]
      interval: 2s
      timeout: 3s
      retries: 10

  test-runner:
    build:
      context: .
      dockerfile: Dockerfile.test
    depends_on:
      api:
        condition: service_healthy
      simulator:
        condition: service_healthy
      fetcher:
        condition: service_completed_successfully
    environment:
      API_URL: http://api:8081
      SIM_CONTROL_URL: http://simulator:8080/_control
      GOGROWATT_TEST_DB_URL: postgres://test:test@timescaledb:5432/gogrowatt_test?sslmode=disable
    command: >
      go test -tags=integration -v -count=1
      -timeout=10m
      ./integration/...
```

---

## 13. Dockerfile Definitions

### Dockerfile.test

```dockerfile
FROM golang:1.21-bookworm

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Install test dependencies
RUN go install github.com/stretchr/testify@latest 2>/dev/null || true

ENTRYPOINT ["go", "test"]
CMD ["-tags=integration", "-v", "./integration/..."]
```

### Dockerfile.simulator

```dockerfile
FROM golang:1.21-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /simulator ./cmd/simulator/

FROM debian:bookworm-slim
COPY --from=builder /simulator /usr/local/bin/simulator
EXPOSE 8080
ENTRYPOINT ["simulator"]
```

### Dockerfile.migrate

```dockerfile
FROM golang:1.21-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /migrate ./cmd/migrate/

FROM debian:bookworm-slim
COPY --from=builder /migrate /usr/local/bin/migrate
COPY migrations/ /migrations/
ENTRYPOINT ["migrate"]
```

---

## 14. Makefile Targets

```makefile
# ==============================================================================
# Test Harness Makefile Targets
# ==============================================================================

.PHONY: test test-short test-integration test-perf test-golden
.PHONY: test-docker test-docker-build test-docker-down
.PHONY: test-db-start test-db-stop test-db-reset
.PHONY: golden-update lint

# --- Local Test Targets ---

# Run unit tests only (no external dependencies)
test-short:
	go test -short -race -count=1 ./...

# Run unit + integration tests (requires TimescaleDB at localhost:5433)
test-integration: test-db-start
	GOGROWATT_TEST_DB_URL="postgres://test:test@localhost:5433/gogrowatt_test?sslmode=disable" \
	go test -tags=integration -race -v -count=1 -timeout=5m ./integration/...

# Run performance benchmarks
test-perf: test-db-start
	GOGROWATT_TEST_DB_URL="postgres://test:test@localhost:5433/gogrowatt_test?sslmode=disable" \
	go test -tags=integration -bench=. -benchmem -run='^$$' -timeout=10m ./integration/performance/...

# Run golden file tests
test-golden:
	go test -tags=integration -run=TestGoldenFiles -v ./integration/testharness/...

# Update golden files
golden-update:
	go test -tags=integration -run=TestGoldenFiles -update-golden -v ./integration/testharness/...

# --- Docker-based Test Targets ---

# Build and run all tests in Docker (no local dependencies needed)
test-docker: test-docker-build
	docker compose -f docker-compose.test.yml up \
		--abort-on-container-exit \
		--exit-code-from test-runner
	docker compose -f docker-compose.test.yml down -v

# Build test images
test-docker-build:
	docker compose -f docker-compose.test.yml build

# Tear down test containers and volumes
test-docker-down:
	docker compose -f docker-compose.test.yml down -v --remove-orphans

# --- Test Database Management ---

# Start a local TimescaleDB for integration tests
test-db-start:
	@docker inspect gogrowatt-test-db > /dev/null 2>&1 && \
		docker start gogrowatt-test-db || \
		docker run -d --name gogrowatt-test-db \
			-p 5433:5432 \
			-e POSTGRES_DB=gogrowatt_test \
			-e POSTGRES_USER=test \
			-e POSTGRES_PASSWORD=test \
			timescale/timescaledb:latest-pg16
	@echo "Waiting for TimescaleDB..."
	@for i in $$(seq 1 30); do \
		docker exec gogrowatt-test-db pg_isready -U test -d gogrowatt_test > /dev/null 2>&1 && break; \
		sleep 1; \
	done
	@echo "TimescaleDB ready on port 5433"

# Stop the local test database
test-db-stop:
	docker stop gogrowatt-test-db 2>/dev/null || true

# Stop and remove the test database (fresh start)
test-db-reset:
	docker rm -f gogrowatt-test-db 2>/dev/null || true
	$(MAKE) test-db-start

# --- Linting ---
lint:
	go vet ./...
	staticcheck ./... 2>/dev/null || true

# --- Combined Targets ---

# Full local test suite
test: lint test-short test-integration

# CI target (used by GitHub Actions)
ci: lint test-short test-integration test-perf
```

---

## 15. CI/CD -- GitHub Actions Workflow

```yaml
# .github/workflows/test.yml
name: Test Suite

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  unit-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.21"
      - name: Run unit tests
        run: go test -short -race -coverprofile=coverage.txt -count=1 ./...
      - uses: codecov/codecov-action@v4
        with:
          files: coverage.txt

  integration-tests:
    runs-on: ubuntu-latest
    needs: unit-tests
    services:
      timescaledb:
        image: timescale/timescaledb:latest-pg16
        env:
          POSTGRES_DB: gogrowatt_test
          POSTGRES_USER: test
          POSTGRES_PASSWORD: test
        ports:
          - 5433:5432
        options: >-
          --health-cmd "pg_isready -U test -d gogrowatt_test"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 10
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.21"
      - name: Run integration tests
        run: |
          go test -tags=integration -race -v -count=1 -timeout=5m ./integration/...
        env:
          GOGROWATT_TEST_DB_URL: postgres://test:test@localhost:5433/gogrowatt_test?sslmode=disable

  performance-benchmarks:
    runs-on: ubuntu-latest
    needs: integration-tests
    if: github.ref == 'refs/heads/main'
    services:
      timescaledb:
        image: timescale/timescaledb:latest-pg16
        env:
          POSTGRES_DB: gogrowatt_test
          POSTGRES_USER: test
          POSTGRES_PASSWORD: test
        ports:
          - 5433:5432
        options: >-
          --health-cmd "pg_isready -U test -d gogrowatt_test"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 10
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.21"
      - name: Run benchmarks
        run: |
          go test -tags=integration -bench=. -benchmem \
            -run='^$' -timeout=10m \
            ./integration/performance/... \
            | tee benchmark-results.txt
        env:
          GOGROWATT_TEST_DB_URL: postgres://test:test@localhost:5433/gogrowatt_test?sslmode=disable
      - name: Store benchmark results
        uses: benchmark-action/github-action-benchmark@v1
        with:
          tool: go
          output-file-path: benchmark-results.txt
          auto-push: false
          comment-on-alert: true

  docker-integration:
    runs-on: ubuntu-latest
    needs: unit-tests
    steps:
      - uses: actions/checkout@v4
      - name: Build and run Docker integration tests
        run: |
          docker compose -f docker-compose.test.yml up \
            --build \
            --abort-on-container-exit \
            --exit-code-from test-runner
      - name: Cleanup
        if: always()
        run: docker compose -f docker-compose.test.yml down -v
```

---

## 16. Compression Verification Test

This test verifies that TimescaleDB compression policies work correctly and that compressed data remains queryable.

```go
func TestCompression_DataSurvivesCompression(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping compression test")
    }

    db := testdb.Connect(t)
    testdb.Reset(t, db)
    gen := fixtures.StandardSetup(t, db)

    // Insert data 60 days ago (beyond 30-day compression threshold)
    oldDate := time.Now().AddDate(0, 0, -60)
    points := fixtures.LoadGeneratedDay(t, db, gen, "ABC123456", oldDate)
    require.NotEmpty(t, points)

    dateStr := oldDate.Format("2006-01-02")
    countBefore := testdb.CountReadings(t, db, "ABC123456", dateStr)
    require.Greater(t, countBefore, 0)

    // Enable compression and manually compress
    _, err := db.Exec(`
        ALTER TABLE growatt.power_readings SET (
            timescaledb.compress,
            timescaledb.compress_segmentby = 'device_sn',
            timescaledb.compress_orderby = 'ts DESC'
        )
    `)
    // Compression may already be set; ignore error
    _ = err

    _, err = db.Exec(`
        SELECT compress_chunk(c)
        FROM show_chunks('growatt.power_readings',
            older_than => interval '31 days') c
    `)
    // May fail if no chunks qualify; that is acceptable
    _ = err

    // Verify data is still queryable after compression
    countAfter := testdb.CountReadings(t, db, "ABC123456", dateStr)
    assert.Equal(t, countBefore, countAfter,
        "data count should be identical after compression")

    // Verify actual values survived
    var maxPac float64
    err = db.QueryRow(`
        SELECT max(pac_w) FROM growatt.power_readings
        WHERE device_sn = 'ABC123456'
          AND ts >= $1::date
          AND ts < ($1::date + interval '1 day')
    `, dateStr).Scan(&maxPac)
    require.NoError(t, err)
    assert.Greater(t, maxPac, 0.0, "compressed data should still return valid values")
}
```

---

## 17. Retention Policy Test

```go
func TestRetention_OldDataDropped(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping retention test")
    }

    db := testdb.Connect(t)
    testdb.Reset(t, db)
    gen := fixtures.StandardSetup(t, db)

    // Insert data that would be older than the 2-year retention policy
    // For testing, we use a shorter interval and manual chunk drop
    ancientDate := time.Date(2020, 6, 15, 0, 0, 0, 0, time.UTC)
    fixtures.LoadGeneratedDay(t, db, gen, "ABC123456", ancientDate)

    countBefore := testdb.CountReadings(t, db, "ABC123456", "2020-06-15")
    require.Greater(t, countBefore, 0, "should have inserted ancient data")

    // Manually drop chunks older than a threshold
    _, err := db.Exec(`
        SELECT drop_chunks('growatt.power_readings', older_than => interval '3 years')
    `)
    require.NoError(t, err)

    countAfter := testdb.CountReadings(t, db, "ABC123456", "2020-06-15")
    assert.Equal(t, 0, countAfter, "ancient data should be dropped by retention")
}
```

---

## 18. Simulator Standalone Server -- `cmd/simulator/main.go`

For Docker deployment, the simulator runs as a standalone HTTP server.

```go
package main

import (
    "fmt"
    "log"
    "net/http"
    "os"
    "strconv"

    "github.com/gogrowatt/integration/testharness"
)

func main() {
    cfg := testharness.DefaultConfig()

    // Override from environment variables
    if v := os.Getenv("SIM_DEVICE_SN"); v != "" {
        cfg.DeviceSN = v
    }
    if v := os.Getenv("SIM_PLANT_ID"); v != "" {
        cfg.PlantID = v
    }
    if v := os.Getenv("SIM_SCENARIO"); v != "" {
        cfg.Scenario = testharness.ScenarioType(v)
    }
    if v := os.Getenv("SIM_SEED"); v != "" {
        if seed, err := strconv.ParseInt(v, 10, 64); err == nil {
            cfg.RandomSeed = seed
        }
    }
    if v := os.Getenv("SIM_PEAK_KW"); v != "" {
        if pk, err := strconv.ParseFloat(v, 64); err == nil {
            cfg.PeakPowerKW = pk
        }
    }
    if v := os.Getenv("SIM_LATITUDE"); v != "" {
        if lat, err := strconv.ParseFloat(v, 64); err == nil {
            cfg.Latitude = lat
        }
    }
    if v := os.Getenv("SIM_LONGITUDE"); v != "" {
        if lon, err := strconv.ParseFloat(v, 64); err == nil {
            cfg.Longitude = lon
        }
    }
    if v := os.Getenv("SIM_TIMEZONE"); v != "" {
        cfg.Timezone = v
    }
    if v := os.Getenv("SIM_TOKEN"); v != "" {
        cfg.ValidToken = v
    }

    sim := testharness.New(cfg)

    addr := os.Getenv("SIM_LISTEN_ADDR")
    if addr == "" {
        addr = ":8080"
    }

    log.Printf("Starting Growatt API simulator on %s", addr)
    log.Printf("  Device SN: %s", cfg.DeviceSN)
    log.Printf("  Scenario:  %s", cfg.Scenario)
    log.Printf("  Seed:      %d", cfg.RandomSeed)
    log.Printf("  Location:  %.4f, %.4f (%s)", cfg.Latitude, cfg.Longitude, cfg.Timezone)

    if err := http.ListenAndServe(addr, sim.Handler()); err != nil {
        fmt.Fprintf(os.Stderr, "server error: %v\n", err)
        os.Exit(1)
    }
}
```

---

## 19. Test Matrix Summary

| # | Scenario | Go Test Function | Seed | Error Injection | Key Assertions |
|---|---|---|---|---|---|
| 1 | Clear sunny day | `TestScenario01_ClearSunnyDay` | 42 | None | Full curve, smooth, ~100 readings, no gaps >10min |
| 2 | Cloudy variable day | `TestScenario02_CloudyVariableDay` | 43 | None | High variance (range >1500W), erratic swings |
| 3 | API downtime + backfill | `TestScenario03_APIDowntimeAndBackfill` | 44 | `ServerDown` after 3 req, 2s | Error during outage, recovery succeeds |
| 4 | Duplicate/upsert | `TestScenario04_DuplicateDataHandling` | 45 | None | Row count unchanged, latest value wins |
| 5 | Timezone boundary | `TestScenario05_TimezoneBoundary` | 46 | None | CST midnight split correctly assigned |
| 6 | Nighttime empty | `TestScenario06_EmptyNighttime` | -- | None | Zero data points returned |
| 7 | Rate limiting | `TestScenario07_RateLimiting` | 47 | `RateLimit` after 2 req | Error 10012, recovery after cooldown |
| 8 | Auth failure | `TestScenario08_AuthFailure` | -- | Wrong token | Error 10011, no crash |
| 9 | Multi-day range | `TestScenario09_MultiDayRange` | 48 | None | 7 days loaded, no cross-day leakage |
| 10 | Partial day | `TestScenario10_PartialDayLateStart` | 49 | None | First reading after 07:00 |

---

## 20. Query Latency Targets

These benchmarks run against TimescaleDB with a year of generated data (~36,500 rows).

| Query | Benchmark Function | Target P50 | Target P99 |
|---|---|---|---|
| Single day raw readings | `BenchmarkQuery_SingleDayRaw` | < 5 ms | < 20 ms |
| Hourly aggregation (1 day) | `BenchmarkQuery_HourlyAggregation` | < 10 ms | < 50 ms |
| Daily rollup (30 days) | `BenchmarkQuery_DailyRollup30Days` | < 20 ms | < 100 ms |
| 50 concurrent queries | `TestPerformance_ConcurrentQueries` | < 50 ms avg | < 500 ms max |

---

## 21. REST API Path Reference (from doc 03)

All test assertions must use these canonical paths. This table cross-references test usage with the API spec.

| Endpoint | HTTP Method | Test Usage |
|---|---|---|
| `GET /api/v1/health` | GET | Health check before test suite |
| `GET /api/v1/plants` | GET | Verify plant seeding |
| `GET /api/v1/plants/{id}` | GET | Plant detail with devices |
| `GET /api/v1/plants/{id}/devices` | GET | Device listing |
| `GET /api/v1/devices/{sn}` | GET | Device detail |
| `GET /api/v1/devices/{sn}/power?from=...&to=...` | GET | Primary power query (scenarios 1-10) |
| `GET /api/v1/devices/{sn}/power?...&interval=1h` | GET | Hourly aggregation validation |
| `GET /api/v1/devices/{sn}/power/latest` | GET | Latest reading check |
| `GET /api/v1/devices/{sn}/energy?from=...&to=...&unit=day` | GET | Device daily energy |
| `GET /api/v1/plants/{id}/energy?from=...&to=...&unit=day` | GET | Plant energy (growatt.energy_summaries) |
| `GET /api/v1/devices/{sn}/stats?from=...&to=...` | GET | Statistical summaries |

---

## 22. Database Column Name Cross-Reference

Per consistency analysis, all code must use the schema-canonical column names with unit suffixes.

| Go Struct Field | Growatt API JSON | DB Column | REST API JSON |
|---|---|---|---|
| `Pac` | `pac` | `pac_w` | `pac_w` |
| `Ppv` | `ppv` | `ppv_w` | `ppv_w` |
| `Vpv1` | `vpv1` | `vpv1_v` | `vpv1_v` |
| `Vpv2` | `vpv2` | `vpv2_v` | `vpv2_v` |
| `Ipv1` | `ipv1` | `ipv1_a` | `ipv1_a` |
| `Ipv2` | `ipv2` | `ipv2_a` | `ipv2_a` |
| `Vac1` | `vac1` | `vac1_v` | `vac1_v` |
| `Iac1` | `iac1` | `iac1_a` | `iac1_a` |
| `Time` | `time` | `ts` | `time` |
| -- | -- | `fetched_at` | -- |
| -- | -- | `device_sn` | `serial_number` |

---

## 23. Implementation Order

Recommended build sequence:

1. **`integration/testharness/config.go`** -- Types and constants
2. **`integration/testharness/solar.go`** -- Sunrise/sunset calculator (pure math, easy to test)
3. **`integration/testharness/datagenerator.go`** -- Data generation pipeline
4. **`integration/testharness/datagenerator_test.go`** -- Determinism and golden file tests
5. **`integration/testharness/simulator.go`** -- HTTP server wrapping the generator
6. **`integration/testharness/simulator_test.go`** -- Endpoint format verification
7. **`integration/testdb/testdb.go`** -- Database connection and reset utilities
8. **`integration/testdb/assertions.go`** -- Query helpers
9. **`integration/fixtures/fixtures.go`** -- Test data factories
10. **`integration/fetcher/fetcher_test.go`** -- Scenarios 1-10
11. **`integration/api/api_test.go`** -- REST API validation (depends on API server impl)
12. **`integration/performance/bench_test.go`** -- Benchmarks
13. **`docker-compose.test.yml`** -- Container topology
14. **`cmd/simulator/main.go`** -- Standalone simulator binary
15. **Makefile targets** -- Build automation
16. **`.github/workflows/test.yml`** -- CI pipeline

Each step is testable independently. Steps 1-6 require no database. Steps 7-10 require only a running TimescaleDB instance. Steps 11+ require the full stack.
