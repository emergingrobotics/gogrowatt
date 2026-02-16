# Data Fetcher Service Design

## Overview

The data fetcher is a long-running Go daemon (`gogrowatt-fetcher`) that continuously polls the Growatt OpenAPI for 5-minute interval power data and writes it to PostgreSQL. It reuses the existing `pkg/growatt` client library -- specifically `Client.GetMINInverterHistory()` which calls the `device/tlx/tlx_data` POST endpoint -- and adds adaptive timing, overlap-based gap filling, upsert conflict resolution, and automatic backfill on startup.

### Existing Client Surface Used

| Method | Endpoint | Verb | Notes |
|---|---|---|---|
| `GetMINInverterHistory` | `device/tlx/tlx_data` | POST (form) | Returns `MINHistoryResponse{Count, Datas[]MINHistoryDataPoint}` for a single calendar day. Fields: `time`, `pac`, `ppv`, `vpv1/2`, `ipv1/2`, `vac1`, `iac1`. Max 100 rows per page. |
| `GetMINInverterHistoryRange` | (wrapper) | -- | Iterates day-by-day calling the above. Already handles context cancellation. |
| `ListPlants` | `plant/list` | GET | Used at startup for auto-discovery. |
| `ListDevices` | `device/list` | GET | Used at startup for auto-discovery. |

Key constraints from `client.go`:
- Built-in rate limiter: `enforceRateLimit()` sleeps to maintain `DefaultRateLimit = 3 * time.Second` between calls.
- Token passed via `token` HTTP header.
- API responses wrapped in `Response[T]{ErrorCode, ErrorMsg, Data}`.
- Rate-limit error: code `10012` with message containing "frequently" (see `errors.go:IsRateLimited`).

---

## 1. Overall Architecture

```mermaid
flowchart TB
    subgraph "gogrowatt-fetcher daemon"
        CFG[Configuration Loader]
        SCHED[Scheduler / Ticker]
        FETCH[Fetch Orchestrator]
        ADAPT[Adaptive Timer]
        UPSERT[Upsert Writer]
        META[Metadata Tracker]
        BACKFILL[Backfill Manager]
        HEALTH[Health / Metrics]
    end

    subgraph "External"
        API[Growatt OpenAPI<br/>device/tlx/tlx_data]
        PG[(PostgreSQL)]
    end

    CFG --> SCHED
    CFG --> FETCH
    SCHED -->|tick| FETCH
    FETCH -->|HTTP POST| API
    API -->|MINHistoryResponse| FETCH
    FETCH --> ADAPT
    ADAPT -->|next fire time| SCHED
    FETCH --> UPSERT
    UPSERT -->|INSERT ON CONFLICT| PG
    FETCH --> META
    META -->|collection_runs insert| PG
    BACKFILL -->|startup| FETCH
    BACKFILL -->|query last timestamp| PG
    HEALTH -->|/healthz, /metrics| HEALTH
```

### Component Responsibilities

| Component | Responsibility |
|---|---|
| **Configuration Loader** | Reads env vars and CLI flags: DB DSN, device SN, plant ID, timezone, poll interval, lookback window. |
| **Scheduler / Ticker** | Fires fetch cycles. Default period is 5 min but adjusted by the Adaptive Timer after each successful fetch. |
| **Fetch Orchestrator** | Calls `GetMINInverterHistory` for the current day (and previous day if near midnight). Applies 1-hour overlap window. |
| **Adaptive Timer** | Examines the latest data point timestamp received and schedules the next fetch for `last_data_ts + 1 minute`. |
| **Upsert Writer** | Writes `MINHistoryDataPoint` rows to `growatt.power_readings` using `INSERT ... ON CONFLICT (ts, device_sn) DO UPDATE`. Latest-fetched data wins. |
| **Metadata Tracker** | Logs each fetch cycle to `growatt.collection_runs` table: source_type, source_id, query dates, points_collected, started_at, finished_at, status, error_message. |
| **Backfill Manager** | On startup, queries the latest `ts` from `growatt.power_readings`. If missing or older than 7 days, backfills day-by-day up to the 7-day API lookback limit. |
| **Health / Metrics** | Optional HTTP endpoint exposing last successful fetch time, error counts, gap count, for monitoring. |

---

## 2. Polling Loop State Machine

```mermaid
stateDiagram-v2
    [*] --> Init
    Init --> Backfill: startup / check DB for last reading
    Backfill --> SteadyState: backfill complete
    Backfill --> Backfill: fetch next day (rate-limited)
    SteadyState --> WaitForTick: schedule next tick
    WaitForTick --> Fetching: tick fires
    Fetching --> ProcessData: API returned data
    Fetching --> RetryWait: transient error
    Fetching --> RateLimitWait: rate-limit error (code 10012)
    RetryWait --> Fetching: retry after exponential backoff
    RateLimitWait --> Fetching: retry after 30s
    ProcessData --> Upserting: parse + validate points
    Upserting --> AdjustTimer: write complete
    AdjustTimer --> WaitForTick: computed next fire time
    Fetching --> Shutdown: context cancelled
    WaitForTick --> Shutdown: context cancelled
    Shutdown --> [*]
```

### State Descriptions

- **Init**: Load configuration, open DB pool, create Growatt client via `NewClient(token)`.
- **Backfill**: Query `SELECT MAX(ts) FROM growatt.power_readings WHERE device_sn = $1`. If NULL or > 7 days ago, iterate from `max(last_reading, now - 7 days)` to today, calling `GetMINInverterHistory` once per day. The existing 3-second rate limiter in `client.go:enforceRateLimit()` spaces these calls automatically.
- **SteadyState**: Normal polling mode. The scheduler fires at adaptive intervals.
- **Fetching**: Call `GetMINInverterHistory(ctx, serial, date, timezone)` for the current calendar day. If the current time is within 65 minutes of midnight, also fetch the previous day (to capture overlap across the day boundary).
- **RetryWait**: Exponential backoff: 5s, 10s, 20s, 40s, capped at 5 minutes. Max 5 retries per cycle.
- **RateLimitWait**: Detected via `growatt.IsRateLimited(err)`. Fixed 30-second wait, then retry. Max 3 rate-limit retries per cycle.
- **ProcessData**: Parse the `MINHistoryResponse.Datas` slice. Validate timestamps, filter to only the overlap window (last 1 hour).
- **Upserting**: Batch `INSERT ... ON CONFLICT` into PostgreSQL (see section 4).
- **AdjustTimer**: Compute next fire time from latest data point (see section 3).

---

## 3. Timing Adjustment Algorithm

The goal: fetch soon after new data becomes available, without hammering the API.

```mermaid
flowchart TD
    A[Fetch returns data points] --> B{Any data points<br/>received?}
    B -->|Yes| C[Find max timestamp<br/>from returned points]
    B -->|No| D[Schedule retry in<br/>5 minutes]
    C --> E["next_fetch = max_ts + 6 minutes<br/>(5-min interval + 1-min buffer)"]
    E --> F{next_fetch<br/>in the past?}
    F -->|Yes| G["Set next_fetch = now + 1 minute<br/>(data is stale, fetch soon)"]
    F -->|No| H{next_fetch > now<br/>+ 10 minutes?}
    H -->|Yes| I["Cap at now + 10 minutes<br/>(safety ceiling)"]
    H -->|No| J[Use computed next_fetch]
    G --> K[Schedule timer]
    I --> K
    J --> K
    D --> K
    K --> L[Log: next fetch at T]
```

### Algorithm in Detail

```
Given:
  data_points    = fetched MINHistoryDataPoint list
  now            = time.Now()
  INTERVAL       = 5 * time.Minute   // Growatt reporting interval
  BUFFER         = 1 * time.Minute   // Wait after expected availability
  MAX_WAIT       = 10 * time.Minute  // Never wait longer than this
  MIN_WAIT       = 1 * time.Minute   // Never wait less than this

Procedure:
  if len(data_points) == 0:
      return now + 5*time.Minute    // no data, try again in 5 min

  latest_ts = max(dp.Time for dp in data_points)  // e.g. "12:05"
  latest_time = parse(current_date + " " + latest_ts, timezone)

  // The NEXT data point should appear at latest_time + INTERVAL
  // We add BUFFER to give the API time to make it available
  next_fetch = latest_time + INTERVAL + BUFFER   // 12:05 + 5m + 1m = 12:11

  if next_fetch < now + MIN_WAIT:
      next_fetch = now + MIN_WAIT                 // data is stale, don't spin

  if next_fetch > now + MAX_WAIT:
      next_fetch = now + MAX_WAIT                 // safety cap

  return next_fetch
```

### Timing Example

| Last Data Point | Expected Next Point | Fetch Scheduled At | Rationale |
|---|---|---|---|
| 12:05 | 12:10 | 12:11 | 12:10 + 1 min buffer |
| 12:10 | 12:15 | 12:16 | 12:15 + 1 min buffer |
| 11:50 (stale) | 11:55 | now + 1 min | Data is old, check again soon |
| None (empty) | unknown | now + 5 min | No data yet, default interval |

---

## 4. Upsert / Conflict Resolution Flow

The Growatt `device/tlx/tlx_data` endpoint returns `MINHistoryDataPoint` structs with fields: `time`, `pac`, `ppv`, `vpv1`, `vpv2`, `ipv1`, `ipv2`, `vac1`, `iac1`. Each fetch retrieves the full current day. We request overlap (the last hour of data) and use "latest fetch wins" semantics.

```mermaid
sequenceDiagram
    participant F as Fetch Orchestrator
    participant P as Parser
    participant DB as PostgreSQL

    F->>P: Raw MINHistoryDataPoint[]
    P->>P: Parse timestamps to UTC<br/>Normalize time format<br/>Attach device_sn + date
    P->>DB: BEGIN transaction
    loop For each batch of 100 points
        P->>DB: INSERT INTO growatt.power_readings<br/>(ts, device_sn, pac_w, ppv_w,<br/>vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a,<br/>fetched_at)<br/>VALUES ($1..$11), ...<br/>ON CONFLICT (ts, device_sn)<br/>DO UPDATE SET<br/>  pac_w = EXCLUDED.pac_w,<br/>  ppv_w = EXCLUDED.ppv_w,<br/>  vpv1_v = EXCLUDED.vpv1_v,<br/>  vpv2_v = EXCLUDED.vpv2_v,<br/>  ipv1_a = EXCLUDED.ipv1_a,<br/>  ipv2_a = EXCLUDED.ipv2_a,<br/>  vac1_v = EXCLUDED.vac1_v,<br/>  iac1_a = EXCLUDED.iac1_a,<br/>  fetched_at = EXCLUDED.fetched_at
        DB-->>P: rows affected
    end
    P->>DB: COMMIT
    P-->>F: UpsertResult{inserted, updated, errors}
```

### Conflict Resolution Rules

1. **Primary key**: `(ts, device_sn)` -- composite unique constraint, time-first for TimescaleDB partitioning.
2. **On conflict**: Unconditional `DO UPDATE SET ... = EXCLUDED.*` -- the latest fetch always overwrites. The `fetched_at` column records when the data was retrieved, providing an audit trail.
3. **Why latest wins**: The Growatt API may retroactively adjust values (e.g., smoothing, correction). The most recent API response is the most accurate.
4. **Batch size**: 100 rows per `INSERT` statement (matches the API's `perpage` max of 100). A typical day has ~288 points (24h * 12 per hour), so 3 batches per day.

### Overlap Window

Each steady-state fetch requests the full current day but only processes points from `now - 1 hour` through the latest available. This provides:
- **Gap filling**: If a previous fetch missed points (network error, API hiccup), the overlap window catches them.
- **Correction**: If the API revised earlier values, the overlap overwrites them.
- **Efficiency**: We still only make one API call per cycle (the endpoint returns the whole day regardless; we filter client-side).

Note: The `device/tlx/tlx_data` endpoint always returns all data points for the requested calendar day. There is no way to request a sub-day range. The 1-hour overlap filtering is done client-side after receiving the response, purely for logging and gap-detection purposes. All received points are upserted regardless.

---

## 5. Error Recovery and Backfill Strategy

```mermaid
flowchart TD
    subgraph "Startup Backfill"
        S1[Query DB: last ts<br/>for this device_sn] --> S2{Last reading<br/>found?}
        S2 -->|No| S3["Set backfill_start = now - 7 days<br/>(API maximum lookback)"]
        S2 -->|Yes| S4{Age of last<br/>reading?}
        S4 -->|"> 7 days"| S3
        S4 -->|"<= 7 days"| S5["Set backfill_start = last_reading.Date()"]
        S3 --> S6[Backfill Loop]
        S5 --> S6
        S6 --> S7["Fetch day N via<br/>GetMINInverterHistory"]
        S7 --> S8[Upsert all points]
        S8 --> S9{More days<br/>to fetch?}
        S9 -->|Yes| S10["Wait for rate limit<br/>(enforced by client)"]
        S10 --> S7
        S9 -->|No| S11[Enter steady-state polling]
    end

    subgraph "Runtime Error Recovery"
        E1[API call fails] --> E2{Error type?}
        E2 -->|Rate limited<br/>IsRateLimited| E3["Wait 30 seconds<br/>Retry up to 3 times"]
        E2 -->|Network / timeout| E4["Exponential backoff<br/>5s, 10s, 20s, 40s, 80s<br/>Max 5 retries"]
        E2 -->|Permission denied<br/>IsPermissionDenied| E5[Log CRITICAL<br/>Alert operator<br/>Continue with backoff]
        E2 -->|Context cancelled| E6[Graceful shutdown]
        E2 -->|Unknown API error| E4
        E3 --> E7{Retry<br/>succeeded?}
        E4 --> E7
        E7 -->|Yes| E8[Process data normally]
        E7 -->|No| E9["Log error<br/>Schedule retry at<br/>next normal interval"]
        E9 --> E10[Record gap in growatt.collection_runs]
    end

    subgraph "Gap Detection"
        G1[After upsert completes] --> G2["Query: SELECT ts<br/>FROM growatt.power_readings<br/>WHERE device_sn = $1<br/>AND ts BETWEEN $2 AND $3<br/>ORDER BY ts"]
        G2 --> G3["Walk timestamps<br/>Expect 5-min intervals"]
        G3 --> G4{Gap > 10 min<br/>found?}
        G4 -->|Yes| G5["Record in growatt.collection_gaps:<br/>device_sn, gap_date, expected_start, expected_end"]
        G4 -->|No| G6[No gaps - nominal]
    end
```

### Retry Strategy Details

| Error Condition | Detection | Wait | Max Retries | Escalation |
|---|---|---|---|---|
| Rate limited | `growatt.IsRateLimited(err)` returns true (code 10012 + "frequently") | Fixed 30s | 3 | Log warning, skip cycle |
| Network error | `err` from `httpClient.Do()` | Exponential: 5s base, 2x | 5 | Log error, skip cycle |
| HTTP timeout | `context.DeadlineExceeded` | Exponential: 5s base, 2x | 5 | Log error, skip cycle |
| Permission denied | `growatt.IsPermissionDenied(err)` (code 10011) | 5 minutes | 1 | Log critical, alert |
| Invalid token | code 10011 | None | 0 | Log fatal, exit |
| DB write error | `pq` / `pgx` error | 5s | 3 | Log error, keep fetching (data will be re-upserted next cycle) |

### Backfill Behavior

On startup, or after a period of downtime:

1. Query `MAX(ts)` from `growatt.power_readings` for the configured device.
2. Compute `backfill_start`:
   - If no data: `now - 7 days` (API hard limit).
   - If last reading exists but is older than 7 days: `now - 7 days`.
   - If last reading is within 7 days: that reading's calendar date.
3. Iterate from `backfill_start` to today, calling `GetMINInverterHistory` once per date. The existing `enforceRateLimit()` in `client.go` ensures a minimum 3-second gap between API calls.
4. Upsert all returned points for each day.
5. A 7-day backfill requires at most 7 API calls = ~21 seconds (7 x 3s rate limit). This is fast enough to run synchronously at startup before entering steady-state.

---

## 6. Configuration Options

### Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `GROWATT_API_KEY` | Yes | -- | API token (passed as `token` header). Used by existing `NewClientFromEnv()`. |
| `GROWATT_DEVICE_SN` | Yes* | auto-detect | MIN/TLX inverter serial number. If omitted, auto-detected via `ListPlants` + `ListDevices` (costs 2 extra API calls). |
| `GROWATT_PLANT_ID` | No | auto-detect | Plant ID. Only needed if multiple plants exist on the account. |
| `GROWATT_TIMEZONE` | No | `US/Central` | Timezone ID passed to the API's `timezone_id` parameter. Must match the inverter's configured timezone. |
| `GROWATT_BASE_URL` | No | `https://openapi.growatt.com/v1/` | API base URL (existing client support). |
| `DATABASE_URL` | Yes | -- | PostgreSQL connection string, e.g. `postgres://user:pass@host:5432/growatt?sslmode=require`. |
| `FETCH_INTERVAL` | No | `5m` | Default polling interval (overridden by adaptive timer). |
| `FETCH_OVERLAP` | No | `1h` | How far back from "now" to consider for gap-filling on each fetch. |
| `BACKFILL_DAYS` | No | `7` | Maximum days to backfill on startup (capped at 7 by API). |
| `HEALTH_PORT` | No | `8080` | Port for the health/metrics HTTP endpoint. |
| `LOG_LEVEL` | No | `info` | Logging level: `debug`, `info`, `warn`, `error`. |

*Auto-detection costs 2 API calls (6 seconds). For a daemon, this is acceptable as a one-time startup cost. The detected values are cached in memory for the lifetime of the process.

### CLI Flags (override env vars)

```
gogrowatt-fetcher \
  --device-sn=ABC123 \
  --timezone=US/Central \
  --db=postgres://localhost:5432/growatt \
  --interval=5m \
  --overlap=1h \
  --backfill-days=7 \
  --health-port=8080 \
  --log-level=info
```

---

## 7. Sequence Diagram: Steady-State Fetch Cycle

```mermaid
sequenceDiagram
    participant Timer as Adaptive Timer
    participant Orch as Fetch Orchestrator
    participant Client as growatt.Client
    participant API as Growatt API
    participant Writer as Upsert Writer
    participant DB as PostgreSQL
    participant Meta as Metadata Tracker

    Timer->>Orch: tick (scheduled time reached)
    Orch->>Orch: Determine target date(s)<br/>(today; also yesterday if near midnight)

    loop For each target date
        Orch->>Client: GetMINInverterHistory(ctx, sn, date, tz)
        Client->>Client: enforceRateLimit() -- sleep if < 3s since last call
        Client->>API: POST device/tlx/tlx_data<br/>{tlx_sn, start_date, end_date, timezone_id, page=1, perpage=100}
        API-->>Client: {error_code:0, data:{count:N, datas:[...]}}
        Client-->>Orch: *PowerData{Date, Powers[]}
    end

    Orch->>Orch: Merge data from all dates<br/>Convert MINHistoryDataPoint to DB rows<br/>Parse "HH:MM" times into full timestamps

    Orch->>Writer: []PowerReading rows
    Writer->>DB: BEGIN
    Writer->>DB: INSERT INTO growatt.power_readings ... ON CONFLICT DO UPDATE<br/>(batched, 100 rows per statement)
    DB-->>Writer: inserted/updated counts
    Writer->>DB: COMMIT
    Writer-->>Orch: UpsertResult{inserted: X, updated: Y}

    Orch->>Meta: LogFetch{source_type, source_id, query_dates, points, status}
    Meta->>DB: INSERT INTO growatt.collection_runs ...

    Orch->>Timer: latest data timestamp = T
    Timer->>Timer: Compute next_fetch = T + 6min<br/>Apply min/max bounds
    Timer-->>Orch: Next fetch at: T+6min
```

---

## 8. Database Tables Referenced

The schema is designed separately (see `01-database-schema.md`). The fetcher depends on these tables, reproduced here exactly as defined in the schema.

### `growatt.power_readings`

```sql
CREATE TABLE growatt.power_readings (
    ts          TIMESTAMPTZ     NOT NULL,
    device_sn   TEXT            NOT NULL,
    pac_w       DOUBLE PRECISION,   -- AC output power (watts)
    ppv_w       DOUBLE PRECISION,   -- total PV input power (watts)
    vpv1_v      DOUBLE PRECISION,   -- PV string 1 voltage (V)
    vpv2_v      DOUBLE PRECISION,   -- PV string 2 voltage (V)
    ipv1_a      DOUBLE PRECISION,   -- PV string 1 current (A)
    ipv2_a      DOUBLE PRECISION,   -- PV string 2 current (A)
    vac1_v      DOUBLE PRECISION,   -- grid AC voltage (V)
    iac1_a      DOUBLE PRECISION,   -- grid AC current (A)
    fetched_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),
    CONSTRAINT power_readings_pkey PRIMARY KEY (ts, device_sn)
);

SELECT create_hypertable(
    'growatt.power_readings',
    by_range('ts', INTERVAL '7 days')
);
```

The `fetched_at` column is added by the fetcher's migration to support "latest wins" audit trail semantics. It is not part of the PK or any conflict target; it is simply overwritten on each upsert.

All columns map directly from `MINHistoryDataPoint` fields in `device.go`:

| Go Field (`MINHistoryDataPoint`) | DB Column | Type |
|---|---|---|
| `Time` | `ts` | `TIMESTAMPTZ` (parsed from `"HH:MM"` string + date + timezone) |
| `Pac` | `pac_w` | `DOUBLE PRECISION` |
| `Ppv` | `ppv_w` | `DOUBLE PRECISION` |
| `Vpv1` | `vpv1_v` | `DOUBLE PRECISION` |
| `Vpv2` | `vpv2_v` | `DOUBLE PRECISION` |
| `Ipv1` | `ipv1_a` | `DOUBLE PRECISION` |
| `Ipv2` | `ipv2_a` | `DOUBLE PRECISION` |
| `Vac1` | `vac1_v` | `DOUBLE PRECISION` |
| `Iac1` | `iac1_a` | `DOUBLE PRECISION` |

### `growatt.collection_runs`

```sql
CREATE TABLE growatt.collection_runs (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_type         TEXT        NOT NULL,   -- 'plant_power', 'device_history', 'energy_daily', 'energy_monthly', 'plant_snapshot', 'device_snapshot'
    source_id           TEXT        NOT NULL,   -- plant_id or device_sn
    query_date_start    DATE,
    query_date_end      DATE,
    points_collected    INT         NOT NULL DEFAULT 0,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    status              TEXT        NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'success', 'error', 'partial')),
    error_message       TEXT
);

CREATE INDEX idx_collection_runs_source
    ON growatt.collection_runs (source_type, source_id, query_date_start DESC);
```

The fetcher writes one row per fetch cycle with `source_type = 'device_history'` and `source_id` set to the device serial number.

### `growatt.collection_gaps`

```sql
CREATE TABLE growatt.collection_gaps (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    device_sn       TEXT        NOT NULL,
    gap_date        DATE        NOT NULL,
    expected_start  TIME,                       -- first expected reading (e.g. sunrise)
    expected_end    TIME,                       -- last expected reading (e.g. sunset)
    reason          TEXT,                       -- 'no_data_returned', 'api_error', 'inverter_offline', 'partial_day'
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ,                -- set when backfill succeeds
    CONSTRAINT collection_gaps_uq UNIQUE (device_sn, gap_date)
);
```

The fetcher inserts rows into `growatt.collection_gaps` when gap detection (section 5) finds intervals > 10 minutes during expected production hours.

---

## 9. Timestamp Parsing Strategy

The Growatt API returns time strings in varying formats (observed in `types.go:normalizeTime`):
- `"HH:MM"` (e.g., `"12:05"`)
- `"YYYY-MM-DD HH:MM"` (e.g., `"2025-02-01 12:05"`)
- `"YYYY-MM-DD HH:MM:SS"` (e.g., `"2025-02-01 12:05:00"`)

The fetcher must construct a full `TIMESTAMPTZ` for PostgreSQL:

```
Procedure parseReadingTime(dataPoint, requestDate, timezoneID):
    timeStr = normalizeTime(dataPoint.Time)          // existing helper -> "HH:MM"
    loc = time.LoadLocation(timezoneID)               // e.g. "US/Central"
    combined = requestDate + " " + timeStr            // "2025-02-01 12:05"
    t = time.ParseInLocation("2006-01-02 15:04", combined, loc)
    return t.UTC()                                    // store as UTC in DB
```

This approach:
- Reuses the existing `normalizeTime()` function from `types.go`.
- Loads the timezone from the configured `timezone_id` (same value passed to the API).
- Converts to UTC before storage so all DB queries work consistently.
- Handles the day-boundary edge case: if fetching yesterday's data, uses yesterday's date for parsing.

---

## 10. Proposed Package Structure

```
cmd/
  gogrowatt-fetcher/
    main.go              -- CLI entry point, signal handling, config loading
pkg/
  growatt/               -- existing API client (unchanged)
    client.go
    device.go
    plant.go
    types.go
    errors.go
internal/
  fetcher/
    fetcher.go           -- Fetch Orchestrator (main loop, backfill logic)
    timer.go             -- Adaptive Timer (next-fetch calculation)
    writer.go            -- Upsert Writer (PostgreSQL batch inserts)
    metadata.go          -- Metadata Tracker (collection_runs writes, gap detection)
    config.go            -- Configuration struct + loader
  db/
    db.go                -- Connection pool setup, migrations runner
    queries.go           -- SQL query constants
```

The fetcher imports `pkg/growatt` as a dependency, calling `NewClientFromEnv()` or `NewClient()` exactly as `cmd/growatt-export/main.go` does today. No modifications to the existing client are required.

---

## 11. Graceful Shutdown

```mermaid
sequenceDiagram
    participant OS as Operating System
    participant Main as main()
    participant Fetcher as Fetch Orchestrator
    participant DB as PostgreSQL Pool

    OS->>Main: SIGINT / SIGTERM
    Main->>Main: cancel(ctx)
    Main->>Fetcher: ctx.Done() propagates
    Fetcher->>Fetcher: Exit polling loop<br/>Finish current upsert if in progress
    Fetcher->>DB: Final COMMIT (if txn open)
    Fetcher-->>Main: Run() returns
    Main->>DB: pool.Close()
    Main->>OS: exit(0)
```

The service:
1. Traps `SIGINT` and `SIGTERM` via `signal.NotifyContext`.
2. Cancels the root context, which propagates through to `GetMINInverterHistory` (it already respects `ctx` via `http.NewRequestWithContext`).
3. Waits for the current fetch/write cycle to complete (no mid-transaction abort).
4. Closes the database connection pool.
5. Exits cleanly.

---

## 12. Summary of Design Decisions

| Decision | Rationale |
|---|---|
| Reuse `pkg/growatt.Client` as-is | The existing client already handles rate limiting, token auth, response parsing, and error classification. No need to duplicate. |
| Fetch full day, filter client-side | The `device/tlx/tlx_data` endpoint has no sub-day range parameter. It returns all points for a calendar date. Filtering to the overlap window is purely for gap-detection logging; all points are upserted. |
| Unconditional upsert (latest wins) | The API may revise historical values. The most recent fetch is authoritative. The `fetched_at` column provides an audit trail. |
| Adaptive timer: `last_ts + 6min` | Aligns fetches to ~1 minute after the next expected 5-minute data point, minimizing both latency and wasted API calls. |
| Backfill at startup, not async | A 7-day backfill is only 7 API calls (~21 seconds). Running it synchronously before entering steady-state keeps the logic simple and guarantees the DB is populated before the first adaptive timer calculation. |
| Store all `MINHistoryDataPoint` fields | Fields like `vpv1_v`/`vpv2_v`, `ipv1_a`/`ipv2_a` are useful for diagnostics (panel-level monitoring, string failure detection) even if primary interest is `pac_w`. Storage cost is negligible. |
| UTC storage in PostgreSQL | Avoids timezone ambiguity in DB queries. The configured `timezone_id` is used only for API requests and timestamp parsing, never stored. |

---

## Testing

### Unit Tests

#### Adaptive Timer Algorithm

Use table-driven tests in `internal/fetcher/timer_test.go` covering edge cases:

| Test Case | Input | Expected `next_fetch` |
|---|---|---|
| No data points | `data_points = []` | `now + 5 min` |
| Normal case | `latest_ts = 12:05, now = 12:06` | `12:11` (12:05 + 5m + 1m) |
| Stale data | `latest_ts = 11:00, now = 12:00` | `now + 1 min` (MIN_WAIT floor) |
| Future data (clock skew) | `latest_ts = 12:30, now = 12:10` | `12:36` (12:30 + 6m), capped if > now + MAX_WAIT |
| Data exactly at MAX_WAIT boundary | `latest_ts = 12:00, now = 12:05` | `12:07` (12:06 is > now + MIN_WAIT, < now + MAX_WAIT) |
| Multiple data points | `latest_ts = max(12:00, 12:05, 12:10)` | Based on `12:10` |

Each test case should assert the exact `time.Duration` returned by the timer function. Use a fixed `now` value (not `time.Now()`) for deterministic tests.

#### Timestamp Parsing

Use table-driven tests in `internal/fetcher/parser_test.go` covering the three Growatt time formats:

| Test Case | Input `time` field | Request Date | Timezone | Expected UTC |
|---|---|---|---|---|
| HH:MM format | `"12:05"` | `2026-02-15` | `US/Central` | `2026-02-15T18:05:00Z` |
| Full datetime | `"2026-02-15 12:05"` | `2026-02-15` | `US/Central` | `2026-02-15T18:05:00Z` |
| With seconds | `"2026-02-15 12:05:00"` | `2026-02-15` | `US/Central` | `2026-02-15T18:05:00Z` |
| Midnight boundary | `"23:55"` | `2026-02-15` | `US/Central` | `2026-02-16T05:55:00Z` |
| DST transition | `"02:30"` | `2026-03-08` | `US/Central` | Verify correct UTC offset during spring-forward |
| Empty string | `""` | `2026-02-15` | `US/Central` | Error returned |
| Invalid format | `"not-a-time"` | `2026-02-15` | `US/Central` | Error returned |

### Integration Tests

#### Test Environment Setup

Use Docker Compose to create a fully isolated test environment:

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

  growatt-simulator:
    build:
      context: .
      dockerfile: cmd/growatt-simulator/Dockerfile
    environment:
      SIMULATOR_PORT: "8080"
      SIMULATOR_DEVICE_SN: "TEST123"
      SIMULATOR_TIMEZONE: "US/Central"
    ports:
      - "8081:8080"
```

The `growatt-simulator` is the test harness defined in doc 07. It provides a fake Growatt OpenAPI endpoint that returns realistic `MINHistoryDataPoint` data, supports configurable error injection (rate limiting, timeouts), and allows control over the simulated clock.

Run the schema migration against the test database before each test suite:

```bash
psql -h localhost -p 5433 -U growatt -d growatt_test -f migrations/001_schema.sql
```

#### Backfill Logic

1. Start with an empty `growatt.power_readings` table.
2. Configure the fetcher with `BACKFILL_DAYS=7` and point it at the simulator.
3. The simulator should return data for each of the past 7 days.
4. Start the fetcher and wait for it to enter steady-state.
5. Verify: `SELECT COUNT(DISTINCT ts::date) FROM growatt.power_readings WHERE device_sn = 'TEST123'` returns 7 (or 8 including today).
6. Verify: `SELECT MIN(ts) FROM growatt.power_readings WHERE device_sn = 'TEST123'` is within the 7-day backfill window.
7. Verify: a `growatt.collection_runs` row exists for each backfill day with `status = 'success'`.

#### Upsert Logic (Latest Wins)

1. Use the simulator to serve data for a single day with known values (e.g., `pac_w = 1000.0` at `12:00`).
2. Run the fetcher to ingest this data.
3. Verify: `SELECT pac_w FROM growatt.power_readings WHERE device_sn = 'TEST123' AND ts = '2026-02-15 18:00:00Z'` returns `1000.0`.
4. Reconfigure the simulator to return modified values for the same timestamp (e.g., `pac_w = 1500.0` at `12:00`).
5. Run the fetcher again.
6. Verify: the same row now has `pac_w = 1500.0` (latest wins).
7. Verify: `fetched_at` has been updated to the second fetch time.

#### Error Recovery (Rate Limiting)

1. Configure the simulator to return error code `10012` ("frequently") on the first 2 requests, then succeed on the 3rd.
2. Start the fetcher.
3. Verify: the fetcher retries after ~30 seconds per rate-limit error.
4. Verify: data is eventually ingested successfully.
5. Verify: the `growatt.collection_runs` row has `status = 'success'` (not `'error'`).
6. Verify: logs contain rate-limit warning messages with exponential backoff timestamps.

#### Graceful Shutdown

1. Start the fetcher in a subprocess pointing at the simulator.
2. Wait for at least one successful fetch cycle (poll `growatt.collection_runs` for a `status = 'success'` row).
3. Send `SIGTERM` to the fetcher process during a fetch cycle (coordinate timing with the simulator by adding a deliberate delay to the API response).
4. Verify: the process exits with code 0.
5. Verify: no partial writes -- either the full batch was committed or the transaction was rolled back. Check `SELECT COUNT(*) FROM growatt.power_readings` is consistent (not a partial batch of, say, 50 out of 144 expected points for a day).
6. Verify: the DB connection pool is cleanly closed (no `pg_stat_activity` entries for the test user remain).

#### Gap Detection

1. Configure the simulator to return data for a full day but with a deliberate 30-minute gap (e.g., no data points between 13:00 and 13:30).
2. Run the fetcher.
3. Verify: a row is inserted into `growatt.collection_gaps` with `gap_date` matching the test day, `expected_start` at approximately `13:00`, and `expected_end` at approximately `13:30`.
4. Verify: `reason` is set appropriately (e.g., `'partial_day'`).
5. Reconfigure the simulator to fill in the missing gap data.
6. Run the fetcher again.
7. Verify: `growatt.collection_gaps.resolved_at` is updated (no longer NULL).

#### Midnight Boundary

1. Set the simulator's clock to 23:50 local time (US/Central).
2. Start the fetcher. Because the current time is within 65 minutes of midnight, it should fetch both today and yesterday.
3. Verify: `growatt.power_readings` contains rows for both calendar days.
4. Advance the simulator's clock past midnight (to 00:05 the next day).
5. Trigger another fetch cycle.
6. Verify: the fetcher now fetches the new "today" and also the previous day (which was "today" before midnight).
7. Verify: no duplicate rows exist -- the upsert handles the overlap cleanly. Check `SELECT ts, COUNT(*) FROM growatt.power_readings WHERE device_sn = 'TEST123' GROUP BY ts HAVING COUNT(*) > 1` returns zero rows.
