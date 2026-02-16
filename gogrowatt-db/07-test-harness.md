# 07 - Test Harness Design

## Overview

This document specifies a comprehensive integration test harness for the gogrowatt system. The harness simulates the Growatt cloud API, drives the data fetcher against it, persists results to PostgreSQL (TimescaleDB), and validates correctness through the read-only REST API. All test data is modeled on real production data captured from a MIN 9000TL-X inverter in Austin, TX (US/Central timezone).

### Key Observations from Real Data

Analysis of CSV files from 2026-02-12 through 2026-02-15 reveals these patterns:

| Characteristic | Observed Value |
|---|---|
| Sunrise (first nonzero reading) | ~07:28 (winter) to ~11:04 (late start / partial day) |
| Sunset (last nonzero reading) | ~19:16 - 19:22 |
| Interval spacing | ~5 minutes, but not exact (e.g., 11:04, 11:09, 11:14 -- offset from :00/:05) |
| Pre-doubling peak | ~2,700 - 2,750 W (single-string output) |
| Post-doubling peak | ~5,000 - 5,400 W (both strings active) |
| Doubling transition | Occurs between 12:50 and 13:00 (sharp jump from ~2,700 W to ~5,200 W) |
| Cloudy day variance | Feb 13: erratic swings from 6,006 W down to 599 W within hour 14 |
| Clear day variance | Feb 12/14: smooth bell curve above the doubling point |
| Nighttime readings | Small nonzero values (0.1 - 3.6 W) persist for ~20 min after true sunset |
| API error code for rate limit | 10012 with message containing "frequently" |
| API error code for auth | 10011 |
| Time format from MIN history | "YYYY-MM-DD HH:MM" (full datetime, normalized to "HH:MM") |
| Time format from plant/power | "HH:MM" (time only, map keys) |

---

## 1. Architecture

```mermaid
graph TB
    subgraph "Test Runner (go test)"
        TR[Test Orchestrator]
        SC[Scenario Controller]
        VA[Validation Assertions]
    end

    subgraph "Simulated Growatt API (httptest.Server)"
        SIM[Simulator Mux]
        PL[/v1/plant/list/]
        PD[/v1/plant/data/]
        PP[/v1/plant/power/]
        DL[/v1/device/list/]
        TI[/v1/device/tlx/tlx_data_info/]
        TH[/v1/device/tlx/tlx_data/]
        DG[Data Generator Engine]
    end

    subgraph "System Under Test"
        FET[Data Fetcher Service]
        GC[growatt.Client]
        DB[(TimescaleDB)]
        API[REST API Server]
    end

    TR --> SC
    SC -->|configures scenarios| SIM
    SC -->|triggers fetch cycles| FET
    TR --> VA

    SIM --> PL
    SIM --> PD
    SIM --> PP
    SIM --> DL
    SIM --> TI
    SIM --> TH

    PL --> DG
    PD --> DG
    PP --> DG
    TH --> DG

    FET --> GC
    GC -->|HTTP| SIM
    FET -->|INSERT/UPSERT| DB
    API -->|SELECT| DB
    VA -->|HTTP GET| API
    VA -->|direct SQL| DB
```

---

## 2. Test Execution Flow

```mermaid
sequenceDiagram
    participant T as Test Runner
    participant SC as Scenario Controller
    participant SIM as API Simulator
    participant FET as Data Fetcher
    participant DB as TimescaleDB
    participant API as REST API

    T->>DB: Reset schema (migrate fresh)
    T->>SC: Load scenario config
    SC->>SIM: Configure data generator + error injection
    T->>SIM: Start httptest.Server
    T->>API: Start REST API (points at DB)

    loop For each simulated time window
        T->>SC: Advance simulated clock
        SC->>SIM: Update available data window
        T->>FET: Trigger fetch cycle (pointing at SIM)
        FET->>SIM: GET /v1/device/list
        SIM-->>FET: Device list response
        FET->>SIM: POST /v1/device/tlx/tlx_data
        SIM-->>FET: History data (or error per scenario)
        FET->>DB: Upsert power readings
    end

    T->>API: GET /api/v1/devices/ABC123456/power?from=2026-02-14&to=2026-02-14
    API->>DB: SELECT FROM growatt.power_readings
    DB-->>API: Results
    API-->>T: JSON response {data, pagination, meta}

    T->>T: Validate completeness
    T->>T: Validate accuracy
    T->>T: Validate aggregations
    T->>T: Report results
```

---

## 3. Simulated API Server

### 3.1 Endpoint Implementation

The simulator implements the same URL structure as the real Growatt OpenAPI. The `growatt.Client` connects to `httptest.Server.URL + "/v1/"` via the `WithBaseURL` option.

#### Endpoints

| Method | Path | Parameters | Response Type |
|---|---|---|---|
| GET | `plant/list` | (token header) | `Response[PlantListData]` |
| GET | `plant/data` | `plant_id` | `Response[PlantData]` |
| GET | `plant/power` | `plant_id`, `date` | `Response[PowerDataRaw]` |
| GET | `device/list` | `plant_id` | `Response[DeviceListData]` |
| GET | `device/tlx/tlx_data_info` | `tlx_sn` | `Response[MINInverterData]` |
| POST | `device/tlx/tlx_data` | `tlx_sn`, `start_date`, `end_date`, `timezone_id`, `page`, `perpage` | `Response[MINHistoryResponse]` |

#### Token Validation

Every request must include a `token` header. The simulator validates it against a configured test token (`"test-harness-token-abc123"`). Invalid tokens return:

```json
{"error_code": 10011, "error_msg": "error_permission_denied", "data": ""}
```

### 3.2 Simulator State Machine

```mermaid
stateDiagram-v2
    [*] --> Normal: default

    Normal --> RateLimited: rate_limit trigger
    Normal --> Down: api_down trigger
    Normal --> PartialData: gap trigger
    Normal --> Normal: data request

    RateLimited --> Normal: cooldown expires
    RateLimited --> RateLimited: request during cooldown

    Down --> Normal: recovery trigger
    Down --> Down: request during outage

    PartialData --> Normal: gap window ends

    state Normal {
        [*] --> Serving
        Serving --> Serving: respond with generated data
    }

    state RateLimited {
        [*] --> Rejecting
        Rejecting --> Rejecting: respond 10012 frequently_access
    }

    state Down {
        [*] --> Unreachable
        Unreachable --> Unreachable: TCP reset / HTTP 503
    }

    state PartialData {
        [*] --> OmittingPoints
        OmittingPoints --> OmittingPoints: respond with gaps in time series
    }
```

### 3.3 Configuration Interface

```go
// SimulatorConfig controls the API simulator behavior.
type SimulatorConfig struct {
    // Plant/device identity
    PlantID    string   // e.g., "12345"
    PlantName  string   // e.g., "Home Solar"
    DeviceSN   string   // e.g., "ABC123456"
    DeviceModel string  // e.g., "MIN 9000TL-X"
    PeakPowerKW float64 // e.g., 9.0

    // Location (drives sunrise/sunset)
    Latitude  float64 // e.g., 30.2672
    Longitude float64 // e.g., -97.7431
    Timezone  string  // e.g., "US/Central"

    // Data generation
    Scenario     ScenarioType
    RandomSeed   int64  // deterministic generation
    DateRange    DateRange
    IntervalSecs int    // default 300 (5 min)

    // Error injection
    ErrorSchedule []ErrorEvent
}

type ScenarioType string
const (
    ScenarioClearDay    ScenarioType = "clear_day"
    ScenarioCloudyDay   ScenarioType = "cloudy_day"
    ScenarioNightOnly   ScenarioType = "night_only"
    ScenarioWithDoubling ScenarioType = "with_doubling" // default; matches real data
    ScenarioFlatTop     ScenarioType = "flat_top"       // saturated inverter
)

type ErrorEvent struct {
    TriggerAfterRequests int           // inject after N requests
    TriggerAtTime        time.Time     // or inject at simulated time
    ErrorType            ErrorType     // rate_limit, auth_fail, server_down, empty_response
    Duration             time.Duration // how long the error persists
    HTTPStatus           int           // 0 = use JSON error body; 503 = HTTP-level failure
}

type ErrorType string
const (
    ErrTypeRateLimit     ErrorType = "rate_limit"
    ErrTypeAuthFail      ErrorType = "auth_fail"
    ErrTypeServerDown    ErrorType = "server_down"
    ErrTypeEmptyResponse ErrorType = "empty_response"
    ErrTypeTimeout       ErrorType = "timeout"
)
```

---

## 4. Data Generation Model

The data generator synthesizes realistic 5-minute interval power readings based on the patterns observed in the real CSV data.

```mermaid
graph TD
    subgraph "Data Generation Pipeline"
        CFG[Scenario Config<br/>peak_kw, lat/lon, seed]
        SUN[Solar Position Model<br/>sunrise/sunset from lat/lon + date]
        BASE[Base Irradiance Curve<br/>sin envelope: 0 at sunrise/sunset, 1.0 at solar noon]
        DBL[Doubling Model<br/>at ~12:55 local: multiply by 2x<br/>simulates second string activation]
        CLOUD[Cloud Layer<br/>Perlin noise or random walk<br/>multiplier 0.1 - 1.0]
        NOISE[Measurement Noise<br/>Gaussian +/- 2% of reading]
        CLAMP[Clamping & Quantization<br/>min=0, max=peak_kw*1000<br/>round to 0.1 W]
        OUT[5-min PowerDataPoint array]
    end

    CFG --> SUN
    SUN --> BASE
    BASE --> DBL
    DBL --> CLOUD
    CLOUD --> NOISE
    NOISE --> CLAMP
    CLAMP --> OUT
```

### 4.1 Solar Position Model

For a given date and latitude/longitude, calculate approximate sunrise and sunset times. In Austin, TX in February:

| Parameter | Winter Value (Feb) |
|---|---|
| Sunrise | ~07:15 CST |
| Solar noon | ~12:35 CST |
| Sunset | ~18:15 CST |
| First nonzero reading | sunrise + 10-15 min |
| Last nonzero reading | sunset + 50-65 min (long tail) |

### 4.2 Base Irradiance Curve

```
power(t) = peak_single_string * sin(pi * (t - sunrise) / (sunset - sunrise))
```

Where `peak_single_string` is approximately `peak_kw * 1000 / 2` (since the system has two PV strings, and only one is active in the morning). From the real data, this yields ~2,700 W peak for a 9 kW system before the doubling event.

### 4.3 Doubling Event Model (The ~13:00 Jump)

The real data shows a consistent pattern across all four observed days: power approximately doubles between 12:50 and 13:00. This likely corresponds to a second PV string becoming active (different roof orientation catching afternoon sun, or an MPPT activation threshold).

```
if t >= doubling_time:
    power(t) = power(t) * doubling_factor
```

| Parameter | Value from Real Data |
|---|---|
| `doubling_time` | 12:55 +/- 5 min (varies by day) |
| `doubling_factor` | 1.85 - 2.05 (not exactly 2x) |
| Feb 12 | 2,748.9 W -> 5,226.0 W at 13:00 (factor: 1.90) |
| Feb 13 | 2,756.1 W -> 5,005.3 W at 12:50 (factor: 1.82) |
| Feb 14 | 2,729.5 W -> 5,223.3 W at 12:56 (factor: 1.91) |
| Feb 15 | 2,613.6 W -> 5,389.9 W at 12:53 (factor: 2.06) |

### 4.4 Cloud Cover Models

**Clear day** (Feb 12, Feb 14): Low variance, smooth bell curve. Gaussian noise with sigma = 3% of reading.

**Cloudy day** (Feb 13): High variance, erratic swings. Random walk with large steps:
- Hour 14 ranged from 599.5 W to 5,526.8 W
- Standard deviation 1,634.6 W (vs. clear day ~50 W at same hour)
- Model: Multiply base curve by cloud_factor sampled from Beta(2, 5) distribution, producing values biased low with occasional clear spikes.

**Variable day** (Feb 15 morning): Moderate variance with partial clearing. Cloud factor from Beta(3, 3).

### 4.5 Time Offset Model

Real Growatt data does not align to exact :00/:05 boundaries. Observed offsets:

| Date | First Reading | Offset from :00 |
|---|---|---|
| Feb 12 | 11:04 | +4 min |
| Feb 13 | 11:05 | +5 min |
| Feb 14 | 11:05 | +5 min |
| Feb 15 | 07:28 | +3 min |

The simulator applies a per-day random offset of 0-5 minutes to all timestamps, matching this real-world behavior.

### 4.6 Nighttime Tail

After sunset, readings do not immediately drop to zero. The real data shows:

```
19:01  0.70 W
19:06  1.90 W
19:11  0.20 W
19:16  0.10 W
19:21  3.30 W  (small spike, possibly measurement noise)
```

The generator produces 4-5 readings of 0.1-3.5 W after sunset, then stops returning data.

---

## 5. Scenario Test Suite

### 5.1 Scenario Definitions

```mermaid
stateDiagram-v2
    direction LR

    state "Scenario: Clear Sunny Day" as S1 {
        [*] --> Night1: 00:00-07:15
        Night1 --> Ramp_Up: 07:15-10:00
        Ramp_Up --> Morning_Peak: 10:00-12:55
        Morning_Peak --> Doubling: 12:55
        Doubling --> Afternoon_Peak: 12:55-14:30
        Afternoon_Peak --> Ramp_Down: 14:30-18:15
        Ramp_Down --> Tail: 18:15-19:25
        Tail --> Night2: 19:25-23:59
    }

    state "Scenario: Cloudy Variable Day" as S2 {
        [*] --> Night_C: 00:00-07:15
        Night_C --> Erratic_Morning: 07:15-12:55
        Erratic_Morning --> Erratic_Doubling: 12:55
        Erratic_Doubling --> Wild_Afternoon: 12:55-17:00
        Wild_Afternoon --> Ramp_Down_C: 17:00-19:00
        Ramp_Down_C --> Night_C2: 19:00-23:59
    }

    state "Scenario: API Downtime + Recovery" as S3 {
        [*] --> Normal_Fetch: cycles 1-5
        Normal_Fetch --> API_Down: cycle 6 (simulate outage)
        API_Down --> Retry_Fail: cycles 7-10 (fetcher gets errors)
        Retry_Fail --> Recovery: cycle 11 (API returns)
        Recovery --> Backfill: cycles 11-15 (fetcher backfills gaps)
        Backfill --> Verify: all data present
    }

    state "Scenario: Rate Limiting" as S4 {
        [*] --> Fast_Requests: burst of 5 requests
        Fast_Requests --> Rate_Limited: error_code 10012
        Rate_Limited --> Backoff: client waits
        Backoff --> Retry_Success: retry succeeds
    }
```

### 5.2 Scenario Test Matrix

| # | Scenario | Generator Config | Error Injection | Validates |
|---|---|---|---|---|
| 1 | Clear sunny day | `ScenarioWithDoubling`, seed=42 | None | Full curve shape, doubling at ~13:00, 5-min completeness |
| 2 | Cloudy/variable day | `ScenarioCloudyDay`, seed=43 | None | High variance tolerated, no false gaps flagged |
| 3 | API downtime + backfill | `ScenarioWithDoubling`, seed=44 | `ServerDown` at request 6, duration 5 cycles | Fetcher retries, backfills missing window, final data complete |
| 4 | Duplicate data handling | `ScenarioWithDoubling`, seed=45 | None; fetcher runs twice for same date | Upsert idempotency, no duplicate rows, latest value wins |
| 5 | Timezone boundary | `ScenarioWithDoubling`, seed=46 | Date crosses UTC midnight | Readings correctly assigned to local date, not UTC date |
| 6 | Empty response (nighttime) | `ScenarioNightOnly` | None | Empty powers array handled, no errors, no rows inserted |
| 7 | Rate limit (10012) | `ScenarioWithDoubling`, seed=47 | `RateLimit` after every 3rd request | Client backs off, eventually gets all data |
| 8 | Auth failure | Any | `AuthFail` on first request | Client surfaces error, does not crash |
| 9 | Multi-day range | `ScenarioWithDoubling`, seed=48, 7 days | None | 7 days of data, daily rollups correct, no cross-day leakage |
| 10 | Partial day (data starts late) | Custom: first reading at 11:04 (matches Feb 12) | None | Hours 0-10 correctly show zero, hour 11 aggregation starts at 11:04 |

### 5.3 Test Assertions

Each scenario test performs these validation categories:

**Completeness checks:**
- For each hour with expected production, verify at least 10 readings exist (12 expected at 5-min intervals)
- No unexpected gaps longer than 10 minutes within active production hours
- Night hours (22:00 - 06:00) have zero or no readings

**Accuracy checks:**
- Each stored `pac_w` value matches what the simulator sent (within 0.01 W floating-point tolerance)
- `ts` timestamps match (exact string comparison after normalization)
- Plant ID and device SN match configuration

**Upsert checks:**
- Run fetcher twice for the same date window
- Second run with modified values for a subset of timestamps
- Query DB: verify modified values are present (latest wins)
- Verify row count did not increase (upsert, not insert)

**Aggregation checks:**
- Query hourly rollup from REST API via `GET /api/v1/devices/{sn}/power?from=...&to=...&interval=1h`
- Compute expected hourly avg/min/max from raw test data
- Compare with tolerance: avg within 0.1 W, min/max exact

---

## 6. Docker Compose Service Topology

```mermaid
graph TB
    subgraph "docker-compose.test.yml"
        PG[timescale/timescaledb:latest-pg16<br/>Port 5433<br/>DB: gogrowatt_test]
        MIG[migrate<br/>Runs schema migrations<br/>then exits]
        FETCH[gogrowatt-fetcher<br/>GROWATT_BASE_URL=http://sim:8080/v1/<br/>DB_URL=postgres://...]
        SIM_SVC[gogrowatt-sim<br/>Simulated Growatt API<br/>Port 8080]
        REST[gogrowatt-api<br/>Read-only REST API<br/>Port 8081]
        TEST[gogrowatt-test-runner<br/>go test ./integration/...<br/>Depends on all above]
    end

    MIG -->|depends_on| PG
    FETCH -->|depends_on| MIG
    FETCH -->|depends_on| SIM_SVC
    REST -->|depends_on| MIG
    TEST -->|depends_on| FETCH
    TEST -->|depends_on| REST
    TEST -->|depends_on| SIM_SVC

    FETCH -->|HTTP| SIM_SVC
    FETCH -->|SQL| PG
    REST -->|SQL| PG
    TEST -->|HTTP assertions| REST
    TEST -->|HTTP control| SIM_SVC
    TEST -->|direct SQL| PG
```

### 6.1 Docker Compose File Structure

```yaml
# docker-compose.test.yml
services:
  postgres:
    image: timescale/timescaledb:latest-pg16
    environment:
      POSTGRES_DB: gogrowatt_test
      POSTGRES_USER: test
      POSTGRES_PASSWORD: test
    ports:
      - "5433:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U test -d gogrowatt_test"]
      interval: 2s
      timeout: 5s
      retries: 10

  migrate:
    build:
      context: .
      dockerfile: Dockerfile.migrate
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DATABASE_URL: postgres://test:test@postgres:5432/gogrowatt_test?sslmode=disable

  simulator:
    build:
      context: .
      dockerfile: Dockerfile.simulator
    ports:
      - "8080:8080"
    environment:
      SIM_SCENARIO: "clear_day"   # overridden per test via control API
      SIM_SEED: "42"

  fetcher:
    build:
      context: .
      dockerfile: Dockerfile.fetcher
    depends_on:
      migrate:
        condition: service_completed_successfully
      simulator:
        condition: service_started
    environment:
      GROWATT_API_KEY: test-harness-token-abc123
      GROWATT_BASE_URL: http://simulator:8080/v1/
      GROWATT_DEVICE_SN: ABC123456
      GROWATT_TIMEZONE: US/Central
      DATABASE_URL: postgres://test:test@postgres:5432/gogrowatt_test?sslmode=disable

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
      DATABASE_URL: postgres://test:test@postgres:5432/gogrowatt_test?sslmode=disable

  test-runner:
    build:
      context: .
      dockerfile: Dockerfile.test
    depends_on:
      api:
        condition: service_started
      fetcher:
        condition: service_started
    environment:
      API_URL: http://api:8081
      SIM_CONTROL_URL: http://simulator:8080/_control
      DATABASE_URL: postgres://test:test@postgres:5432/gogrowatt_test?sslmode=disable
```

### 6.2 In-Process Alternative (Unit/Integration Hybrid)

For fast iteration and CI without Docker, tests can also run entirely in-process:

```go
func TestIntegration_ClearDay(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    // 1. Start simulated API server (httptest)
    sim := simulator.New(simulator.Config{
        Scenario: simulator.ClearDay,
        Seed:     42,
        PlantID:  "12345",
        DeviceSN: "ABC123456",
    })
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // 2. Connect to test database (requires running TimescaleDB)
    db := testdb.Connect(t) // uses GOGROWATT_TEST_DB_URL or defaults
    testdb.Reset(t, db)     // run migrations, truncate growatt.* tables

    // 3. Create client pointing at simulator
    client := growatt.NewClient("test-harness-token-abc123",
        growatt.WithBaseURL(server.URL+"/v1/"),
        growatt.WithRateLimit(0),
    )

    // 4. Run fetcher
    fetcher := fetch.New(client, db, fetch.Config{
        DeviceSN: "ABC123456",
        Timezone: "US/Central",
    })
    ctx := context.Background()
    err := fetcher.FetchDate(ctx, time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC))
    require.NoError(t, err)

    // 5. Start REST API
    apiServer := api.NewServer(db)
    apiTS := httptest.NewServer(apiServer.Handler())
    defer apiTS.Close()

    // 6. Validate via REST API using canonical endpoint from doc 03
    resp := httpGet(t, apiTS.URL+"/api/v1/devices/ABC123456/power?from=2026-02-14&to=2026-02-14")
    var envelope struct {
        Data struct {
            SerialNumber string `json:"serial_number"`
            From         string `json:"from"`
            To           string `json:"to"`
            Interval     string `json:"interval"`
            Timezone     string `json:"timezone"`
            Fields       []string `json:"fields"`
            Readings     []struct {
                Time string  `json:"time"`
                Pac  float64 `json:"pac"`
            } `json:"readings"`
        } `json:"data"`
        Pagination struct {
            Page       int `json:"page"`
            PerPage    int `json:"per_page"`
            Total      int `json:"total"`
            TotalPages int `json:"total_pages"`
        } `json:"pagination"`
        Meta struct {
            Timestamp string `json:"timestamp"`
            RequestID string `json:"request_id"`
        } `json:"meta"`
    }
    json.Unmarshal(resp, &envelope)

    // Validate response envelope structure
    assert.NotEmpty(t, envelope.Meta.RequestID)
    assert.NotEmpty(t, envelope.Meta.Timestamp)
    assert.Equal(t, "ABC123456", envelope.Data.SerialNumber)

    // Validate data completeness
    assert.GreaterOrEqual(t, len(envelope.Data.Readings), 80) // ~100 expected for full day
    assertDoublingPattern(t, envelope.Data.Readings)
    assertNoGaps(t, envelope.Data.Readings, 10*time.Minute)
}
```

---

## 7. Performance Tests

### 7.1 Bulk Data Generation

```go
func TestPerformance_BulkHistoricalLoad(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping performance test")
    }

    sim := simulator.New(simulator.Config{
        Scenario:  simulator.ClearDay,
        Seed:      100,
        DateRange: simulator.DateRange{
            From: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
            To:   time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC),
        },
    })
    // ... load 365 days of data (365 * ~100 readings = ~36,500 rows)

    // Benchmark: expect < 30 seconds for full year load
    start := time.Now()
    err := fetcher.FetchRange(ctx, from, to)
    elapsed := time.Since(start)
    assert.Less(t, elapsed, 30*time.Second)
    t.Logf("Loaded 365 days in %v", elapsed)
}
```

### 7.2 Query Latency Benchmarks

| Query | Target P50 | Target P99 |
|---|---|---|
| Single day power readings | < 5 ms | < 20 ms |
| Hourly aggregation (1 day) | < 10 ms | < 50 ms |
| Daily rollup (30 days) | < 20 ms | < 100 ms |
| Monthly rollup (12 months) | < 50 ms | < 200 ms |
| Date range scan (365 days raw) | < 100 ms | < 500 ms |

### 7.3 Concurrency Test

```go
func TestPerformance_ConcurrentQueries(t *testing.T) {
    // Pre-load 30 days of data
    // Launch 50 concurrent goroutines querying random dates
    // Assert: no errors, no deadlocks, all responses < 200ms
    // Assert: total throughput > 500 queries/sec
}
```

---

## 8. Simulator Control API

The simulator exposes a `/_control` endpoint (not part of Growatt API) for test orchestration:

| Method | Path | Body | Effect |
|---|---|---|---|
| POST | `/_control/scenario` | `{"scenario": "cloudy_day", "seed": 43}` | Switch data generation scenario |
| POST | `/_control/error` | `{"type": "rate_limit", "duration_secs": 30}` | Inject error for next N seconds |
| POST | `/_control/reset` | `{}` | Clear all state, reset request counters |
| GET | `/_control/stats` | | Return `{"requests": 42, "errors_injected": 3}` |
| POST | `/_control/advance` | `{"minutes": 60}` | Advance simulated clock |

This allows the test runner to dynamically reconfigure the simulator between test phases without restarting the server.

---

## 9. Database Test Utilities

### 9.1 Schema Reset

```go
// testdb.Reset drops and recreates the growatt schema, runs migrations.
func Reset(t *testing.T, db *sql.DB) {
    t.Helper()
    // DROP SCHEMA IF EXISTS growatt CASCADE
    // Run migration files in order (creates growatt schema, tables, hypertables)
    // Verify growatt.power_readings, growatt.energy_summaries, etc. exist
}
```

### 9.2 Direct Validation Queries

```go
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

// GetReading returns the pac_w value for a specific device, date, and time.
func GetReading(t *testing.T, db *sql.DB, deviceSN, dateTime string) float64 {
    t.Helper()
    var pacW float64
    err := db.QueryRow(`
        SELECT pac_w FROM growatt.power_readings
        WHERE device_sn = $1 AND ts = $2::timestamptz
    `, deviceSN, dateTime).Scan(&pacW)
    require.NoError(t, err)
    return pacW
}

// GetHourlyAgg returns the hourly aggregation for a device, date, and hour.
func GetHourlyAgg(t *testing.T, db *sql.DB, deviceSN, date string, hour int) HourlyRow {
    t.Helper()
    var row HourlyRow
    err := db.QueryRow(`
        SELECT
            avg(pac_w) AS avg_pac_w,
            min(pac_w) AS min_pac_w,
            max(pac_w) AS max_pac_w,
            count(*)   AS samples
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
```

---

## 10. CI/CD Integration

```mermaid
graph LR
    subgraph "CI Pipeline (GitHub Actions)"
        PUSH[Push / PR]
        LINT[go vet + staticcheck]
        UNIT[go test -short ./...]
        BUILD[Build binaries]
        COMPOSE[docker compose -f docker-compose.test.yml up]
        INTEG[Integration tests run inside container]
        PERF[Performance benchmarks<br/>go test -bench -run=^$ ./integration/...]
        REPORT[Upload test results + coverage]
    end

    PUSH --> LINT
    LINT --> UNIT
    UNIT --> BUILD
    BUILD --> COMPOSE
    COMPOSE --> INTEG
    INTEG --> PERF
    PERF --> REPORT
```

### 10.1 GitHub Actions Workflow

```yaml
name: Integration Tests
on: [push, pull_request]

jobs:
  unit-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.21' }
      - run: go test -short -race -coverprofile=coverage.txt ./...
      - uses: codecov/codecov-action@v4

  integration-tests:
    runs-on: ubuntu-latest
    needs: unit-tests
    services:
      postgres:
        image: timescale/timescaledb:latest-pg16
        env:
          POSTGRES_DB: gogrowatt_test
          POSTGRES_USER: test
          POSTGRES_PASSWORD: test
        ports: ['5433:5432']
        options: >-
          --health-cmd pg_isready
          --health-interval 10s
          --health-timeout 5s
          --health-retries 5
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.21' }
      - run: go test -race -tags=integration -v ./integration/...
        env:
          GOGROWATT_TEST_DB_URL: postgres://test:test@localhost:5433/gogrowatt_test?sslmode=disable

  performance-benchmarks:
    runs-on: ubuntu-latest
    needs: integration-tests
    if: github.ref == 'refs/heads/main'
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.21' }
      - run: go test -bench=. -benchmem -run=^$ -tags=integration ./integration/...
        env:
          GOGROWATT_TEST_DB_URL: postgres://test:test@localhost:5433/gogrowatt_test?sslmode=disable
      - uses: benchmark-action/github-action-benchmark@v1
        with:
          tool: go
          output-file-path: benchmark-results.txt
```

### 10.2 Test Tags

| Tag | Scope | Duration | Requires |
|---|---|---|---|
| (none) | Unit tests only | < 30s | Nothing |
| `-short` | Skip integration | < 10s | Nothing |
| `-tags=integration` | Unit + integration | < 5 min | TimescaleDB |
| `-tags=integration -bench` | Benchmarks | < 10 min | TimescaleDB |

---

## 11. File Structure

```
gogrowatt/
  integration/
    testharness/
      simulator.go          # Simulated Growatt API server
      simulator_test.go     # Tests for the simulator itself
      datagenerator.go      # Solar curve + cloud + doubling models
      datagenerator_test.go # Verify generated data matches real patterns
      scenarios.go          # Scenario definitions and state machines
      config.go             # SimulatorConfig, ErrorEvent types
    fetcher/
      fetcher_test.go       # Integration tests driving fetcher -> sim -> db
    api/
      api_test.go           # REST API validation tests
    performance/
      bench_test.go         # Bulk load and query benchmarks
    testdb/
      testdb.go             # Database utilities (reset, assert, query helpers)
    testdata/
      golden/
        clear_day_seed42.json   # Golden file: expected output for seed 42 clear day
        cloudy_day_seed43.json
        night_only_seed50.json
        with_doubling_seed42.json
        flat_top_seed51.json
  docker-compose.test.yml
```

---

## 12. Reproducibility Guarantees

All data generation uses a seeded `math/rand.Source`. For any given `(scenario, seed, date)` tuple, the generator produces identical output across runs, platforms, and Go versions.

Golden file tests verify this:

```go
func TestDataGenerator_Determinism(t *testing.T) {
    gen1 := NewGenerator(Config{Scenario: ClearDay, Seed: 42})
    gen2 := NewGenerator(Config{Scenario: ClearDay, Seed: 42})

    date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
    data1 := gen1.GenerateDay(date)
    data2 := gen2.GenerateDay(date)

    assert.Equal(t, data1, data2, "same seed must produce identical output")

    // Compare against golden file
    golden := loadGoldenFile(t, "golden/clear_day_seed42.json")
    assert.Equal(t, golden, data1, "output must match golden file")
}
```

---

## 13. Summary of Real Data Patterns Encoded

The following table captures the specific values extracted from the real CSV data that the simulator must faithfully reproduce:

| Pattern | Source File | Value | Generator Parameter |
|---|---|---|---|
| Morning ramp start | `power_2026-02-15.csv` line 2 | 07:28, 0.00 W | `sunrise_offset = -10 min` |
| Single-string peak | `power_2026-02-14.csv` line 23 | 12:51, 2729.5 W | `single_string_peak = 2750 W` |
| Doubling transition | `power_2026-02-14.csv` line 24 | 12:56, 5223.3 W | `doubling_time = 12:55, factor = 1.91` |
| Post-doubling peak | `power_2026-02-12.csv` line 33 | 13:40, 5403.6 W | `dual_string_peak = 5400 W` |
| Cloudy swing range | `power_2026-02-13.csv` lines 137-142 | 599.5 - 5526.8 W in hour 14 | `cloud_variance = 0.85` |
| Evening tail | `power_2026-02-14.csv` lines 97-101 | 0.7, 1.9, 0.2, 0.1, 3.3 W | `tail_readings = 5, tail_max = 3.5 W` |
| Interval jitter | All files | 4-5 min offset from :00 | `time_offset_minutes = rand(0, 5)` |
| Readings per day | `hourly_2026-02-14.csv` | 100 readings (11:05 - 19:21) | `readings_per_day ~ 100` |
| Samples per active hour | `hourly_2026-02-14.csv` | 11-12 per hour | `samples_per_hour = 12` |

---

## 14. Testing the Test Harness

This section describes how to verify the test harness infrastructure itself. Since the harness is a substantial piece of software (simulator, data generator, control API, golden files), it requires its own testing strategy to ensure confidence in test results.

### 14.1 Test the Simulator Itself

Verify that each simulator endpoint returns responses matching the real Growatt API format.

```go
func TestSimulator_PlantList(t *testing.T) {
    sim := simulator.New(simulator.Config{
        PlantID:  "12345",
        PlantName: "Home Solar",
        DeviceSN: "ABC123456",
    })
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    req, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
    req.Header.Set("token", "test-harness-token-abc123")
    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    require.Equal(t, 200, resp.StatusCode)

    var body struct {
        ErrorCode int    `json:"error_code"`
        ErrorMsg  string `json:"error_msg"`
        Data      struct {
            Plants []struct {
                PlantID   string `json:"plant_id"`
                PlantName string `json:"plant_name"`
            } `json:"plants"`
        } `json:"data"`
    }
    require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
    assert.Equal(t, 0, body.ErrorCode)
    assert.Len(t, body.Data.Plants, 1)
    assert.Equal(t, "12345", body.Data.Plants[0].PlantID)
}

func TestSimulator_TlxData(t *testing.T) {
    sim := simulator.New(simulator.Config{
        Scenario: simulator.WithDoubling,
        Seed:     42,
        DeviceSN: "ABC123456",
    })
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // POST device/tlx/tlx_data with valid parameters
    form := url.Values{
        "tlx_sn":      {"ABC123456"},
        "start_date":  {"2026-02-14"},
        "end_date":    {"2026-02-14"},
        "timezone_id": {"US/Central"},
        "page":        {"1"},
        "perpage":     {"200"},
    }
    req, _ := http.NewRequest("POST", server.URL+"/v1/device/tlx/tlx_data", strings.NewReader(form.Encode()))
    req.Header.Set("token", "test-harness-token-abc123")
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)

    var body struct {
        ErrorCode int    `json:"error_code"`
        Data      struct {
            Datas []struct {
                Time string  `json:"time"`
                Pac  float64 `json:"pac"`
                Ppv  float64 `json:"ppv"`
                Vpv1 float64 `json:"vpv1"`
                Ipv1 float64 `json:"ipv1"`
            } `json:"datas"`
            Count int `json:"count"`
        } `json:"data"`
    }
    require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
    assert.Equal(t, 0, body.ErrorCode)
    assert.Greater(t, body.Data.Count, 0, "should return data points for a full day")

    // Verify time format matches real Growatt: "YYYY-MM-DD HH:MM"
    for _, dp := range body.Data.Datas {
        assert.Regexp(t, `^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$`, dp.Time)
    }
}

func TestSimulator_InvalidToken(t *testing.T) {
    sim := simulator.New(simulator.Config{DeviceSN: "ABC123456"})
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    req, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
    req.Header.Set("token", "wrong-token")
    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)

    var body struct {
        ErrorCode int    `json:"error_code"`
        ErrorMsg  string `json:"error_msg"`
    }
    require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
    assert.Equal(t, 10011, body.ErrorCode)
    assert.Equal(t, "error_permission_denied", body.ErrorMsg)
}
```

### 14.2 Test the Data Generator

Verify determinism (same seed produces identical output) and that the doubling pattern appears at the correct time.

```go
func TestDataGenerator_Determinism(t *testing.T) {
    seeds := []int64{42, 43, 100, 999}
    scenarios := []ScenarioType{ClearDay, CloudyDay, WithDoubling, FlatTop, NightOnly}

    for _, seed := range seeds {
        for _, scenario := range scenarios {
            t.Run(fmt.Sprintf("seed=%d/scenario=%s", seed, scenario), func(t *testing.T) {
                gen1 := NewGenerator(Config{Scenario: scenario, Seed: seed})
                gen2 := NewGenerator(Config{Scenario: scenario, Seed: seed})

                date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
                data1 := gen1.GenerateDay(date)
                data2 := gen2.GenerateDay(date)

                require.Equal(t, data1, data2,
                    "identical (scenario=%s, seed=%d) must produce identical output", scenario, seed)
            })
        }
    }
}

func TestDataGenerator_DifferentSeedsDiffer(t *testing.T) {
    gen1 := NewGenerator(Config{Scenario: ClearDay, Seed: 42})
    gen2 := NewGenerator(Config{Scenario: ClearDay, Seed: 43})

    date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
    data1 := gen1.GenerateDay(date)
    data2 := gen2.GenerateDay(date)

    assert.NotEqual(t, data1, data2, "different seeds must produce different output")
}

func TestDataGenerator_DoublingPattern(t *testing.T) {
    gen := NewGenerator(Config{
        Scenario:    WithDoubling,
        Seed:        42,
        PeakPowerKW: 9.0,
        Latitude:    30.2672,
        Longitude:   -97.7431,
        Timezone:    "US/Central",
    })

    date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
    points := gen.GenerateDay(date)

    // Find the doubling transition: look for the largest jump between consecutive readings
    var maxJump float64
    var jumpTime string
    for i := 1; i < len(points); i++ {
        jump := points[i].Pac - points[i-1].Pac
        if jump > maxJump {
            maxJump = jump
            jumpTime = points[i].Time
        }
    }

    // The doubling should occur near 12:50-13:00
    assert.Greater(t, maxJump, 2000.0, "doubling jump should be > 2000 W")
    assert.Contains(t, jumpTime, "12:5", "doubling should occur around 12:5x")
}
```

### 14.3 Test Each Scenario Independently

Each scenario should be tested in isolation to verify it produces the expected shape.

```go
func TestScenario_ClearDay(t *testing.T) {
    gen := NewGenerator(Config{Scenario: ClearDay, Seed: 42, PeakPowerKW: 9.0,
        Latitude: 30.2672, Longitude: -97.7431, Timezone: "US/Central"})
    points := gen.GenerateDay(time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC))

    // Clear day: low variance, smooth bell curve
    var maxPac float64
    for _, p := range points {
        if p.Pac > maxPac { maxPac = p.Pac }
    }
    assert.InDelta(t, 2750.0, maxPac, 500.0, "clear day single-string peak ~2750 W")

    // Verify smoothness: consecutive readings should not jump more than 15%
    for i := 1; i < len(points); i++ {
        if points[i-1].Pac > 100 { // skip near-zero readings
            ratio := points[i].Pac / points[i-1].Pac
            assert.InDelta(t, 1.0, ratio, 0.15,
                "clear day should be smooth; jump at %s", points[i].Time)
        }
    }
}

func TestScenario_CloudyDay(t *testing.T) {
    gen := NewGenerator(Config{Scenario: CloudyDay, Seed: 43, PeakPowerKW: 9.0,
        Latitude: 30.2672, Longitude: -97.7431, Timezone: "US/Central"})
    points := gen.GenerateDay(time.Date(2026, 2, 13, 0, 0, 0, 0, time.UTC))

    // Cloudy day: high variance
    var values []float64
    for _, p := range points {
        if p.Pac > 100 { values = append(values, p.Pac) }
    }
    stddev := calcStddev(values)
    assert.Greater(t, stddev, 500.0, "cloudy day should have high variance")
}

func TestScenario_NightOnly(t *testing.T) {
    gen := NewGenerator(Config{Scenario: NightOnly, Seed: 50})
    points := gen.GenerateDay(time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC))

    assert.Empty(t, points, "night-only scenario should produce no data points")
}

func TestScenario_WithDoubling(t *testing.T) {
    gen := NewGenerator(Config{Scenario: WithDoubling, Seed: 42, PeakPowerKW: 9.0,
        Latitude: 30.2672, Longitude: -97.7431, Timezone: "US/Central"})
    points := gen.GenerateDay(time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC))

    // Should have both single-string and dual-string readings
    var preDbl, postDbl []float64
    for _, p := range points {
        hour, _ := strconv.Atoi(p.Time[11:13])
        if hour >= 11 && hour < 13 { preDbl = append(preDbl, p.Pac) }
        if hour >= 13 && hour < 15 { postDbl = append(postDbl, p.Pac) }
    }
    avgPre := mean(preDbl)
    avgPost := mean(postDbl)
    ratio := avgPost / avgPre
    assert.InDelta(t, 1.9, ratio, 0.3, "post-doubling should be ~1.9x pre-doubling")
}

func TestScenario_FlatTop(t *testing.T) {
    gen := NewGenerator(Config{Scenario: FlatTop, Seed: 51, PeakPowerKW: 9.0,
        Latitude: 30.2672, Longitude: -97.7431, Timezone: "US/Central"})
    points := gen.GenerateDay(time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC))

    // Flat top: should clip at inverter max for sustained period
    var clippedCount int
    maxWatts := 9.0 * 1000 // peak_kw * 1000
    for _, p := range points {
        if p.Pac >= maxWatts*0.98 { clippedCount++ }
    }
    assert.Greater(t, clippedCount, 5, "flat top should have multiple clipped readings")
}
```

### 14.4 Test Error Injection

Verify that rate-limit responses return the correct Growatt error code 10012 and a message containing "frequently".

```go
func TestErrorInjection_RateLimit(t *testing.T) {
    sim := simulator.New(simulator.Config{
        DeviceSN: "ABC123456",
        Scenario: simulator.ClearDay,
        Seed:     42,
        ErrorSchedule: []simulator.ErrorEvent{
            {
                TriggerAfterRequests: 2,
                ErrorType:            simulator.ErrTypeRateLimit,
                Duration:             10 * time.Second,
            },
        },
    })
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    token := "test-harness-token-abc123"

    // First two requests succeed
    for i := 0; i < 2; i++ {
        req, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
        req.Header.Set("token", token)
        resp, err := http.DefaultClient.Do(req)
        require.NoError(t, err)
        var body map[string]interface{}
        json.NewDecoder(resp.Body).Decode(&body)
        assert.Equal(t, float64(0), body["error_code"])
    }

    // Third request should be rate limited
    req, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
    req.Header.Set("token", token)
    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)

    var body struct {
        ErrorCode int    `json:"error_code"`
        ErrorMsg  string `json:"error_msg"`
    }
    require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
    assert.Equal(t, 10012, body.ErrorCode, "rate limit must return error_code 10012")
    assert.Contains(t, body.ErrorMsg, "frequently",
        "rate limit message must contain 'frequently'")
}

func TestErrorInjection_ServerDown(t *testing.T) {
    sim := simulator.New(simulator.Config{
        DeviceSN: "ABC123456",
        ErrorSchedule: []simulator.ErrorEvent{
            {
                TriggerAfterRequests: 1,
                ErrorType:            simulator.ErrTypeServerDown,
                Duration:             5 * time.Second,
                HTTPStatus:           503,
            },
        },
    })
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // First request succeeds
    req, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
    req.Header.Set("token", "test-harness-token-abc123")
    resp, _ := http.DefaultClient.Do(req)
    assert.Equal(t, 200, resp.StatusCode)

    // Second request gets 503
    req2, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
    req2.Header.Set("token", "test-harness-token-abc123")
    resp2, _ := http.DefaultClient.Do(req2)
    assert.Equal(t, 503, resp2.StatusCode)
}
```

### 14.5 Test the Control API

Verify that `POST /_control/scenario` switches the data generator and `GET /_control/stats` tracks request counts.

```go
func TestControlAPI_SwitchScenario(t *testing.T) {
    sim := simulator.New(simulator.Config{
        Scenario: simulator.ClearDay,
        Seed:     42,
        DeviceSN: "ABC123456",
    })
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // Switch to cloudy day
    body := `{"scenario": "cloudy_day", "seed": 43}`
    resp, err := http.Post(server.URL+"/_control/scenario",
        "application/json", strings.NewReader(body))
    require.NoError(t, err)
    assert.Equal(t, 200, resp.StatusCode)

    // Fetch data and verify it now has cloudy-day characteristics
    // (high variance, erratic swings)
    form := url.Values{
        "tlx_sn":      {"ABC123456"},
        "start_date":  {"2026-02-13"},
        "end_date":    {"2026-02-13"},
        "timezone_id": {"US/Central"},
        "page":        {"1"},
        "perpage":     {"200"},
    }
    req, _ := http.NewRequest("POST", server.URL+"/v1/device/tlx/tlx_data",
        strings.NewReader(form.Encode()))
    req.Header.Set("token", "test-harness-token-abc123")
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    dataResp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    assert.Equal(t, 200, dataResp.StatusCode)
    // Decode and verify cloudy characteristics (high variance)
}

func TestControlAPI_Stats(t *testing.T) {
    sim := simulator.New(simulator.Config{DeviceSN: "ABC123456"})
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // Make a few requests
    for i := 0; i < 3; i++ {
        req, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
        req.Header.Set("token", "test-harness-token-abc123")
        http.DefaultClient.Do(req)
    }

    // Check stats
    resp, err := http.Get(server.URL + "/_control/stats")
    require.NoError(t, err)
    var stats struct {
        Requests       int `json:"requests"`
        ErrorsInjected int `json:"errors_injected"`
    }
    require.NoError(t, json.NewDecoder(resp.Body).Decode(&stats))
    assert.Equal(t, 3, stats.Requests)
    assert.Equal(t, 0, stats.ErrorsInjected)
}

func TestControlAPI_Reset(t *testing.T) {
    sim := simulator.New(simulator.Config{DeviceSN: "ABC123456"})
    server := httptest.NewServer(sim.Handler())
    defer server.Close()

    // Make some requests
    for i := 0; i < 5; i++ {
        req, _ := http.NewRequest("GET", server.URL+"/v1/plant/list", nil)
        req.Header.Set("token", "test-harness-token-abc123")
        http.DefaultClient.Do(req)
    }

    // Reset
    resp, err := http.Post(server.URL+"/_control/reset",
        "application/json", strings.NewReader("{}"))
    require.NoError(t, err)
    assert.Equal(t, 200, resp.StatusCode)

    // Verify stats are zeroed
    statsResp, _ := http.Get(server.URL + "/_control/stats")
    var stats struct {
        Requests       int `json:"requests"`
        ErrorsInjected int `json:"errors_injected"`
    }
    json.NewDecoder(statsResp.Body).Decode(&stats)
    assert.Equal(t, 0, stats.Requests)
}
```

### 14.6 Verify Golden Files

Golden files are the source of truth for deterministic output. Run the generator with a known seed and compare output to stored golden files.

```go
func TestGoldenFiles(t *testing.T) {
    tests := []struct {
        scenario   ScenarioType
        seed       int64
        goldenFile string
    }{
        {ClearDay, 42, "golden/clear_day_seed42.json"},
        {CloudyDay, 43, "golden/cloudy_day_seed43.json"},
        {NightOnly, 50, "golden/night_only_seed50.json"},
        {WithDoubling, 42, "golden/with_doubling_seed42.json"},
        {FlatTop, 51, "golden/flat_top_seed51.json"},
    }

    date := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)

    for _, tt := range tests {
        t.Run(tt.goldenFile, func(t *testing.T) {
            gen := NewGenerator(Config{
                Scenario:    tt.scenario,
                Seed:        tt.seed,
                PeakPowerKW: 9.0,
                Latitude:    30.2672,
                Longitude:   -97.7431,
                Timezone:    "US/Central",
            })
            actual := gen.GenerateDay(date)

            if *updateGolden {
                // When run with -update-golden flag, write new golden files
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

// loadGoldenFile reads a golden file from integration/testdata/.
func loadGoldenFile(t *testing.T, name string) []DataPoint {
    t.Helper()
    data, err := os.ReadFile(filepath.Join("testdata", name))
    require.NoError(t, err, "golden file %s not found; run with -update-golden to create", name)
    var points []DataPoint
    require.NoError(t, json.Unmarshal(data, &points))
    return points
}

// writeGoldenFile writes a golden file to integration/testdata/.
func writeGoldenFile(t *testing.T, name string, points []DataPoint) {
    t.Helper()
    dir := filepath.Join("testdata", filepath.Dir(name))
    require.NoError(t, os.MkdirAll(dir, 0o755))
    data, err := json.MarshalIndent(points, "", "  ")
    require.NoError(t, err)
    require.NoError(t, os.WriteFile(filepath.Join("testdata", name), data, 0o644))
    t.Logf("Updated golden file: %s", name)
}

var updateGolden = flag.Bool("update-golden", false, "update golden test files")
```

### 14.7 Test the Full Pipeline End-to-End

Verify the complete path: simulator -> fetcher -> DB -> REST API -> assertions.

```go
func TestFullPipeline_EndToEnd(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping end-to-end pipeline test in short mode")
    }

    // 1. Start simulator with known scenario
    sim := simulator.New(simulator.Config{
        Scenario:    simulator.WithDoubling,
        Seed:        42,
        PlantID:     "12345",
        DeviceSN:    "ABC123456",
        PeakPowerKW: 9.0,
        Latitude:    30.2672,
        Longitude:   -97.7431,
        Timezone:    "US/Central",
    })
    simServer := httptest.NewServer(sim.Handler())
    defer simServer.Close()

    // 2. Connect to TimescaleDB and reset schema
    db := testdb.Connect(t)
    testdb.Reset(t, db)

    // 3. Create Growatt client pointing at simulator
    client := growatt.NewClient("test-harness-token-abc123",
        growatt.WithBaseURL(simServer.URL+"/v1/"),
        growatt.WithRateLimit(0),
    )

    // 4. Run fetcher for a specific date
    fetcher := fetch.New(client, db, fetch.Config{
        DeviceSN: "ABC123456",
        Timezone: "US/Central",
    })
    ctx := context.Background()
    targetDate := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
    require.NoError(t, fetcher.FetchDate(ctx, targetDate))

    // 5. Verify data landed in growatt.power_readings
    count := testdb.CountReadings(t, db, "ABC123456", "2026-02-14")
    assert.GreaterOrEqual(t, count, 80, "expected ~100 readings for full day")

    // 6. Verify column values directly in DB
    var pacW, ppvW, vpv1V float64
    err := db.QueryRow(`
        SELECT pac_w, ppv_w, vpv1_v FROM growatt.power_readings
        WHERE device_sn = 'ABC123456'
        ORDER BY ts DESC LIMIT 1
    `).Scan(&pacW, &ppvW, &vpv1V)
    require.NoError(t, err)
    assert.Greater(t, pacW, 0.0)

    // 7. Start REST API and validate via HTTP
    apiServer := api.NewServer(db)
    apiTS := httptest.NewServer(apiServer.Handler())
    defer apiTS.Close()

    // 7a. Query raw 5-min readings
    resp := httpGet(t, apiTS.URL+"/api/v1/devices/ABC123456/power?from=2026-02-14&to=2026-02-14")
    var envelope struct {
        Data struct {
            Readings []struct {
                Time string  `json:"time"`
                Pac  float64 `json:"pac"`
            } `json:"readings"`
        } `json:"data"`
        Pagination struct {
            Total int `json:"total"`
        } `json:"pagination"`
        Meta struct {
            RequestID string `json:"request_id"`
        } `json:"meta"`
    }
    require.NoError(t, json.Unmarshal(resp, &envelope))
    assert.GreaterOrEqual(t, envelope.Pagination.Total, 80)
    assert.NotEmpty(t, envelope.Meta.RequestID)

    // 7b. Verify doubling pattern in REST response
    assertDoublingPattern(t, envelope.Data.Readings)

    // 7c. Query hourly aggregation
    aggResp := httpGet(t, apiTS.URL+
        "/api/v1/devices/ABC123456/power?from=2026-02-14&to=2026-02-14&interval=1h")
    var aggEnvelope struct {
        Data struct {
            Readings []struct {
                Time string `json:"time"`
                Pac  struct {
                    Avg     float64 `json:"avg"`
                    Min     float64 `json:"min"`
                    Max     float64 `json:"max"`
                    Samples int     `json:"samples"`
                } `json:"pac"`
            } `json:"readings"`
        } `json:"data"`
    }
    require.NoError(t, json.Unmarshal(aggResp, &aggEnvelope))
    assert.Greater(t, len(aggEnvelope.Data.Readings), 0)

    // 7d. Verify aggregation against direct DB query
    for _, reading := range aggEnvelope.Data.Readings {
        if reading.Pac.Samples > 0 {
            assert.Greater(t, reading.Pac.Avg, 0.0)
            assert.LessOrEqual(t, reading.Pac.Min, reading.Pac.Avg)
            assert.GreaterOrEqual(t, reading.Pac.Max, reading.Pac.Avg)
        }
    }
}
```

### 14.8 How to Run the Test Suite

Run the full integration test suite (requires a running TimescaleDB instance):

```bash
go test -tags=integration ./integration/...
```

Run only the test harness self-tests (simulator, generator, golden files):

```bash
go test -tags=integration -run 'TestSimulator|TestDataGenerator|TestGolden|TestScenario|TestControlAPI|TestErrorInjection' ./integration/testharness/...
```

Run in short mode (skips integration tests that require a database):

```bash
go test -short ./integration/...
```

Run with verbose output for debugging:

```bash
go test -tags=integration -v -run TestFullPipeline ./integration/...
```

### 14.9 How to Run with Docker Compose

Run the full integration suite in containers (no local dependencies required):

```bash
docker compose -f docker-compose.test.yml up --abort-on-container-exit
```

This starts all services (TimescaleDB, simulator, fetcher, REST API, test runner) and exits with the test runner's exit code.

To rebuild images after code changes:

```bash
docker compose -f docker-compose.test.yml build && \
docker compose -f docker-compose.test.yml up --abort-on-container-exit
```

To run a specific test scenario inside the container:

```bash
docker compose -f docker-compose.test.yml run --rm test-runner \
    go test -tags=integration -v -run TestScenario_ClearDay ./integration/...
```

To tear down all containers and volumes (clean slate):

```bash
docker compose -f docker-compose.test.yml down -v
```

### 14.10 How to Add a New Scenario

Follow these steps to add a new data generation scenario to the test harness:

1. **Define the scenario constant** in `integration/testharness/config.go`:

    ```go
    const ScenarioPartialShading ScenarioType = "partial_shading"
    ```

2. **Implement the generation logic** in `integration/testharness/datagenerator.go`. Add a case to the generator's scenario switch:

    ```go
    case ScenarioPartialShading:
        // Apply shading model: reduce string 1 output by 40% during hours 14-16
        if hour >= 14 && hour <= 16 {
            vpv1Factor = 0.6
        }
    ```

3. **Add a scenario test** in `integration/testharness/datagenerator_test.go`:

    ```go
    func TestScenario_PartialShading(t *testing.T) {
        gen := NewGenerator(Config{Scenario: PartialShading, Seed: 60, ...})
        points := gen.GenerateDay(date)
        // Assert string 1 voltage drops during shading hours
        // Assert total power reduction matches expected shading loss
    }
    ```

4. **Generate the golden file** by running the tests with the `-update-golden` flag:

    ```bash
    go test -tags=integration -run TestGoldenFiles -update-golden ./integration/testharness/...
    ```

5. **Add the golden file entry** to the `TestGoldenFiles` test table:

    ```go
    {PartialShading, 60, "golden/partial_shading_seed60.json"},
    ```

6. **Add an integration test row** to the scenario test matrix (section 5.2) and implement the corresponding fetcher/API-level integration test in `integration/fetcher/fetcher_test.go`.

7. **Register the scenario in the control API** so Docker-based tests can switch to it dynamically via `POST /_control/scenario`.

8. **Run the full suite** to verify nothing is broken:

    ```bash
    go test -tags=integration ./integration/...
    ```

### 14.11 How to Update Golden Files When the Generator Changes

When the data generator algorithm changes (new cloud model, adjusted doubling factor, different noise distribution, etc.), golden files must be regenerated:

1. **Make the generator change** in `integration/testharness/datagenerator.go`.

2. **Run golden file tests to see what changed**:

    ```bash
    go test -tags=integration -run TestGoldenFiles -v ./integration/testharness/...
    ```

    This will show diffs between the current golden files and the new generator output.

3. **Review the diffs carefully.** Verify that the changes are intentional and the new output still matches the expected real-world patterns from the CSV data (section 13).

4. **Regenerate all golden files**:

    ```bash
    go test -tags=integration -run TestGoldenFiles -update-golden ./integration/testharness/...
    ```

5. **Re-run the full test suite** to confirm everything passes with the new golden files:

    ```bash
    go test -tags=integration ./integration/...
    ```

6. **Commit the updated golden files alongside the generator change** in the same commit, so the history clearly ties the golden file update to the algorithm change:

    ```bash
    git add integration/testharness/datagenerator.go integration/testdata/golden/
    git commit -m "Update data generator: <describe change>; regenerate golden files"
    ```

7. **Never update golden files without understanding why they changed.** If `TestGoldenFiles` fails unexpectedly (no intentional generator change), this indicates a regression -- investigate rather than blindly regenerating.
