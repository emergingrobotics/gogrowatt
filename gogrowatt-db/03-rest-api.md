# gogrowatt REST API Design

Read-only REST API for serving solar production data from PostgreSQL.

## Source Data Model Reference

The API exposes data originally collected from the Growatt OpenAPI and stored in PostgreSQL. The key domain types (from `pkg/growatt/types.go`) are:

- **Plant** -- a solar installation site with location, peak capacity, and cumulative energy totals
- **Device** -- an inverter (MIN/TLX series) identified by serial number, belonging to a plant
- **MINHistoryDataPoint** -- a 5-minute granularity reading with fields: `pac_w` (AC power W), `ppv_w` (PV power W), `vpv1_v`/`vpv2_v` (PV string voltages), `ipv1_a`/`ipv2_a` (PV string currents), `vac1_v` (AC voltage), `iac1_a` (AC current)
- **EnergyDataPoint** -- daily or monthly energy totals (kWh)
- **HourlyStats / AggregatedHourStats / MultiDayStats** (from `internal/stats/stats.go`) -- statistical aggregations: min, max, mean, median, stddev computed per hour across date ranges

---

## 1. Architecture

```mermaid
graph TB
    subgraph Clients
        WEB[Web Frontend]
        MOB[Mobile App]
        CLI[CLI / Scripts]
    end

    subgraph "gogrowatt-api (read-only)"
        RTR[chi Router + Middleware]
        RTR --> CORS[CORS Middleware]
        RTR --> LOG[Request Logger]
        RTR --> CACHE[Cache-Control Headers]
        RTR --> VAL[Query Param Validator]

        H_PLANT[Plant Handlers]
        H_DEVICE[Device Handlers]
        H_POWER[Power Handlers]
        H_ENERGY[Energy Handlers]
        H_STATS[Stats Handlers]
        H_HEALTH[Health Handler]

        RTR --> H_PLANT
        RTR --> H_DEVICE
        RTR --> H_POWER
        RTR --> H_ENERGY
        RTR --> H_STATS
        RTR --> H_HEALTH

        REPO[Repository Layer<br/>sqlx / pgx]
        H_PLANT --> REPO
        H_DEVICE --> REPO
        H_POWER --> REPO
        H_ENERGY --> REPO
        H_STATS --> REPO
        H_HEALTH --> REPO
    end

    subgraph PostgreSQL
        T_PLANT[(growatt.plants)]
        T_DEVICE[(growatt.devices)]
        T_POWER[(growatt.power_readings)]
        T_ENERGY[(growatt.energy_summaries)]
        T_MVDAILY[(growatt.mv_daily_production)]
    end

    WEB --> RTR
    MOB --> RTR
    CLI --> RTR
    REPO --> T_PLANT
    REPO --> T_DEVICE
    REPO --> T_POWER
    REPO --> T_ENERGY
    REPO --> T_MVDAILY

    subgraph "Data Fetcher (separate service)"
        FETCH[growatt-fetcher]
    end
    FETCH -->|INSERT/UPSERT| T_PLANT
    FETCH -->|INSERT/UPSERT| T_DEVICE
    FETCH -->|INSERT| T_POWER
    FETCH -->|UPSERT| T_ENERGY
```

---

## 2. Request / Response Flow

```mermaid
sequenceDiagram
    participant C as Client
    participant M as Middleware Stack
    participant H as Handler
    participant R as Repository
    participant DB as PostgreSQL

    C->>M: GET /api/v1/devices/{sn}/power?from=...&to=...&interval=15min
    M->>M: CORS check
    M->>M: Request logging
    M->>M: Validate query params
    M->>H: Parsed request context
    H->>R: GetPowerReadings(sn, from, to, interval)
    R->>DB: SELECT with time_bucket / date_trunc
    DB-->>R: Rows
    R-->>H: []PowerPoint
    H->>H: Build envelope, set Cache-Control
    H-->>M: JSON response
    M-->>C: 200 OK + JSON body

    Note over M,H: On validation failure
    M-->>C: 400 Bad Request + error envelope

    Note over R,DB: On DB timeout
    R-->>H: context.DeadlineExceeded
    H-->>M: 504 Gateway Timeout
    M-->>C: 504 + error envelope
```

---

## 3. Response Envelope

Every response uses a consistent JSON envelope:

```json
{
  "data": { ... },
  "meta": {
    "timestamp": "2026-02-15T12:00:00Z",
    "request_id": "req_abc123"
  }
}
```

Error responses:

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "Parameter 'from' is required",
    "details": {
      "parameter": "from",
      "constraint": "required"
    }
  },
  "meta": {
    "timestamp": "2026-02-15T12:00:00Z",
    "request_id": "req_abc123"
  }
}
```

Paginated responses add a `pagination` field:

```json
{
  "data": [ ... ],
  "pagination": {
    "page": 1,
    "per_page": 100,
    "total": 342,
    "total_pages": 4
  },
  "meta": { ... }
}
```

Go types:

```go
type Envelope[T any] struct {
    Data       T           `json:"data"`
    Pagination *Pagination `json:"pagination,omitempty"`
    Meta       Meta        `json:"meta"`
}

type ErrorEnvelope struct {
    Error APIError `json:"error"`
    Meta  Meta     `json:"meta"`
}

type APIError struct {
    Code    string      `json:"code"`
    Message string      `json:"message"`
    Details interface{} `json:"details,omitempty"`
}

type Pagination struct {
    Page       int `json:"page"`
    PerPage    int `json:"per_page"`
    Total      int `json:"total"`
    TotalPages int `json:"total_pages"`
}

type Meta struct {
    Timestamp string `json:"timestamp"`
    RequestID string `json:"request_id"`
}
```

---

## 4. Endpoint Specification

### 4.1 GET /api/v1/health

Service health and data freshness check.

| Aspect | Detail |
|--------|--------|
| Auth | None |
| Cache | `Cache-Control: no-cache` |

**Response 200:**

```json
{
  "data": {
    "status": "healthy",
    "version": "0.1.0",
    "uptime_seconds": 86412,
    "database": {
      "connected": true,
      "latency_ms": 2
    },
    "data_freshness": {
      "latest_reading": "2026-02-15T12:55:00-06:00",
      "age_seconds": 312,
      "is_stale": false,
      "stale_threshold_seconds": 900
    }
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_h1" }
}
```

**Response 503 (degraded):**

```json
{
  "data": {
    "status": "degraded",
    "version": "0.1.0",
    "uptime_seconds": 86412,
    "database": {
      "connected": false,
      "latency_ms": 0
    },
    "data_freshness": null
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_h2" }
}
```

---

### 4.2 GET /api/v1/plants

List all plants.

| Aspect | Detail |
|--------|--------|
| Query params | `page` (int, default 1), `per_page` (int, default 50, max 100) |
| Cache | `Cache-Control: public, max-age=300` |

**Response 200:**

```json
{
  "data": [
    {
      "id": "12345",
      "name": "Home Solar",
      "country": "US",
      "city": "Austin",
      "latitude": 30.2672,
      "longitude": -97.7431,
      "peak_power_kw": 8.4,
      "status": "online",
      "current_power_w": 5120.5,
      "today_energy_kwh": 18.7,
      "total_energy_kwh": 12450.3,
      "created_at": "2024-06-15T00:00:00Z",
      "device_count": 1
    }
  ],
  "pagination": {
    "page": 1,
    "per_page": 50,
    "total": 1,
    "total_pages": 1
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_p1" }
}
```

---

### 4.3 GET /api/v1/plants/{id}

Plant details with live summary.

| Aspect | Detail |
|--------|--------|
| Path params | `id` -- plant ID (string) |
| Cache | `Cache-Control: public, max-age=60` |

**Response 200:**

```json
{
  "data": {
    "id": "12345",
    "name": "Home Solar",
    "country": "US",
    "city": "Austin",
    "latitude": 30.2672,
    "longitude": -97.7431,
    "peak_power_kw": 8.4,
    "status": "online",
    "current_power_w": 5120.5,
    "today_energy_kwh": 18.7,
    "month_energy_kwh": 385.2,
    "year_energy_kwh": 1050.8,
    "total_energy_kwh": 12450.3,
    "peak_power_today_w": 5389.9,
    "formula_coal_kg": 4945.2,
    "formula_co2_kg": 12388.5,
    "formula_trees": 610,
    "money_saved": 1867.55,
    "money_unit": "USD",
    "created_at": "2024-06-15T00:00:00Z",
    "devices": [
      {
        "serial_number": "TLXABC12345",
        "name": "MIN 6000TL-XH",
        "type": "inverter",
        "model": "MIN 6000TL-XH",
        "status": "online",
        "last_update": "2026-02-15T12:55:00-06:00"
      }
    ]
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_p2" }
}
```

**Response 404:**

```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "Plant '99999' not found"
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_p3" }
}
```

---

### 4.4 GET /api/v1/plants/{id}/devices

List devices belonging to a plant.

| Aspect | Detail |
|--------|--------|
| Path params | `id` -- plant ID |
| Query params | `page`, `per_page` |
| Cache | `Cache-Control: public, max-age=300` |

**Response 200:**

```json
{
  "data": [
    {
      "serial_number": "TLXABC12345",
      "plant_id": "12345",
      "name": "MIN 6000TL-XH",
      "type": "inverter",
      "model": "MIN 6000TL-XH",
      "status": "online",
      "last_update": "2026-02-15T12:55:00-06:00"
    }
  ],
  "pagination": {
    "page": 1,
    "per_page": 50,
    "total": 1,
    "total_pages": 1
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_d1" }
}
```

---

### 4.5 GET /api/v1/devices/{sn}

Device details including current live status.

| Aspect | Detail |
|--------|--------|
| Path params | `sn` -- device serial number |
| Cache | `Cache-Control: public, max-age=60` |

**Response 200:**

```json
{
  "data": {
    "serial_number": "TLXABC12345",
    "plant_id": "12345",
    "name": "MIN 6000TL-XH",
    "type": "inverter",
    "model": "MIN 6000TL-XH",
    "status": "online",
    "last_update": "2026-02-15T12:55:00-06:00",
    "current": {
      "pac_w": 5120.5,
      "ppv_w": 5250.0,
      "vpv1_v": 324.5,
      "vpv2_v": 318.2,
      "ipv1_a": 8.12,
      "ipv2_a": 8.35,
      "vac1_v": 243.1,
      "iac1_a": 21.05,
      "frequency_hz": 60.01,
      "temperature_c": 42.3,
      "today_energy_kwh": 18.7,
      "total_energy_kwh": 12450.3
    }
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_d2" }
}
```

---

### 4.6 GET /api/v1/devices/{sn}/power

Time-series power data with flexible aggregation. This is the primary query endpoint, corresponding to the 5-minute `growatt.power_readings` table rows.

| Aspect | Detail |
|--------|--------|
| Path params | `sn` -- device serial number |
| Query params | See table below |
| Cache | `Cache-Control: public, max-age=300` (past data); `max-age=60` (includes today) |

**Query Parameters:**

| Param | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `from` | RFC 3339 or `YYYY-MM-DD` | yes | -- | Start of time range (inclusive) |
| `to` | RFC 3339 or `YYYY-MM-DD` | yes | -- | End of time range (inclusive) |
| `interval` | enum | no | `5min` | Aggregation bucket: `5min`, `15min`, `1h`, `1d` |
| `fields` | comma-separated | no | `pac_w` | Fields to include: `pac_w`, `ppv_w`, `vpv1_v`, `vpv2_v`, `ipv1_a`, `ipv2_a`, `vac1_v`, `iac1_a` |
| `tz` | IANA timezone | no | `UTC` | Timezone for bucket alignment and date interpretation |
| `page` | int | no | 1 | Page number |
| `per_page` | int | no | 500 | Results per page (max 2000) |

**Aggregation behavior:**

- `5min` -- raw readings, no aggregation
- `15min` -- average of three 5-minute readings per bucket
- `1h` -- average, min, max per hour (matches `internal/stats` HourlyStats pattern)
- `1d` -- average, min, max, peak per day

**Response 200 (interval=5min, fields=pac_w):**

```json
{
  "data": {
    "serial_number": "TLXABC12345",
    "from": "2026-02-15T00:00:00-06:00",
    "to": "2026-02-15T23:59:59-06:00",
    "interval": "5min",
    "timezone": "US/Central",
    "fields": ["pac_w"],
    "readings": [
      { "time": "2026-02-15T07:28:00-06:00", "pac_w": 0.0 },
      { "time": "2026-02-15T07:33:00-06:00", "pac_w": 5.0 },
      { "time": "2026-02-15T07:38:00-06:00", "pac_w": 29.9 },
      { "time": "2026-02-15T07:43:00-06:00", "pac_w": 57.2 },
      { "time": "2026-02-15T07:48:00-06:00", "pac_w": 78.6 },
      { "time": "2026-02-15T07:53:00-06:00", "pac_w": 101.6 },
      { "time": "2026-02-15T07:58:00-06:00", "pac_w": 120.9 },
      { "time": "2026-02-15T08:03:00-06:00", "pac_w": 136.6 },
      { "time": "2026-02-15T08:08:00-06:00", "pac_w": 155.7 }
    ]
  },
  "pagination": {
    "page": 1,
    "per_page": 500,
    "total": 66,
    "total_pages": 1
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_pw1" }
}
```

**Response 200 (interval=1h, fields=pac_w,vpv1_v,vpv2_v):**

```json
{
  "data": {
    "serial_number": "TLXABC12345",
    "from": "2026-02-15T00:00:00-06:00",
    "to": "2026-02-15T23:59:59-06:00",
    "interval": "1h",
    "timezone": "US/Central",
    "fields": ["pac_w", "vpv1_v", "vpv2_v"],
    "readings": [
      {
        "time": "2026-02-15T07:00:00-06:00",
        "pac_w": { "avg": 56.17, "min": 0.0, "max": 120.9, "samples": 7 },
        "vpv1_v": { "avg": 82.3, "min": 0.0, "max": 145.2, "samples": 7 },
        "vpv2_v": { "avg": 78.1, "min": 0.0, "max": 138.6, "samples": 7 }
      },
      {
        "time": "2026-02-15T08:00:00-06:00",
        "pac_w": { "avg": 253.34, "min": 136.6, "max": 383.1, "samples": 12 },
        "vpv1_v": { "avg": 198.5, "min": 155.2, "max": 242.1, "samples": 12 },
        "vpv2_v": { "avg": 192.3, "min": 148.7, "max": 235.8, "samples": 12 }
      },
      {
        "time": "2026-02-15T09:00:00-06:00",
        "pac_w": { "avg": 539.12, "min": 338.8, "max": 1023.2, "samples": 12 },
        "vpv1_v": { "avg": 265.1, "min": 220.4, "max": 325.8, "samples": 12 },
        "vpv2_v": { "avg": 258.7, "min": 215.0, "max": 318.2, "samples": 12 }
      }
    ]
  },
  "pagination": {
    "page": 1,
    "per_page": 500,
    "total": 6,
    "total_pages": 1
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_pw2" }
}
```

**SQL pattern for aggregation (interval=1h example):**

```sql
SELECT
    date_trunc('hour', ts AT TIME ZONE $tz) AS bucket,
    avg(pac_w)  AS pac_w_avg,
    min(pac_w)  AS pac_w_min,
    max(pac_w)  AS pac_w_max,
    count(*)    AS samples
FROM growatt.power_readings
WHERE device_sn = $1
  AND ts >= $2
  AND ts < $3
GROUP BY bucket
ORDER BY bucket;
```

---

### 4.7 GET /api/v1/devices/{sn}/power/latest

Most recent reading for a device.

| Aspect | Detail |
|--------|--------|
| Path params | `sn` -- device serial number |
| Cache | `Cache-Control: public, max-age=30` |

**Response 200:**

```json
{
  "data": {
    "serial_number": "TLXABC12345",
    "time": "2026-02-15T12:55:00-06:00",
    "pac_w": 5389.9,
    "ppv_w": 5450.2,
    "vpv1_v": 335.2,
    "vpv2_v": 330.1,
    "ipv1_a": 8.25,
    "ipv2_a": 8.40,
    "vac1_v": 243.5,
    "iac1_a": 22.15
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_lt1" }
}
```

---

### 4.8 GET /api/v1/devices/{sn}/energy

Device-level energy totals (daily or monthly). This endpoint queries `growatt.mv_daily_production`, which is a materialized view computed from `growatt.power_readings` and keyed by `(production_date, device_sn)`. This is distinct from the plant-level energy endpoint (section 4.10), which queries `growatt.energy_summaries`.

> **Why two different tables?** The schema's `growatt.energy_summaries` table is keyed by `(period_date, plant_id, time_unit)` -- it stores plant-level totals reported by the Growatt API. For per-device energy, the API uses `growatt.mv_daily_production`, which derives daily energy estimates from the 5-minute `pac_w` readings in `growatt.power_readings`.

| Aspect | Detail |
|--------|--------|
| Path params | `sn` -- device serial number |
| Query params | See table below |
| Data source | `growatt.mv_daily_production` (materialized view, per-device) |
| Cache | `Cache-Control: public, max-age=3600` (historical); `max-age=300` (includes today) |

**Query Parameters:**

| Param | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `from` | `YYYY-MM-DD` | yes | -- | Start date |
| `to` | `YYYY-MM-DD` | yes | -- | End date |
| `unit` | enum | no | `day` | Grouping: `day` or `month` |
| `tz` | IANA timezone | no | `UTC` | Timezone for date boundaries |

**SQL pattern (unit=day):**

```sql
SELECT
    production_date     AS date,
    estimated_energy_kwh AS energy_kwh
FROM growatt.mv_daily_production
WHERE device_sn = $1
  AND production_date >= $2
  AND production_date <= $3
ORDER BY production_date;
```

**Response 200 (unit=day):**

```json
{
  "data": {
    "serial_number": "TLXABC12345",
    "from": "2026-02-01",
    "to": "2026-02-15",
    "unit": "day",
    "timezone": "US/Central",
    "totals": [
      { "date": "2026-02-01", "energy_kwh": 22.4 },
      { "date": "2026-02-02", "energy_kwh": 18.1 },
      { "date": "2026-02-03", "energy_kwh": 25.7 },
      { "date": "2026-02-14", "energy_kwh": 20.4 },
      { "date": "2026-02-15", "energy_kwh": 18.7 }
    ],
    "summary": {
      "total_kwh": 298.5,
      "average_kwh": 19.9,
      "max_kwh": 25.7,
      "max_date": "2026-02-03",
      "min_kwh": 12.3,
      "min_date": "2026-02-10",
      "days_with_data": 15
    }
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_e1" }
}
```

**Response 200 (unit=month):**

```json
{
  "data": {
    "serial_number": "TLXABC12345",
    "from": "2025-01-01",
    "to": "2026-02-15",
    "unit": "month",
    "timezone": "US/Central",
    "totals": [
      { "date": "2025-01", "energy_kwh": 450.2 },
      { "date": "2025-02", "energy_kwh": 512.8 },
      { "date": "2025-12", "energy_kwh": 320.1 },
      { "date": "2026-01", "energy_kwh": 380.5 },
      { "date": "2026-02", "energy_kwh": 298.5 }
    ],
    "summary": {
      "total_kwh": 6234.8,
      "average_kwh": 445.3,
      "months_with_data": 14
    }
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_e2" }
}
```

---

### 4.9 GET /api/v1/devices/{sn}/stats

Statistical summaries by hour across a date range. This mirrors the `AggregatedHourStats` and `MultiDayStats` types from `internal/stats/stats.go`.

| Aspect | Detail |
|--------|--------|
| Path params | `sn` -- device serial number |
| Query params | See table below |
| Cache | `Cache-Control: public, max-age=3600` |

**Query Parameters:**

| Param | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `from` | `YYYY-MM-DD` | yes | -- | Start date |
| `to` | `YYYY-MM-DD` | yes | -- | End date |
| `field` | enum | no | `pac_w` | Which measurement field to compute stats on: `pac_w`, `ppv_w`, `vpv1_v`, `vpv2_v`, `ipv1_a`, `ipv2_a`, `vac1_v`, `iac1_a` |
| `tz` | IANA timezone | no | `UTC` | Timezone for hour alignment |

**Response 200:**

```json
{
  "data": {
    "serial_number": "TLXABC12345",
    "from": "2026-02-12",
    "to": "2026-02-15",
    "field": "pac_w",
    "timezone": "US/Central",
    "days_analyzed": 4,
    "total_production_kwh": 78.5,
    "daily_average_kwh": 19.6,
    "peak_hour": 13,
    "peak_power_avg_w": 5067.06,
    "by_hour": [
      {
        "hour": 7,
        "min_w": 0.0,
        "max_w": 120.9,
        "avg_w": 56.17,
        "median_w": 52.3,
        "stddev_w": 42.8,
        "sample_days": 2
      },
      {
        "hour": 8,
        "min_w": 136.6,
        "max_w": 383.1,
        "avg_w": 253.34,
        "median_w": 248.5,
        "stddev_w": 65.2,
        "sample_days": 2
      },
      {
        "hour": 11,
        "min_w": 1629.5,
        "max_w": 2614.9,
        "avg_w": 2043.6,
        "median_w": 2007.3,
        "stddev_w": 210.5,
        "sample_days": 2
      },
      {
        "hour": 12,
        "min_w": 2339.4,
        "max_w": 5223.3,
        "avg_w": 2812.96,
        "median_w": 2721.9,
        "stddev_w": 620.3,
        "sample_days": 2
      },
      {
        "hour": 13,
        "min_w": 4837.8,
        "max_w": 5195.3,
        "avg_w": 5067.06,
        "median_w": 5067.1,
        "stddev_w": 98.4,
        "sample_days": 1
      },
      {
        "hour": 17,
        "min_w": 339.1,
        "max_w": 1151.2,
        "avg_w": 610.98,
        "median_w": 605.2,
        "stddev_w": 185.3,
        "sample_days": 1
      }
    ]
  },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_s1" }
}
```

**SQL pattern (executed in PostgreSQL):**

```sql
-- Step 1: hourly averages per day
WITH hourly AS (
    SELECT
        (ts AT TIME ZONE $tz)::date AS day,
        EXTRACT(HOUR FROM ts AT TIME ZONE $tz)::int AS hour,
        avg(pac_w) AS avg_pac_w,
        min(pac_w) AS min_pac_w,
        max(pac_w) AS max_pac_w,
        count(*) AS samples
    FROM growatt.power_readings
    WHERE device_sn = $1
      AND ts >= $2
      AND ts < $3
    GROUP BY day, hour
)
-- Step 2: aggregate across days
SELECT
    hour,
    min(min_pac_w) AS min_w,
    max(max_pac_w) AS max_w,
    avg(avg_pac_w) AS avg_w,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY avg_pac_w) AS median_w,
    stddev_samp(avg_pac_w) AS stddev_w,
    count(DISTINCT day) AS sample_days
FROM hourly
WHERE samples > 0
GROUP BY hour
ORDER BY hour;
```

---

### 4.10 Plant-Level Aggregated Queries

Plant-level endpoints aggregate across all devices in a plant.

#### GET /api/v1/plants/{id}/power

Same parameters and response shape as the device power endpoint (section 4.6), but `pac_w` values are summed across all devices in the plant.

| Aspect | Detail |
|--------|--------|
| Path params | `id` -- plant ID |
| Query params | Same as `/devices/{sn}/power` |
| Data source | `growatt.power_readings` (aggregated across all devices belonging to the plant) |
| Behavior | SUM of `pac_w`/`ppv_w` across devices; voltages/currents excluded (not summable) |

**Response 200 (interval=1h):**

```json
{
  "data": {
    "plant_id": "12345",
    "from": "2026-02-15T00:00:00-06:00",
    "to": "2026-02-15T23:59:59-06:00",
    "interval": "1h",
    "timezone": "US/Central",
    "fields": ["pac_w"],
    "readings": [
      {
        "time": "2026-02-15T07:00:00-06:00",
        "pac_w": { "avg": 56.17, "min": 0.0, "max": 120.9, "samples": 7 }
      },
      {
        "time": "2026-02-15T12:00:00-06:00",
        "pac_w": { "avg": 2960.05, "min": 2339.4, "max": 5389.9, "samples": 11 }
      }
    ]
  },
  "pagination": { "page": 1, "per_page": 500, "total": 6, "total_pages": 1 },
  "meta": { "timestamp": "2026-02-15T19:00:12Z", "request_id": "req_pp1" }
}
```

#### GET /api/v1/plants/{id}/energy

Plant-level energy totals. Unlike the device-level energy endpoint (section 4.8) which queries `growatt.mv_daily_production`, this endpoint queries `growatt.energy_summaries`, which stores official plant-level totals reported by the Growatt API. The table is keyed by `(period_date, plant_id, time_unit)`.

| Aspect | Detail |
|--------|--------|
| Path params | `id` -- plant ID |
| Query params | Same as `/devices/{sn}/energy` |
| Data source | `growatt.energy_summaries` (plant-level, from Growatt API) |

**SQL pattern (unit=day):**

```sql
SELECT
    period_date  AS date,
    energy_kwh
FROM growatt.energy_summaries
WHERE plant_id = $1
  AND time_unit = 'day'
  AND period_date >= $2
  AND period_date <= $3
ORDER BY period_date;
```

#### GET /api/v1/plants/{id}/stats

Same as device stats endpoint, aggregated across all devices in the plant.

| Aspect | Detail |
|--------|--------|
| Path params | `id` -- plant ID |
| Query params | Same as `/devices/{sn}/stats` |
| Data source | `growatt.power_readings` (aggregated across all devices belonging to the plant) |

---

## 5. Error Handling

### Error Codes

| HTTP Status | Error Code | When |
|-------------|-----------|------|
| 400 | `VALIDATION_ERROR` | Missing/invalid query parameters |
| 400 | `INVALID_DATE_RANGE` | `to` before `from`, range > 366 days |
| 400 | `INVALID_INTERVAL` | Unknown interval value |
| 400 | `INVALID_TIMEZONE` | Unrecognized IANA timezone |
| 404 | `NOT_FOUND` | Plant or device does not exist |
| 404 | `NO_DATA` | Valid entity but no readings in range |
| 429 | `RATE_LIMITED` | Too many requests |
| 500 | `INTERNAL_ERROR` | Unexpected server error |
| 502 | `DATABASE_ERROR` | Database connection/query failure |
| 504 | `TIMEOUT` | Query exceeded deadline |

### Error Flow Diagram

```mermaid
flowchart TD
    REQ[Incoming Request] --> PARSE{Parse path + query}
    PARSE -->|Invalid params| E400[400 VALIDATION_ERROR]
    PARSE -->|OK| LOOKUP{Lookup entity}
    LOOKUP -->|Not found| E404[404 NOT_FOUND]
    LOOKUP -->|Found| QUERY{Execute DB query}
    QUERY -->|Timeout| E504[504 TIMEOUT]
    QUERY -->|DB error| E502[502 DATABASE_ERROR]
    QUERY -->|Empty result| EMPTY{Has data?}
    EMPTY -->|No rows| E404B[404 NO_DATA]
    EMPTY -->|Has rows| OK[200 + JSON envelope]
```

### Validation Rules

| Parameter | Rules |
|-----------|-------|
| `from` | Required for time-series endpoints. Must be valid RFC 3339 or `YYYY-MM-DD`. |
| `to` | Required for time-series endpoints. Must be >= `from`. Range `to - from` <= 366 days. |
| `interval` | Must be one of: `5min`, `15min`, `1h`, `1d`. |
| `unit` | Must be one of: `day`, `month`. |
| `fields` | Comma-separated subset of: `pac_w`, `ppv_w`, `vpv1_v`, `vpv2_v`, `ipv1_a`, `ipv2_a`, `vac1_v`, `iac1_a`. |
| `tz` | Must be valid IANA timezone (loaded via `time.LoadLocation`). |
| `page` | Integer >= 1. |
| `per_page` | Integer 1..2000 (power), 1..100 (plants/devices). |
| `sn` (path) | Alphanumeric, 5-30 characters. |
| `id` (path) | Numeric string. |

---

## 6. Go Implementation Patterns

### Router Setup (chi)

```go
package main

import (
    "net/http"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"
    "github.com/go-chi/cors"
)

func newRouter(h *Handlers) http.Handler {
    r := chi.NewRouter()

    // Middleware stack
    r.Use(middleware.RequestID)
    r.Use(middleware.RealIP)
    r.Use(middleware.Logger)
    r.Use(middleware.Recoverer)
    r.Use(middleware.Timeout(30 * time.Second))
    r.Use(cors.Handler(cors.Options{
        AllowedOrigins:   []string{"https://solar.example.com", "http://localhost:*"},
        AllowedMethods:   []string{"GET", "OPTIONS"},
        AllowedHeaders:   []string{"Accept", "Content-Type"},
        ExposedHeaders:   []string{"X-Request-ID"},
        MaxAge:           86400,
    }))

    r.Route("/api/v1", func(r chi.Router) {
        r.Get("/health", h.Health)

        r.Route("/plants", func(r chi.Router) {
            r.Get("/", h.ListPlants)
            r.Route("/{plantID}", func(r chi.Router) {
                r.Get("/", h.GetPlant)
                r.Get("/devices", h.ListPlantDevices)
                r.Get("/power", h.GetPlantPower)
                r.Get("/energy", h.GetPlantEnergy)
                r.Get("/stats", h.GetPlantStats)
            })
        })

        r.Route("/devices/{deviceSN}", func(r chi.Router) {
            r.Get("/", h.GetDevice)
            r.Get("/power", h.GetDevicePower)
            r.Get("/power/latest", h.GetDevicePowerLatest)
            r.Get("/energy", h.GetDeviceEnergy)
            r.Get("/stats", h.GetDeviceStats)
        })
    })

    return r
}
```

### Handler Pattern

```go
func (h *Handlers) GetDevicePower(w http.ResponseWriter, r *http.Request) {
    sn := chi.URLParam(r, "deviceSN")

    params, err := parsePowerParams(r)
    if err != nil {
        writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
        return
    }

    readings, total, err := h.repo.GetPowerReadings(r.Context(), sn, params)
    if err != nil {
        handleDBError(w, r, err)
        return
    }

    setCacheControl(w, params.To, "public, max-age=300", "public, max-age=60")
    writeJSON(w, r, http.StatusOK, Envelope[PowerResponse]{
        Data: PowerResponse{
            SerialNumber: sn,
            From:         params.From,
            To:           params.To,
            Interval:     params.Interval,
            Timezone:     params.Timezone,
            Fields:       params.Fields,
            Readings:     readings,
        },
        Pagination: newPagination(params.Page, params.PerPage, total),
    })
}
```

### Cache-Control Strategy

```go
// setCacheControl sets Cache-Control based on whether the query includes today.
// Historical data is cached longer; data including today is cached briefly.
func setCacheControl(w http.ResponseWriter, queryEnd time.Time, historical, live string) {
    now := time.Now()
    today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

    if queryEnd.Before(today) {
        w.Header().Set("Cache-Control", historical)
    } else {
        w.Header().Set("Cache-Control", live)
    }
}
```

| Endpoint | Historical (before today) | Live (includes today) |
|----------|--------------------------|----------------------|
| `/health` | -- | `no-cache` |
| `/plants` | `public, max-age=300` | `public, max-age=300` |
| `/plants/{id}` | `public, max-age=60` | `public, max-age=60` |
| `/devices/{sn}` | `public, max-age=60` | `public, max-age=60` |
| `/devices/{sn}/power` | `public, max-age=300` | `public, max-age=60` |
| `/devices/{sn}/power/latest` | -- | `public, max-age=30` |
| `/devices/{sn}/energy` | `public, max-age=3600` | `public, max-age=300` |
| `/devices/{sn}/stats` | `public, max-age=3600` | `public, max-age=300` |

---

## 7. Timezone Handling

All timestamps stored in PostgreSQL use `TIMESTAMPTZ` (UTC internally). The API converts to the requested timezone for display and aggregation bucketing.

**Rules:**

1. The `tz` query parameter accepts IANA timezone names (e.g., `US/Central`, `America/Chicago`, `UTC`).
2. When `from`/`to` are given as bare dates (`YYYY-MM-DD`), they are interpreted as the start/end of that day in the specified timezone.
   - `from=2026-02-15` with `tz=US/Central` becomes `2026-02-15T00:00:00-06:00`
   - `to=2026-02-15` with `tz=US/Central` becomes `2026-02-15T23:59:59-06:00`
3. When `from`/`to` are RFC 3339 with offset, the `tz` parameter is used only for aggregation bucket alignment, not for reinterpreting the input times.
4. All timestamps in responses include the timezone offset.
5. Aggregation buckets (`date_trunc`, hourly grouping) are aligned to the requested timezone.

```go
func parseTimeParam(value, tz string) (time.Time, error) {
    loc, err := time.LoadLocation(tz)
    if err != nil {
        return time.Time{}, fmt.Errorf("invalid timezone: %s", tz)
    }

    // Try RFC 3339 first
    if t, err := time.Parse(time.RFC3339, value); err == nil {
        return t, nil
    }

    // Try bare date
    if t, err := time.ParseInLocation("2006-01-02", value, loc); err == nil {
        return t, nil
    }

    return time.Time{}, fmt.Errorf("invalid time format: %s (expected RFC3339 or YYYY-MM-DD)", value)
}
```

---

## 8. Pagination

Offset-based pagination for simplicity (the datasets are time-bounded and rarely exceed thousands of rows per query).

**Defaults and limits:**

| Endpoint type | Default `per_page` | Max `per_page` |
|---------------|-------------------|----------------|
| Plants / Devices | 50 | 100 |
| Power readings | 500 | 2000 |
| Energy totals | 100 | 366 |
| Stats (by_hour) | Not paginated (max 24 rows) | -- |

**Headers included:**

```
Link: <https://api.example.com/api/v1/devices/TLXABC12345/power?from=2026-02-01&to=2026-02-15&page=2&per_page=500>; rel="next",
      <https://api.example.com/api/v1/devices/TLXABC12345/power?from=2026-02-01&to=2026-02-15&page=4&per_page=500>; rel="last"
```

---

## 9. Query Parameter Validation Summary

```mermaid
flowchart LR
    subgraph "Parse & Validate"
        A[Raw query string] --> B{from present?}
        B -->|No| ERR1[400: from is required]
        B -->|Yes| C{Valid date format?}
        C -->|No| ERR2[400: invalid date format]
        C -->|Yes| D{to present?}
        D -->|No| ERR3[400: to is required]
        D -->|Yes| E{to >= from?}
        E -->|No| ERR4[400: INVALID_DATE_RANGE]
        E -->|Yes| F{range <= 366 days?}
        F -->|No| ERR5[400: range too large]
        F -->|Yes| G{interval valid?}
        G -->|No| ERR6[400: INVALID_INTERVAL]
        G -->|Yes| H{tz loadable?}
        H -->|No| ERR7[400: INVALID_TIMEZONE]
        H -->|Yes| OK[Proceed to handler]
    end
```

---

## 10. OpenAPI Summary

Below is a condensed OpenAPI 3.1 specification covering all endpoints. The full spec would be generated from annotations or maintained in a separate `openapi.yaml`.

```yaml
openapi: 3.1.0
info:
  title: gogrowatt Solar Data API
  version: 0.1.0
  description: Read-only REST API for solar production data from Growatt inverters.

servers:
  - url: http://localhost:8080/api/v1
    description: Local development

paths:
  /health:
    get:
      summary: Service health and data freshness
      operationId: getHealth
      tags: [System]
      responses:
        '200': { description: Healthy }
        '503': { description: Degraded }

  /plants:
    get:
      summary: List all plants
      operationId: listPlants
      tags: [Plants]
      parameters:
        - $ref: '#/components/parameters/Page'
        - $ref: '#/components/parameters/PerPage'
      responses:
        '200': { description: Plant list with pagination }

  /plants/{plantID}:
    get:
      summary: Get plant details
      operationId: getPlant
      tags: [Plants]
      parameters:
        - name: plantID
          in: path
          required: true
          schema: { type: string }
      responses:
        '200': { description: Plant details }
        '404': { description: Plant not found }

  /plants/{plantID}/devices:
    get:
      summary: List devices in a plant
      operationId: listPlantDevices
      tags: [Plants, Devices]
      parameters:
        - name: plantID
          in: path
          required: true
          schema: { type: string }
        - $ref: '#/components/parameters/Page'
        - $ref: '#/components/parameters/PerPage'
      responses:
        '200': { description: Device list }
        '404': { description: Plant not found }

  /plants/{plantID}/power:
    get:
      summary: Plant-level aggregated power data
      operationId: getPlantPower
      tags: [Plants, Power]
      parameters:
        - name: plantID
          in: path
          required: true
          schema: { type: string }
        - $ref: '#/components/parameters/From'
        - $ref: '#/components/parameters/To'
        - $ref: '#/components/parameters/Interval'
        - $ref: '#/components/parameters/Timezone'
        - $ref: '#/components/parameters/Page'
        - $ref: '#/components/parameters/PerPageLarge'
      responses:
        '200': { description: Aggregated power time series }
        '400': { description: Validation error }
        '404': { description: Plant not found }

  /plants/{plantID}/energy:
    get:
      summary: Plant-level energy totals (from growatt.energy_summaries)
      operationId: getPlantEnergy
      tags: [Plants, Energy]
      parameters:
        - name: plantID
          in: path
          required: true
          schema: { type: string }
        - $ref: '#/components/parameters/From'
        - $ref: '#/components/parameters/To'
        - $ref: '#/components/parameters/Unit'
        - $ref: '#/components/parameters/Timezone'
      responses:
        '200': { description: Energy totals }

  /plants/{plantID}/stats:
    get:
      summary: Plant-level statistical summaries
      operationId: getPlantStats
      tags: [Plants, Stats]
      parameters:
        - name: plantID
          in: path
          required: true
          schema: { type: string }
        - $ref: '#/components/parameters/From'
        - $ref: '#/components/parameters/To'
        - $ref: '#/components/parameters/Timezone'
      responses:
        '200': { description: Hourly statistical summaries }

  /devices/{deviceSN}:
    get:
      summary: Device details with current status
      operationId: getDevice
      tags: [Devices]
      parameters:
        - name: deviceSN
          in: path
          required: true
          schema: { type: string }
      responses:
        '200': { description: Device details }
        '404': { description: Device not found }

  /devices/{deviceSN}/power:
    get:
      summary: Time-series power data with flexible aggregation
      operationId: getDevicePower
      tags: [Devices, Power]
      parameters:
        - name: deviceSN
          in: path
          required: true
          schema: { type: string }
        - $ref: '#/components/parameters/From'
        - $ref: '#/components/parameters/To'
        - $ref: '#/components/parameters/Interval'
        - $ref: '#/components/parameters/Fields'
        - $ref: '#/components/parameters/Timezone'
        - $ref: '#/components/parameters/Page'
        - $ref: '#/components/parameters/PerPageLarge'
      responses:
        '200': { description: Power readings }
        '400': { description: Validation error }
        '404': { description: Device not found }

  /devices/{deviceSN}/power/latest:
    get:
      summary: Most recent power reading
      operationId: getDevicePowerLatest
      tags: [Devices, Power]
      parameters:
        - name: deviceSN
          in: path
          required: true
          schema: { type: string }
      responses:
        '200': { description: Latest reading }
        '404': { description: Device not found or no data }

  /devices/{deviceSN}/energy:
    get:
      summary: Device-level energy totals (from growatt.mv_daily_production)
      operationId: getDeviceEnergy
      tags: [Devices, Energy]
      parameters:
        - name: deviceSN
          in: path
          required: true
          schema: { type: string }
        - $ref: '#/components/parameters/From'
        - $ref: '#/components/parameters/To'
        - $ref: '#/components/parameters/Unit'
        - $ref: '#/components/parameters/Timezone'
      responses:
        '200': { description: Energy totals }
        '400': { description: Validation error }
        '404': { description: Device not found }

  /devices/{deviceSN}/stats:
    get:
      summary: Statistical summaries (min, max, avg, median, stddev by hour)
      operationId: getDeviceStats
      tags: [Devices, Stats]
      parameters:
        - name: deviceSN
          in: path
          required: true
          schema: { type: string }
        - $ref: '#/components/parameters/From'
        - $ref: '#/components/parameters/To'
        - name: field
          in: query
          schema:
            type: string
            enum: [pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a]
            default: pac_w
        - $ref: '#/components/parameters/Timezone'
      responses:
        '200': { description: Hourly statistical summaries }
        '400': { description: Validation error }
        '404': { description: Device not found }

components:
  parameters:
    Page:
      name: page
      in: query
      schema: { type: integer, minimum: 1, default: 1 }
    PerPage:
      name: per_page
      in: query
      schema: { type: integer, minimum: 1, maximum: 100, default: 50 }
    PerPageLarge:
      name: per_page
      in: query
      schema: { type: integer, minimum: 1, maximum: 2000, default: 500 }
    From:
      name: from
      in: query
      required: true
      description: Start of range (RFC3339 or YYYY-MM-DD)
      schema: { type: string }
    To:
      name: to
      in: query
      required: true
      description: End of range (RFC3339 or YYYY-MM-DD)
      schema: { type: string }
    Interval:
      name: interval
      in: query
      schema:
        type: string
        enum: [5min, 15min, 1h, 1d]
        default: 5min
    Fields:
      name: fields
      in: query
      description: "Comma-separated list of measurement fields (pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a)"
      schema:
        type: string
        default: pac_w
    Unit:
      name: unit
      in: query
      schema:
        type: string
        enum: [day, month]
        default: day
    Timezone:
      name: tz
      in: query
      description: IANA timezone name
      schema:
        type: string
        default: UTC

  schemas:
    Meta:
      type: object
      properties:
        timestamp: { type: string, format: date-time }
        request_id: { type: string }

    Pagination:
      type: object
      properties:
        page: { type: integer }
        per_page: { type: integer }
        total: { type: integer }
        total_pages: { type: integer }

    APIError:
      type: object
      properties:
        code: { type: string }
        message: { type: string }
        details: {}
```

---

## 11. Project Layout

```
gogrowatt-api/
  cmd/
    gogrowatt-api/
      main.go              # Entrypoint: config, DB pool, start server
  internal/
    api/
      handlers.go          # HTTP handler functions
      middleware.go         # CORS, logging, request ID, cache headers
      response.go          # Envelope, error helpers, writeJSON
      params.go            # Query parameter parsing & validation
    repository/
      plants.go            # Plant queries
      devices.go           # Device queries
      power.go             # Power reading queries (with aggregation)
      energy.go            # Energy summary queries
      stats.go             # Statistical aggregation queries
      health.go            # DB health + data freshness
    config/
      config.go            # Environment-based config (DB DSN, port, CORS origins)
  openapi.yaml             # Full OpenAPI 3.1 spec
  Dockerfile
  go.mod
  go.sum
```

---

## 12. Configuration

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `API_PORT` | `8080` | Listen port |
| `API_READ_TIMEOUT` | `30s` | HTTP read timeout |
| `API_WRITE_TIMEOUT` | `30s` | HTTP write timeout |
| `DATABASE_URL` | -- | PostgreSQL connection string |
| `DB_MAX_CONNS` | `10` | Max DB pool connections |
| `DB_MAX_IDLE_CONNS` | `5` | Max idle connections |
| `CORS_ORIGINS` | `*` | Comma-separated allowed origins |
| `STALE_THRESHOLD` | `900` | Seconds before data is considered stale |
| `LOG_LEVEL` | `info` | Log level: debug, info, warn, error |

---

## 13. Testing

### 13.1 Test Environment Setup

The test suite requires a Go test server and a test database running TimescaleDB.

**Database setup:**

```bash
# Start a TimescaleDB container for tests
docker run -d --name gogrowatt-test-db \
  -e POSTGRES_USER=testuser \
  -e POSTGRES_PASSWORD=testpass \
  -e POSTGRES_DB=gogrowatt_test \
  -p 5433:5432 \
  timescale/timescaledb:latest-pg16

# Apply the schema (from 01-database-schema.md)
psql "postgresql://testuser:testpass@localhost:5433/gogrowatt_test" \
  -f schema/migrations/001_initial.sql
```

**Go test server:**

```go
// internal/testutil/server.go
package testutil

import (
    "net/http/httptest"
    "testing"

    "github.com/jackc/pgx/v5/pgxpool"
)

// NewTestServer creates a fully wired httptest.Server backed by a real
// (or mocked) database pool.  The caller is responsible for calling
// ts.Close() and pool.Close() when done.
func NewTestServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
    t.Helper()
    repo := repository.New(pool)
    h := api.NewHandlers(repo)
    router := newRouter(h)
    return httptest.NewServer(router)
}
```

Set `DATABASE_URL` for the test run:

```bash
export TEST_DATABASE_URL="postgresql://testuser:testpass@localhost:5433/gogrowatt_test"
go test ./...
```

### 13.2 Unit Tests -- Handler Layer

Mock the repository interface and verify JSON structure, HTTP status codes, and error handling for each handler.

```go
// internal/api/handlers_test.go
func TestGetDevicePower_Success(t *testing.T) {
    mockRepo := &mocks.Repository{
        GetPowerReadingsFunc: func(ctx context.Context, sn string, p PowerParams) ([]PowerPoint, int, error) {
            return []PowerPoint{
                {Time: time.Date(2026, 2, 15, 13, 0, 0, 0, time.UTC), PacW: 5120.5},
            }, 1, nil
        },
    }
    h := NewHandlers(mockRepo)
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    resp, err := http.Get(srv.URL + "/api/v1/devices/TLXABC12345/power?from=2026-02-15&to=2026-02-15")
    require.NoError(t, err)
    assert.Equal(t, 200, resp.StatusCode)

    var env Envelope[PowerResponse]
    json.NewDecoder(resp.Body).Decode(&env)
    assert.Len(t, env.Data.Readings, 1)
    assert.Equal(t, 5120.5, env.Data.Readings[0].PacW)
    assert.NotEmpty(t, env.Meta.RequestID)
}

func TestGetDevicePower_MissingFrom(t *testing.T) {
    h := NewHandlers(&mocks.Repository{})
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    resp, _ := http.Get(srv.URL + "/api/v1/devices/TLXABC12345/power?to=2026-02-15")
    assert.Equal(t, 400, resp.StatusCode)

    var env ErrorEnvelope
    json.NewDecoder(resp.Body).Decode(&env)
    assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
}

func TestGetDevice_NotFound(t *testing.T) {
    mockRepo := &mocks.Repository{
        GetDeviceFunc: func(ctx context.Context, sn string) (*Device, error) {
            return nil, repository.ErrNotFound
        },
    }
    h := NewHandlers(mockRepo)
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    resp, _ := http.Get(srv.URL + "/api/v1/devices/NONEXISTENT")
    assert.Equal(t, 404, resp.StatusCode)
}

func TestGetDevicePower_DBTimeout(t *testing.T) {
    mockRepo := &mocks.Repository{
        GetPowerReadingsFunc: func(ctx context.Context, sn string, p PowerParams) ([]PowerPoint, int, error) {
            return nil, 0, context.DeadlineExceeded
        },
    }
    h := NewHandlers(mockRepo)
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    resp, _ := http.Get(srv.URL + "/api/v1/devices/TLXABC12345/power?from=2026-02-15&to=2026-02-15")
    assert.Equal(t, 504, resp.StatusCode)
}
```

### 13.3 Integration Tests

Load known test data into the database, query each endpoint, and verify responses match expected values.

```go
// internal/api/integration_test.go
//go:build integration

func TestIntegration_DevicePower(t *testing.T) {
    pool := testutil.MustConnectTestDB(t)
    defer pool.Close()
    testutil.SeedPowerReadings(t, pool, "TLXTEST001", []testutil.Reading{
        {Ts: "2026-02-15T13:00:00Z", PacW: 5000.0, PpvW: 5100.0, Vpv1V: 320.0, Ipv1A: 8.0},
        {Ts: "2026-02-15T13:05:00Z", PacW: 5100.0, PpvW: 5200.0, Vpv1V: 322.0, Ipv1A: 8.1},
        {Ts: "2026-02-15T13:10:00Z", PacW: 5200.0, PpvW: 5300.0, Vpv1V: 324.0, Ipv1A: 8.2},
    })

    srv := testutil.NewTestServer(t, pool)
    defer srv.Close()

    resp, err := http.Get(srv.URL + "/api/v1/devices/TLXTEST001/power?from=2026-02-15&to=2026-02-15&fields=pac_w,vpv1_v")
    require.NoError(t, err)
    assert.Equal(t, 200, resp.StatusCode)

    var env Envelope[PowerResponse]
    json.NewDecoder(resp.Body).Decode(&env)
    assert.Equal(t, 3, env.Pagination.Total)
    assert.Equal(t, "TLXTEST001", env.Data.SerialNumber)
}
```

### 13.4 Parameter Validation Tests

Test all validation boundaries: invalid dates, out-of-range values, missing required params, bad timezone names.

```go
func TestValidation(t *testing.T) {
    h := NewHandlers(&mocks.Repository{})
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    tests := []struct {
        name   string
        url    string
        status int
        code   string
    }{
        {"missing from",        "/api/v1/devices/ABC/power?to=2026-02-15",                         400, "VALIDATION_ERROR"},
        {"missing to",          "/api/v1/devices/ABC/power?from=2026-02-15",                        400, "VALIDATION_ERROR"},
        {"invalid date format", "/api/v1/devices/ABC/power?from=not-a-date&to=2026-02-15",          400, "VALIDATION_ERROR"},
        {"to before from",      "/api/v1/devices/ABC/power?from=2026-02-15&to=2026-02-01",          400, "INVALID_DATE_RANGE"},
        {"range too large",     "/api/v1/devices/ABC/power?from=2024-01-01&to=2026-02-15",          400, "INVALID_DATE_RANGE"},
        {"bad interval",        "/api/v1/devices/ABC/power?from=2026-02-15&to=2026-02-15&interval=2h", 400, "INVALID_INTERVAL"},
        {"bad timezone",        "/api/v1/devices/ABC/power?from=2026-02-15&to=2026-02-15&tz=Mars/Olympus", 400, "INVALID_TIMEZONE"},
        {"page zero",           "/api/v1/devices/ABC/power?from=2026-02-15&to=2026-02-15&page=0",   400, "VALIDATION_ERROR"},
        {"per_page too large",  "/api/v1/devices/ABC/power?from=2026-02-15&to=2026-02-15&per_page=9999", 400, "VALIDATION_ERROR"},
        {"bad fields",          "/api/v1/devices/ABC/power?from=2026-02-15&to=2026-02-15&fields=foo", 400, "VALIDATION_ERROR"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            resp, _ := http.Get(srv.URL + tt.url)
            assert.Equal(t, tt.status, resp.StatusCode)
            var env ErrorEnvelope
            json.NewDecoder(resp.Body).Decode(&env)
            assert.Equal(t, tt.code, env.Error.Code)
        })
    }
}
```

### 13.5 Pagination Tests

Insert 1000+ readings and verify `page`, `per_page`, `total`, and `total_pages` are correct.

```go
//go:build integration

func TestPagination_Power(t *testing.T) {
    pool := testutil.MustConnectTestDB(t)
    defer pool.Close()

    // Insert 1080 readings (9 hours of 5-min data)
    readings := make([]testutil.Reading, 1080)
    base := time.Date(2026, 2, 10, 6, 0, 0, 0, time.UTC)
    for i := range readings {
        readings[i] = testutil.Reading{
            Ts:   base.Add(time.Duration(i) * 5 * time.Minute).Format(time.RFC3339),
            PacW: float64(1000 + i),
        }
    }
    testutil.SeedPowerReadings(t, pool, "TLXPAGE001", readings)

    srv := testutil.NewTestServer(t, pool)
    defer srv.Close()

    // Page 1 of 500
    resp, _ := http.Get(srv.URL + "/api/v1/devices/TLXPAGE001/power?from=2026-02-10&to=2026-02-14&per_page=500&page=1")
    var env Envelope[PowerResponse]
    json.NewDecoder(resp.Body).Decode(&env)

    assert.Equal(t, 1, env.Pagination.Page)
    assert.Equal(t, 500, env.Pagination.PerPage)
    assert.Equal(t, 1080, env.Pagination.Total)
    assert.Equal(t, 3, env.Pagination.TotalPages) // ceil(1080/500)
    assert.Len(t, env.Data.Readings, 500)

    // Page 3 (last page, partial)
    resp, _ = http.Get(srv.URL + "/api/v1/devices/TLXPAGE001/power?from=2026-02-10&to=2026-02-14&per_page=500&page=3")
    json.NewDecoder(resp.Body).Decode(&env)
    assert.Equal(t, 3, env.Pagination.Page)
    assert.Len(t, env.Data.Readings, 80) // 1080 - 2*500
}
```

### 13.6 Aggregation Accuracy Tests

Insert known 5-minute data and verify that 15min, 1h, and 1d aggregations produce mathematically correct results.

```go
//go:build integration

func TestAggregation_1h(t *testing.T) {
    pool := testutil.MustConnectTestDB(t)
    defer pool.Close()

    // Insert 12 readings in the 13:00 hour (13:00, 13:05, ..., 13:55)
    readings := []testutil.Reading{
        {Ts: "2026-02-15T13:00:00Z", PacW: 100.0},
        {Ts: "2026-02-15T13:05:00Z", PacW: 200.0},
        {Ts: "2026-02-15T13:10:00Z", PacW: 300.0},
        {Ts: "2026-02-15T13:15:00Z", PacW: 400.0},
        {Ts: "2026-02-15T13:20:00Z", PacW: 500.0},
        {Ts: "2026-02-15T13:25:00Z", PacW: 600.0},
        {Ts: "2026-02-15T13:30:00Z", PacW: 700.0},
        {Ts: "2026-02-15T13:35:00Z", PacW: 800.0},
        {Ts: "2026-02-15T13:40:00Z", PacW: 900.0},
        {Ts: "2026-02-15T13:45:00Z", PacW: 1000.0},
        {Ts: "2026-02-15T13:50:00Z", PacW: 1100.0},
        {Ts: "2026-02-15T13:55:00Z", PacW: 1200.0},
    }
    testutil.SeedPowerReadings(t, pool, "TLXAGG001", readings)

    srv := testutil.NewTestServer(t, pool)
    defer srv.Close()

    // Query at 1h interval
    resp, _ := http.Get(srv.URL + "/api/v1/devices/TLXAGG001/power?from=2026-02-15&to=2026-02-15&interval=1h&fields=pac_w&tz=UTC")
    var env Envelope[PowerResponse]
    json.NewDecoder(resp.Body).Decode(&env)

    require.Len(t, env.Data.Readings, 1)
    bucket := env.Data.Readings[0]
    // avg of 100..1200 step 100 = 650.0
    assert.InDelta(t, 650.0, bucket.PacW.Avg, 0.01)
    assert.InDelta(t, 100.0, bucket.PacW.Min, 0.01)
    assert.InDelta(t, 1200.0, bucket.PacW.Max, 0.01)
    assert.Equal(t, 12, bucket.PacW.Samples)

    // Query at 15min interval -- should produce 4 buckets
    resp, _ = http.Get(srv.URL + "/api/v1/devices/TLXAGG001/power?from=2026-02-15&to=2026-02-15&interval=15min&fields=pac_w&tz=UTC")
    json.NewDecoder(resp.Body).Decode(&env)
    require.Len(t, env.Data.Readings, 4)
    // First 15min bucket (13:00, 13:05, 13:10): avg = 200.0
    assert.InDelta(t, 200.0, env.Data.Readings[0].PacW.Avg, 0.01)
}
```

### 13.7 Timezone Handling Tests

Insert UTC data, query with a non-UTC timezone, and verify bucket alignment shifts correctly.

```go
//go:build integration

func TestTimezone_BucketAlignment(t *testing.T) {
    pool := testutil.MustConnectTestDB(t)
    defer pool.Close()

    // Insert readings that straddle midnight in US/Central (UTC-6)
    // 2026-02-15 05:55 UTC = 2026-02-14 23:55 Central
    // 2026-02-15 06:05 UTC = 2026-02-15 00:05 Central
    readings := []testutil.Reading{
        {Ts: "2026-02-15T05:55:00Z", PacW: 10.0},
        {Ts: "2026-02-15T06:00:00Z", PacW: 20.0},
        {Ts: "2026-02-15T06:05:00Z", PacW: 30.0},
    }
    testutil.SeedPowerReadings(t, pool, "TLXTZ001", readings)

    srv := testutil.NewTestServer(t, pool)
    defer srv.Close()

    // Query for 2026-02-15 in US/Central -- should only include the 06:00 and 06:05 UTC readings
    resp, _ := http.Get(srv.URL + "/api/v1/devices/TLXTZ001/power?from=2026-02-15&to=2026-02-15&tz=US/Central")
    var env Envelope[PowerResponse]
    json.NewDecoder(resp.Body).Decode(&env)

    assert.Equal(t, 2, env.Pagination.Total, "05:55 UTC = Feb 14 Central, should be excluded")

    // Query for 2026-02-14 in US/Central -- should include the 05:55 UTC reading
    resp, _ = http.Get(srv.URL + "/api/v1/devices/TLXTZ001/power?from=2026-02-14&to=2026-02-14&tz=US/Central")
    json.NewDecoder(resp.Body).Decode(&env)
    assert.Equal(t, 1, env.Pagination.Total)
}
```

### 13.8 Cache Header Tests

Verify `Cache-Control` values differ for historical vs live (includes-today) queries.

```go
func TestCacheHeaders(t *testing.T) {
    mockRepo := &mocks.Repository{
        GetPowerReadingsFunc: func(ctx context.Context, sn string, p PowerParams) ([]PowerPoint, int, error) {
            return []PowerPoint{}, 0, nil
        },
    }
    h := NewHandlers(mockRepo)
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    // Historical query (well in the past)
    resp, _ := http.Get(srv.URL + "/api/v1/devices/ABC/power?from=2025-01-01&to=2025-01-02")
    assert.Equal(t, "public, max-age=300", resp.Header.Get("Cache-Control"))

    // Live query (includes today)
    today := time.Now().Format("2006-01-02")
    resp, _ = http.Get(srv.URL + "/api/v1/devices/ABC/power?from=" + today + "&to=" + today)
    assert.Equal(t, "public, max-age=60", resp.Header.Get("Cache-Control"))

    // Latest endpoint -- always short cache
    resp, _ = http.Get(srv.URL + "/api/v1/devices/ABC/power/latest")
    assert.Equal(t, "public, max-age=30", resp.Header.Get("Cache-Control"))

    // Health endpoint -- no-cache
    resp, _ = http.Get(srv.URL + "/api/v1/health")
    assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
}
```

### 13.9 CORS Tests

Verify preflight `OPTIONS` requests return the correct CORS headers.

```go
func TestCORS_Preflight(t *testing.T) {
    h := NewHandlers(&mocks.Repository{})
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    req, _ := http.NewRequest("OPTIONS", srv.URL+"/api/v1/devices/ABC/power", nil)
    req.Header.Set("Origin", "https://solar.example.com")
    req.Header.Set("Access-Control-Request-Method", "GET")
    req.Header.Set("Access-Control-Request-Headers", "Content-Type")

    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    assert.Equal(t, 200, resp.StatusCode)
    assert.Equal(t, "https://solar.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
    assert.Contains(t, resp.Header.Get("Access-Control-Allow-Methods"), "GET")
    assert.Equal(t, "86400", resp.Header.Get("Access-Control-Max-Age"))
}

func TestCORS_DisallowedOrigin(t *testing.T) {
    h := NewHandlers(&mocks.Repository{})
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    req, _ := http.NewRequest("GET", srv.URL+"/api/v1/health", nil)
    req.Header.Set("Origin", "https://evil.example.com")

    resp, _ := http.DefaultClient.Do(req)
    assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
}
```

### 13.10 Error Response Tests

Verify correct HTTP status codes and error envelope structure for each error scenario.

```go
func TestErrorResponses(t *testing.T) {
    mockRepo := &mocks.Repository{
        GetDeviceFunc: func(ctx context.Context, sn string) (*Device, error) {
            return nil, repository.ErrNotFound
        },
        GetPowerReadingsFunc: func(ctx context.Context, sn string, p PowerParams) ([]PowerPoint, int, error) {
            return nil, 0, context.DeadlineExceeded
        },
    }
    h := NewHandlers(mockRepo)
    srv := httptest.NewServer(newRouter(h))
    defer srv.Close()

    t.Run("404 for missing device", func(t *testing.T) {
        resp, _ := http.Get(srv.URL + "/api/v1/devices/NOSUCHDEVICE")
        assert.Equal(t, 404, resp.StatusCode)
        var env ErrorEnvelope
        json.NewDecoder(resp.Body).Decode(&env)
        assert.Equal(t, "NOT_FOUND", env.Error.Code)
        assert.NotEmpty(t, env.Meta.RequestID)
        assert.NotEmpty(t, env.Meta.Timestamp)
    })

    t.Run("400 for bad params", func(t *testing.T) {
        resp, _ := http.Get(srv.URL + "/api/v1/devices/ABC/power")
        assert.Equal(t, 400, resp.StatusCode)
        var env ErrorEnvelope
        json.NewDecoder(resp.Body).Decode(&env)
        assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
    })

    t.Run("504 for DB timeout", func(t *testing.T) {
        resp, _ := http.Get(srv.URL + "/api/v1/devices/ABC/power?from=2026-02-15&to=2026-02-15")
        assert.Equal(t, 504, resp.StatusCode)
        var env ErrorEnvelope
        json.NewDecoder(resp.Body).Decode(&env)
        assert.Equal(t, "TIMEOUT", env.Error.Code)
    })
}
```

### 13.11 Manual Testing with curl

Example `curl` commands for exercising each endpoint:

```bash
# Health check
curl -s http://localhost:8080/api/v1/health | jq .

# List plants
curl -s 'http://localhost:8080/api/v1/plants?page=1&per_page=10' | jq .

# Get plant details
curl -s http://localhost:8080/api/v1/plants/12345 | jq .

# List devices in a plant
curl -s http://localhost:8080/api/v1/plants/12345/devices | jq .

# Get device details
curl -s http://localhost:8080/api/v1/devices/TLXABC12345 | jq .

# Power readings -- raw 5min for a single day
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/power?from=2026-02-15&to=2026-02-15&tz=US/Central' | jq .

# Power readings -- hourly aggregation, multiple fields
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/power?from=2026-02-15&to=2026-02-15&interval=1h&fields=pac_w,vpv1_v,vpv2_v&tz=US/Central' | jq .

# Power readings -- daily aggregation over a range, with pagination
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/power?from=2026-02-01&to=2026-02-15&interval=1d&per_page=100&page=1' | jq .

# Latest reading
curl -s http://localhost:8080/api/v1/devices/TLXABC12345/power/latest | jq .

# Device energy (daily) -- queries growatt.mv_daily_production
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/energy?from=2026-02-01&to=2026-02-15&unit=day&tz=US/Central' | jq .

# Device energy (monthly)
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/energy?from=2025-01-01&to=2026-02-15&unit=month' | jq .

# Device stats
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/stats?from=2026-02-12&to=2026-02-15&field=pac_w&tz=US/Central' | jq .

# Plant-level power (aggregated across all devices)
curl -s 'http://localhost:8080/api/v1/plants/12345/power?from=2026-02-15&to=2026-02-15&interval=1h&tz=US/Central' | jq .

# Plant-level energy -- queries growatt.energy_summaries
curl -s 'http://localhost:8080/api/v1/plants/12345/energy?from=2026-02-01&to=2026-02-15&unit=day' | jq .

# Plant-level stats
curl -s 'http://localhost:8080/api/v1/plants/12345/stats?from=2026-02-12&to=2026-02-15&tz=US/Central' | jq .

# Test validation errors
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/power' | jq .                    # missing from/to
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/power?from=bad&to=bad' | jq .     # invalid dates
curl -s 'http://localhost:8080/api/v1/devices/TLXABC12345/power?from=2026-02-15&to=2026-02-15&tz=Fake/Zone' | jq .  # bad tz

# Test 404
curl -s http://localhost:8080/api/v1/devices/NONEXISTENT | jq .

# Test CORS preflight
curl -s -X OPTIONS http://localhost:8080/api/v1/health \
  -H "Origin: https://solar.example.com" \
  -H "Access-Control-Request-Method: GET" \
  -D - -o /dev/null

# Inspect cache headers
curl -s -D - -o /dev/null 'http://localhost:8080/api/v1/devices/TLXABC12345/power?from=2025-01-01&to=2025-01-02'
curl -s -D - -o /dev/null http://localhost:8080/api/v1/devices/TLXABC12345/power/latest
```
