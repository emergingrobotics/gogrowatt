# GoGrowatt Database Schema

## Overview

This document defines a PostgreSQL + TimescaleDB schema for storing solar production data collected from Growatt inverters via the OpenAPI. The schema is designed around three principles:

1. **Append-friendly time-series storage** using TimescaleDB hypertables for high-frequency power readings.
2. **Upsert-safe dimension tables** for plants and devices, where the latest API fetch always wins.
3. **Collection metadata tracking** to detect gaps and audit data freshness.

The data model captures everything exposed by the `gogrowatt` Go client library:

| Source Struct | What it represents | Target table(s) |
|---|---|---|
| `Plant` | Power station metadata (location, capacity, environmental formulas) | `growatt.plants` |
| `PlantData` | Real-time plant energy overview | `growatt.plant_snapshots` |
| `Device` | Inverter/device identity | `growatt.devices` |
| `MINInverterData` | Real-time inverter telemetry (voltages, currents, temperature) | `growatt.device_snapshots` |
| `MINHistoryDataPoint` | 5-min interval readings (pac, ppv, vpv1/2, ipv1/2, vac1, iac1) | `growatt.power_readings` |
| `PowerDataPoint` | 5-min plant-level AC power | `growatt.power_readings` |
| `EnergyDataPoint` | Daily or monthly energy totals (plant-level) | `growatt.energy_summaries` |
| -- | Collection run tracking | `growatt.collection_runs`, `growatt.collection_gaps` |

> **Note:** `growatt.energy_summaries` stores **plant-level** daily and monthly energy totals
> from the Growatt API. For **device-level** daily energy, use the materialized view
> `growatt.mv_daily_production`, which estimates energy from per-device 5-minute power readings.

---

## Entity-Relationship Diagram

```mermaid
erDiagram
    plants {
        text plant_id PK
        text plant_name
        int plant_type
        text country
        text city
        double latitude
        double longitude
        double peak_power_kw
        double formula_coal
        double formula_co2
        double formula_money
        double formula_tree
        text money_unit
        text money_unit_text
        date create_date
        int status
        timestamptz updated_at
    }

    devices {
        text device_sn PK
        text plant_id FK
        int device_type
        text device_name
        text model
        int status
        timestamptz last_update
        timestamptz updated_at
    }

    plant_snapshots {
        bigint id PK
        text plant_id FK
        timestamptz captured_at
        double current_power_w
        double today_energy_kwh
        double month_energy_kwh
        double year_energy_kwh
        double total_energy_kwh
        double peak_power_today_kw
    }

    device_snapshots {
        bigint id PK
        text device_sn FK
        timestamptz captured_at
        double pac_w
        double etoday_kwh
        double etotal_kwh
        double vpv1_v
        double vpv2_v
        double ipv1_a
        double ipv2_a
        double vac1_v
        double iac1_a
        double fac_hz
        double temperature_c
        int status
    }

    power_readings {
        timestamptz ts PK
        text device_sn PK
        double pac_w
        double ppv_w
        double vpv1_v
        double vpv2_v
        double ipv1_a
        double ipv2_a
        double vac1_v
        double iac1_a
        timestamptz fetched_at
    }

    energy_summaries {
        date period_date PK
        text plant_id PK
        text time_unit PK
        double energy_kwh
        timestamptz updated_at
    }

    collection_runs {
        bigint id PK
        text source_type
        text source_id
        date query_date_start
        date query_date_end
        int points_collected
        timestamptz started_at
        timestamptz finished_at
        text status
        text error_message
    }

    collection_gaps {
        bigint id PK
        text device_sn FK
        date gap_date
        time expected_start
        time expected_end
        text reason
        timestamptz detected_at
    }

    plants ||--o{ devices : "has"
    plants ||--o{ plant_snapshots : "has"
    plants ||--o{ energy_summaries : "has"
    devices ||--o{ device_snapshots : "has"
    devices ||--o{ power_readings : "has"
    devices ||--o{ collection_gaps : "has"
```

---

## CREATE TABLE Statements

### Extension Setup

```sql
-- Required extensions
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS pg_trgm;  -- for text search on plant/device names

-- Schema namespace
CREATE SCHEMA IF NOT EXISTS growatt;
SET search_path TO growatt, public;
```

### Dimension Tables

```sql
-- ============================================================
-- plants: Power station metadata
-- Source: growatt.Plant struct
-- ============================================================
CREATE TABLE growatt.plants (
    plant_id        TEXT        PRIMARY KEY,
    plant_name      TEXT        NOT NULL,
    plant_type      INT         NOT NULL DEFAULT 0,
    country         TEXT,
    city            TEXT,
    latitude        DOUBLE PRECISION,
    longitude       DOUBLE PRECISION,
    peak_power_kw   DOUBLE PRECISION,   -- rated peak capacity
    formula_coal    DOUBLE PRECISION,
    formula_co2     DOUBLE PRECISION,
    formula_money   DOUBLE PRECISION,
    formula_tree    DOUBLE PRECISION,
    money_unit      TEXT,
    money_unit_text TEXT,
    create_date     DATE,
    status          INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  growatt.plants IS 'Growatt power station (plant) metadata, synced from API plant/list and plant/details endpoints.';
COMMENT ON COLUMN growatt.plants.peak_power_kw IS 'Rated peak power capacity in kW.';
COMMENT ON COLUMN growatt.plants.status IS '0=offline, 1=online (per Growatt API).';
COMMENT ON COLUMN growatt.plants.formula_coal IS 'Coal savings conversion factor (kg per kWh).';
COMMENT ON COLUMN growatt.plants.formula_co2 IS 'CO2 reduction conversion factor (kg per kWh).';
COMMENT ON COLUMN growatt.plants.formula_money IS 'Revenue per kWh in local currency.';
COMMENT ON COLUMN growatt.plants.formula_tree IS 'Tree-equivalent conversion factor.';


-- ============================================================
-- devices: Inverter / device identity
-- Source: growatt.Device struct
-- ============================================================
CREATE TABLE growatt.devices (
    device_sn       TEXT        PRIMARY KEY,
    plant_id        TEXT        NOT NULL REFERENCES growatt.plants(plant_id),
    device_type     INT         NOT NULL DEFAULT 0,
    device_name     TEXT,
    model           TEXT,
    status          INT         NOT NULL DEFAULT 0,
    last_update     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  growatt.devices IS 'Inverter/device metadata, synced from API device/list endpoint.';
COMMENT ON COLUMN growatt.devices.device_type IS 'Growatt device type code (e.g. MIN, TLX, SPH, MIX).';
COMMENT ON COLUMN growatt.devices.status IS '0=offline, 1=online.';
```

### Time-Series Fact Tables

```sql
-- ============================================================
-- power_readings: 5-minute interval telemetry (HYPERTABLE)
-- Source: growatt.MINHistoryDataPoint struct
--         (also stores plant-level PowerDataPoint as pac_w only)
-- ============================================================
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
    fetched_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),  -- when this row was fetched from the API (audit trail; latest wins on upsert)
    CONSTRAINT power_readings_pkey PRIMARY KEY (ts, device_sn)
);

-- Convert to TimescaleDB hypertable partitioned by time
-- Chunk interval = 7 days matches the Growatt API max query range
SELECT create_hypertable(
    'growatt.power_readings',
    by_range('ts', INTERVAL '7 days')
);

COMMENT ON TABLE growatt.power_readings IS '5-minute interval power readings from MIN/TLX inverters. TimescaleDB hypertable with 7-day chunks.';
COMMENT ON COLUMN growatt.power_readings.ts IS 'Timestamp of the reading, constructed from date + time fields returned by the API.';
COMMENT ON COLUMN growatt.power_readings.pac_w IS 'AC output power in watts. This is the grid-delivered power.';
COMMENT ON COLUMN growatt.power_readings.ppv_w IS 'Total PV input power in watts (sum of all strings). May be NULL for plant-level data.';
COMMENT ON COLUMN growatt.power_readings.fetched_at IS 'Timestamp when this reading was fetched from the Growatt API. Used for audit trail; on upsert the latest fetch always wins.';


-- ============================================================
-- plant_snapshots: Point-in-time plant energy overview
-- Source: growatt.PlantData struct
-- ============================================================
CREATE TABLE growatt.plant_snapshots (
    id              BIGINT GENERATED ALWAYS AS IDENTITY,
    plant_id        TEXT            NOT NULL REFERENCES growatt.plants(plant_id),
    captured_at     TIMESTAMPTZ     NOT NULL DEFAULT now(),
    current_power_w     DOUBLE PRECISION,
    today_energy_kwh    DOUBLE PRECISION,
    month_energy_kwh    DOUBLE PRECISION,
    year_energy_kwh     DOUBLE PRECISION,
    total_energy_kwh    DOUBLE PRECISION,
    peak_power_today_kw DOUBLE PRECISION,
    CONSTRAINT plant_snapshots_pkey PRIMARY KEY (id, captured_at)
);

SELECT create_hypertable(
    'growatt.plant_snapshots',
    by_range('captured_at', INTERVAL '30 days')
);

COMMENT ON TABLE growatt.plant_snapshots IS 'Point-in-time snapshots of plant-level energy counters from plant/data endpoint. Useful for tracking cumulative energy and current power.';


-- ============================================================
-- device_snapshots: Point-in-time inverter telemetry
-- Source: growatt.MINInverterData struct
-- ============================================================
CREATE TABLE growatt.device_snapshots (
    id              BIGINT GENERATED ALWAYS AS IDENTITY,
    device_sn       TEXT            NOT NULL,
    captured_at     TIMESTAMPTZ     NOT NULL DEFAULT now(),
    pac_w           DOUBLE PRECISION,
    etoday_kwh      DOUBLE PRECISION,
    etotal_kwh      DOUBLE PRECISION,
    vpv1_v          DOUBLE PRECISION,
    vpv2_v          DOUBLE PRECISION,
    ipv1_a          DOUBLE PRECISION,
    ipv2_a          DOUBLE PRECISION,
    vac1_v          DOUBLE PRECISION,
    iac1_a          DOUBLE PRECISION,
    fac_hz          DOUBLE PRECISION,   -- grid frequency
    temperature_c   DOUBLE PRECISION,   -- inverter temperature
    status          INT,
    CONSTRAINT device_snapshots_pkey PRIMARY KEY (id, captured_at)
);

SELECT create_hypertable(
    'growatt.device_snapshots',
    by_range('captured_at', INTERVAL '30 days')
);

COMMENT ON TABLE growatt.device_snapshots IS 'Real-time inverter snapshots from device/tlx/tlx_data_info. Captures instantaneous voltages, currents, frequency, and temperature.';
COMMENT ON COLUMN growatt.device_snapshots.fac_hz IS 'AC grid frequency in Hz.';
COMMENT ON COLUMN growatt.device_snapshots.temperature_c IS 'Inverter internal temperature in degrees Celsius.';


-- ============================================================
-- energy_summaries: Daily and monthly energy totals (PLANT-LEVEL)
-- Source: growatt.EnergyDataPoint struct
--
-- NOTE: This table stores plant-level energy totals from the
-- Growatt API. For device-level daily energy estimates, query
-- the materialized view growatt.mv_daily_production instead.
-- ============================================================
CREATE TABLE growatt.energy_summaries (
    period_date     DATE            NOT NULL,
    plant_id        TEXT            NOT NULL REFERENCES growatt.plants(plant_id),
    time_unit       TEXT            NOT NULL CHECK (time_unit IN ('day', 'month')),
    energy_kwh      DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ     NOT NULL DEFAULT now(),
    CONSTRAINT energy_summaries_pkey PRIMARY KEY (period_date, plant_id, time_unit)
);

COMMENT ON TABLE growatt.energy_summaries IS 'Plant-level daily and monthly energy production totals from the plant/energy API endpoint. For device-level daily energy, use growatt.mv_daily_production.';
COMMENT ON COLUMN growatt.energy_summaries.time_unit IS 'Granularity: ''day'' for daily totals, ''month'' for monthly totals.';
COMMENT ON COLUMN growatt.energy_summaries.period_date IS 'For daily: the date. For monthly: the first day of the month.';
```

### Collection Metadata Tables

```sql
-- ============================================================
-- collection_runs: Audit log for each data fetch
-- ============================================================
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

COMMENT ON TABLE growatt.collection_runs IS 'Audit trail for every data collection run. Used to detect missing dates and troubleshoot API failures.';


-- ============================================================
-- collection_gaps: Known gaps in time-series data
-- ============================================================
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

COMMENT ON TABLE growatt.collection_gaps IS 'Tracks known gaps where expected readings are missing. Enables targeted backfill operations.';
```

---

## Index Strategy

### Primary Access Patterns

The most common queries against this schema are:

1. **Time-range scans on power_readings** -- "show me data for device X between date A and date B"
2. **Daily/hourly aggregations** -- "average power by hour for this week"
3. **Latest snapshot lookups** -- "what is the current state of plant/device Y?"
4. **Energy summary lookups** -- "monthly production for plant Z in 2026"

### Index Definitions

```sql
-- ============================================================
-- power_readings indexes
-- ============================================================

-- Primary index is (ts, device_sn) via the hypertable PK.
-- TimescaleDB automatically creates chunk-level indexes.

-- Device-first lookup for "all data for one device in a time range"
CREATE INDEX idx_power_readings_device_ts
    ON growatt.power_readings (device_sn, ts DESC);

-- ============================================================
-- plant_snapshots indexes
-- ============================================================
CREATE INDEX idx_plant_snapshots_plant_time
    ON growatt.plant_snapshots (plant_id, captured_at DESC);

-- ============================================================
-- device_snapshots indexes
-- ============================================================
CREATE INDEX idx_device_snapshots_device_time
    ON growatt.device_snapshots (device_sn, captured_at DESC);

-- ============================================================
-- energy_summaries indexes
-- ============================================================

-- Already has PK on (period_date, plant_id, time_unit).
-- Add reverse lookup: all dates for a plant + unit
CREATE INDEX idx_energy_summaries_plant_unit_date
    ON growatt.energy_summaries (plant_id, time_unit, period_date DESC);

-- ============================================================
-- Dimension table indexes
-- ============================================================
CREATE INDEX idx_devices_plant_id ON growatt.devices (plant_id);
CREATE INDEX idx_plants_status    ON growatt.plants (status);
```

### TimescaleDB Compression (for older data)

```sql
-- Enable compression on power_readings after 30 days
ALTER TABLE growatt.power_readings
    SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'device_sn',
        timescaledb.compress_orderby = 'ts DESC'
    );

SELECT add_compression_policy('growatt.power_readings', INTERVAL '30 days');

-- Enable compression on device_snapshots after 30 days
ALTER TABLE growatt.device_snapshots
    SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'device_sn',
        timescaledb.compress_orderby = 'captured_at DESC'
    );

SELECT add_compression_policy('growatt.device_snapshots', INTERVAL '30 days');

-- Enable compression on plant_snapshots after 30 days
ALTER TABLE growatt.plant_snapshots
    SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'plant_id',
        timescaledb.compress_orderby = 'captured_at DESC'
    );

SELECT add_compression_policy('growatt.plant_snapshots', INTERVAL '30 days');
```

---

## Upsert Patterns

All ingestion queries should use `INSERT ... ON CONFLICT` so re-fetching the same time period overwrites stale data cleanly.

```sql
-- Upsert a 5-minute power reading
-- On conflict, the latest fetch wins: all measurement columns and fetched_at are overwritten.
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
    fetched_at = EXCLUDED.fetched_at;

-- Upsert a daily/monthly energy summary
INSERT INTO growatt.energy_summaries (period_date, plant_id, time_unit, energy_kwh)
VALUES ($1, $2, $3, $4)
ON CONFLICT (period_date, plant_id, time_unit) DO UPDATE SET
    energy_kwh = EXCLUDED.energy_kwh,
    updated_at = now();

-- Upsert plant metadata
INSERT INTO growatt.plants (plant_id, plant_name, plant_type, country, city, latitude, longitude,
    peak_power_kw, formula_coal, formula_co2, formula_money, formula_tree,
    money_unit, money_unit_text, create_date, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (plant_id) DO UPDATE SET
    plant_name      = EXCLUDED.plant_name,
    plant_type      = EXCLUDED.plant_type,
    country         = EXCLUDED.country,
    city            = EXCLUDED.city,
    latitude        = EXCLUDED.latitude,
    longitude       = EXCLUDED.longitude,
    peak_power_kw   = EXCLUDED.peak_power_kw,
    formula_coal    = EXCLUDED.formula_coal,
    formula_co2     = EXCLUDED.formula_co2,
    formula_money   = EXCLUDED.formula_money,
    formula_tree    = EXCLUDED.formula_tree,
    money_unit      = EXCLUDED.money_unit,
    money_unit_text = EXCLUDED.money_unit_text,
    status          = EXCLUDED.status,
    updated_at      = now();

-- Upsert device metadata
INSERT INTO growatt.devices (device_sn, plant_id, device_type, device_name, model, status, last_update)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (device_sn) DO UPDATE SET
    plant_id    = EXCLUDED.plant_id,
    device_type = EXCLUDED.device_type,
    device_name = EXCLUDED.device_name,
    model       = EXCLUDED.model,
    status      = EXCLUDED.status,
    last_update = EXCLUDED.last_update,
    updated_at  = now();
```

---

## Materialized Views for Common Aggregations

### Hourly Power Aggregates

```sql
-- ============================================================
-- Hourly aggregates per device
-- Mirrors the hourly CSV output from growatt-export
-- ============================================================
CREATE MATERIALIZED VIEW growatt.mv_hourly_power AS
SELECT
    time_bucket('1 hour', ts)   AS hour_start,
    device_sn,
    COUNT(*)                    AS samples,
    MIN(pac_w)                  AS min_pac_w,
    MAX(pac_w)                  AS max_pac_w,
    AVG(pac_w)                  AS avg_pac_w,
    AVG(ppv_w)                  AS avg_ppv_w,
    AVG(vpv1_v)                 AS avg_vpv1_v,
    AVG(vpv2_v)                 AS avg_vpv2_v,
    AVG(ipv1_a)                 AS avg_ipv1_a,
    AVG(ipv2_a)                 AS avg_ipv2_a,
    AVG(vac1_v)                 AS avg_vac1_v,
    AVG(iac1_a)                 AS avg_iac1_a
FROM growatt.power_readings
GROUP BY time_bucket('1 hour', ts), device_sn
ORDER BY hour_start DESC, device_sn;

CREATE UNIQUE INDEX idx_mv_hourly_power
    ON growatt.mv_hourly_power (hour_start, device_sn);

COMMENT ON MATERIALIZED VIEW growatt.mv_hourly_power IS 'Hourly aggregates of 5-minute power readings. Refresh after each collection run.';
```

### Daily Production Summary

```sql
-- ============================================================
-- Daily production per device, computed from power readings
-- This is the source for device-level daily energy (as opposed
-- to energy_summaries which is plant-level).
-- ============================================================
CREATE MATERIALIZED VIEW growatt.mv_daily_production AS
SELECT
    time_bucket('1 day', ts)    AS production_date,
    device_sn,
    COUNT(*)                    AS total_samples,
    MIN(pac_w)                  AS min_pac_w,
    MAX(pac_w)                  AS peak_pac_w,
    AVG(pac_w)                  AS avg_pac_w,
    -- Estimate energy: each 5-min reading represents 5 minutes of production
    -- SUM(pac_w) * (5/60) / 1000 = kWh
    SUM(pac_w) * (5.0 / 60.0) / 1000.0 AS estimated_energy_kwh,
    MIN(ts)                     AS first_reading,
    MAX(ts)                     AS last_reading,
    -- Production hours: time span from first to last non-zero reading
    EXTRACT(EPOCH FROM (MAX(ts) - MIN(ts))) / 3600.0 AS production_hours
FROM growatt.power_readings
WHERE pac_w > 0
GROUP BY time_bucket('1 day', ts), device_sn
ORDER BY production_date DESC, device_sn;

CREATE UNIQUE INDEX idx_mv_daily_production
    ON growatt.mv_daily_production (production_date, device_sn);

COMMENT ON MATERIALIZED VIEW growatt.mv_daily_production IS 'Daily production summary per device. Energy is estimated from 5-minute AC power readings. For official plant-level energy totals use growatt.energy_summaries.';
```

### Monthly Production Summary

```sql
-- ============================================================
-- Monthly production summary from energy_summaries
-- ============================================================
CREATE MATERIALIZED VIEW growatt.mv_monthly_production AS
SELECT
    date_trunc('month', period_date)::DATE AS month,
    plant_id,
    SUM(energy_kwh)                        AS total_energy_kwh,
    AVG(energy_kwh)                        AS avg_daily_energy_kwh,
    MAX(energy_kwh)                        AS best_day_kwh,
    MIN(energy_kwh) FILTER (WHERE energy_kwh > 0) AS worst_producing_day_kwh,
    COUNT(*)                               AS days_with_data,
    COUNT(*) FILTER (WHERE energy_kwh > 0) AS producing_days
FROM growatt.energy_summaries
WHERE time_unit = 'day'
GROUP BY date_trunc('month', period_date), plant_id
ORDER BY month DESC, plant_id;

CREATE UNIQUE INDEX idx_mv_monthly_production
    ON growatt.mv_monthly_production (month, plant_id);

COMMENT ON MATERIALIZED VIEW growatt.mv_monthly_production IS 'Monthly production aggregated from daily energy summaries. Use this for billing and long-term trend analysis.';
```

### Latest Device Status View (non-materialized)

```sql
-- ============================================================
-- Latest status for each device (live query, not materialized)
-- ============================================================
CREATE VIEW growatt.v_device_latest AS
SELECT DISTINCT ON (d.device_sn)
    d.device_sn,
    d.device_name,
    d.model,
    d.plant_id,
    p.plant_name,
    ds.captured_at,
    ds.pac_w,
    ds.etoday_kwh,
    ds.etotal_kwh,
    ds.vpv1_v,
    ds.vpv2_v,
    ds.vac1_v,
    ds.fac_hz,
    ds.temperature_c,
    ds.status AS inverter_status
FROM growatt.devices d
JOIN growatt.plants p ON p.plant_id = d.plant_id
LEFT JOIN growatt.device_snapshots ds ON ds.device_sn = d.device_sn
ORDER BY d.device_sn, ds.captured_at DESC;

COMMENT ON VIEW growatt.v_device_latest IS 'Most recent snapshot for each device. Useful for dashboards and alerting.';
```

### Refresh Procedure

```sql
-- Refresh all materialized views (call after each collection cycle)
CREATE OR REPLACE PROCEDURE growatt.refresh_materialized_views()
LANGUAGE plpgsql AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY growatt.mv_hourly_power;
    REFRESH MATERIALIZED VIEW CONCURRENTLY growatt.mv_daily_production;
    REFRESH MATERIALIZED VIEW CONCURRENTLY growatt.mv_monthly_production;
END;
$$;

COMMENT ON PROCEDURE growatt.refresh_materialized_views IS 'Refreshes all materialized views concurrently. Safe to call while queries are running.';
```

---

## TimescaleDB Continuous Aggregates (Alternative to Materialized Views)

For production deployments, TimescaleDB continuous aggregates are preferred over standard materialized views because they support incremental refresh (only new data is re-aggregated).

```sql
-- ============================================================
-- Continuous aggregate: hourly power (auto-refreshing)
-- ============================================================
CREATE MATERIALIZED VIEW growatt.cagg_hourly_power
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', ts)   AS hour_start,
    device_sn,
    COUNT(*)                    AS samples,
    MIN(pac_w)                  AS min_pac_w,
    MAX(pac_w)                  AS max_pac_w,
    AVG(pac_w)                  AS avg_pac_w,
    AVG(ppv_w)                  AS avg_ppv_w,
    AVG(vac1_v)                 AS avg_vac1_v
FROM growatt.power_readings
GROUP BY time_bucket('1 hour', ts), device_sn;

SELECT add_continuous_aggregate_policy('growatt.cagg_hourly_power',
    start_offset    => INTERVAL '3 days',
    end_offset      => INTERVAL '1 hour',
    schedule_interval => INTERVAL '1 hour'
);

-- ============================================================
-- Continuous aggregate: daily power (auto-refreshing)
-- ============================================================
CREATE MATERIALIZED VIEW growatt.cagg_daily_power
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 day', ts)    AS day,
    device_sn,
    COUNT(*)                    AS samples,
    MIN(pac_w)                  AS min_pac_w,
    MAX(pac_w)                  AS peak_pac_w,
    AVG(pac_w)                  AS avg_pac_w,
    SUM(pac_w) * (5.0 / 60.0) / 1000.0 AS estimated_energy_kwh
FROM growatt.power_readings
GROUP BY time_bucket('1 day', ts), device_sn;

SELECT add_continuous_aggregate_policy('growatt.cagg_daily_power',
    start_offset    => INTERVAL '7 days',
    end_offset      => INTERVAL '1 day',
    schedule_interval => INTERVAL '1 day'
);
```

---

## Data Retention Policy

### Strategy

| Data tier | Retention | Rationale |
|---|---|---|
| **Raw 5-min readings** (`growatt.power_readings`) | 2 years | Sufficient for detailed analysis; compressed after 30 days |
| **Device snapshots** (`growatt.device_snapshots`) | 1 year | Real-time telemetry; value diminishes quickly |
| **Plant snapshots** (`growatt.plant_snapshots`) | 1 year | Same as device snapshots |
| **Energy summaries** (`growatt.energy_summaries`) | Indefinite | Small table, valuable for lifetime tracking |
| **Hourly continuous aggregate** (`growatt.cagg_hourly_power`) | 5 years | Compact, useful for long-term trends |
| **Daily continuous aggregate** (`growatt.cagg_daily_power`) | Indefinite | Tiny, keep forever |
| **Collection runs** (`growatt.collection_runs`) | 1 year | Operational metadata |
| **Collection gaps** (`growatt.collection_gaps`) | Indefinite | Small, useful for data quality audits |

### Retention Policy Statements

```sql
-- Drop raw 5-minute data older than 2 years
SELECT add_retention_policy('growatt.power_readings', INTERVAL '2 years');

-- Drop device snapshots older than 1 year
SELECT add_retention_policy('growatt.device_snapshots', INTERVAL '1 year');

-- Drop plant snapshots older than 1 year
SELECT add_retention_policy('growatt.plant_snapshots', INTERVAL '1 year');

-- Prune old collection_runs (not a hypertable, use a scheduled job or cron)
-- Example: DELETE FROM growatt.collection_runs WHERE started_at < now() - INTERVAL '1 year';
```

### Storage Estimates

For a single inverter reporting every 5 minutes during ~12 hours of daylight:

| Table | Rows/day | Row size (est.) | Daily bytes | Annual |
|---|---|---|---|---|
| `growatt.power_readings` | ~144 | ~100 B | ~14 KB | ~5 MB |
| `growatt.device_snapshots` | ~288 (every 5 min, 24h) | ~120 B | ~34 KB | ~12 MB |
| `growatt.energy_summaries` | 1 daily + ~0.03 monthly | ~60 B | ~60 B | ~22 KB |
| `growatt.cagg_hourly_power` | 24 | ~80 B | ~2 KB | ~700 KB |

With TimescaleDB compression (typically 10-20x on numeric time-series), raw 5-minute data for a single inverter over 2 years compresses to roughly **500 KB**. This schema comfortably handles hundreds of inverters on modest hardware.

---

## Mapping: Go Structs to Database Columns

### Plant -> growatt.plants

| Go Field (`Plant`) | JSON Key | DB Column | DB Type | Notes |
|---|---|---|---|---|
| `PlantID` | `plant_id` | `plant_id` | `TEXT` | PK |
| `PlantName` | `plant_name` | `plant_name` | `TEXT` | |
| `PlantType` | `plant_type` | `plant_type` | `INT` | |
| `Country` | `country` | `country` | `TEXT` | |
| `City` | `city` | `city` | `TEXT` | |
| `Latitude` | `latitude` | `latitude` | `DOUBLE PRECISION` | |
| `Longitude` | `longitude` | `longitude` | `DOUBLE PRECISION` | |
| `PeakPower` | `peak_power` | `peak_power_kw` | `DOUBLE PRECISION` | |
| `CurrentPower` | `current_power` | _(plant_snapshots)_ | | Real-time, stored in snapshots |
| `TodayEnergy` | `today_energy` | _(plant_snapshots)_ | | Real-time, stored in snapshots |
| `TotalEnergy` | `total_energy` | _(plant_snapshots)_ | | Real-time, stored in snapshots |
| `CreateDate` | `create_date` | `create_date` | `DATE` | |
| `Status` | `status` | `status` | `INT` | |
| `FormulaCoal` | `formula_coal` | `formula_coal` | `DOUBLE PRECISION` | |
| `FormulaCO2` | `formula_co2` | `formula_co2` | `DOUBLE PRECISION` | |
| `FormulaMoney` | `formula_money` | `formula_money` | `DOUBLE PRECISION` | |
| `FormulaTree` | `formula_tree` | `formula_tree` | `DOUBLE PRECISION` | |
| `MoneyUnit` | `money_unit` | `money_unit` | `TEXT` | |
| `MoneyUnitText` | `money_unit_text` | `money_unit_text` | `TEXT` | |

### MINHistoryDataPoint -> growatt.power_readings

| Go Field | JSON Key | DB Column | DB Type | Notes |
|---|---|---|---|---|
| `Time` | `time` | `ts` | `TIMESTAMPTZ` | Combined with date to form full timestamp |
| `Pac` | `pac` | `pac_w` | `DOUBLE PRECISION` | AC output power (W) |
| `Ppv` | `ppv` | `ppv_w` | `DOUBLE PRECISION` | PV input power (W) |
| `Vpv1` | `vpv1` | `vpv1_v` | `DOUBLE PRECISION` | PV string 1 voltage (V) |
| `Vpv2` | `vpv2` | `vpv2_v` | `DOUBLE PRECISION` | PV string 2 voltage (V) |
| `Ipv1` | `ipv1` | `ipv1_a` | `DOUBLE PRECISION` | PV string 1 current (A) |
| `Ipv2` | `ipv2` | `ipv2_a` | `DOUBLE PRECISION` | PV string 2 current (A) |
| `Vac1` | `vac1` | `vac1_v` | `DOUBLE PRECISION` | Grid AC voltage (V) |
| `Iac1` | `iac1` | `iac1_a` | `DOUBLE PRECISION` | Grid AC current (A) |
| -- | -- | `fetched_at` | `TIMESTAMPTZ` | Set to `now()` at insert time; auto-updated on upsert |

### MINInverterData -> growatt.device_snapshots

| Go Field | JSON Key | DB Column | DB Type | Notes |
|---|---|---|---|---|
| `Serial` | `tlx_sn` | `device_sn` | `TEXT` | FK to devices |
| `Status` | `status` | `status` | `INT` | |
| `Pac` | `pac` | `pac_w` | `DOUBLE PRECISION` | |
| `Etoday` | `etoday` | `etoday_kwh` | `DOUBLE PRECISION` | |
| `Etotal` | `etotal` | `etotal_kwh` | `DOUBLE PRECISION` | |
| `Vpv1` | `vpv1` | `vpv1_v` | `DOUBLE PRECISION` | |
| `Vpv2` | `vpv2` | `vpv2_v` | `DOUBLE PRECISION` | |
| `Ipv1` | `ipv1` | `ipv1_a` | `DOUBLE PRECISION` | |
| `Ipv2` | `ipv2` | `ipv2_a` | `DOUBLE PRECISION` | |
| `Vac1` | `vac1` | `vac1_v` | `DOUBLE PRECISION` | |
| `Iac1` | `iac1` | `iac1_a` | `DOUBLE PRECISION` | |
| `Fac` | `fac` | `fac_hz` | `DOUBLE PRECISION` | |
| `Temperature` | `temperature` | `temperature_c` | `DOUBLE PRECISION` | |

---

## Example Queries

### Current production for all devices

```sql
SELECT * FROM growatt.v_device_latest;
```

### Power curve for a specific day

```sql
SELECT ts, pac_w, ppv_w, vpv1_v, vpv2_v
FROM growatt.power_readings
WHERE device_sn = 'ABC123'
  AND ts >= '2026-02-15'::DATE
  AND ts <  '2026-02-16'::DATE
ORDER BY ts;
```

### Hourly averages for the past week

```sql
SELECT hour_start, avg_pac_w, samples
FROM growatt.cagg_hourly_power
WHERE device_sn = 'ABC123'
  AND hour_start >= now() - INTERVAL '7 days'
ORDER BY hour_start;
```

### Best production days this month

```sql
SELECT
    production_date,
    peak_pac_w,
    estimated_energy_kwh,
    production_hours
FROM growatt.mv_daily_production
WHERE device_sn = 'ABC123'
  AND production_date >= date_trunc('month', CURRENT_DATE)
ORDER BY estimated_energy_kwh DESC
LIMIT 10;
```

### Detect missing collection days

```sql
SELECT d.device_sn, gs.gap_date
FROM growatt.devices d
CROSS JOIN generate_series(
    CURRENT_DATE - INTERVAL '30 days',
    CURRENT_DATE - INTERVAL '1 day',
    '1 day'::INTERVAL
) AS gs(gap_date)
LEFT JOIN growatt.power_readings pr
    ON pr.device_sn = d.device_sn
   AND pr.ts >= gs.gap_date
   AND pr.ts <  gs.gap_date + INTERVAL '1 day'
WHERE pr.ts IS NULL
ORDER BY d.device_sn, gs.gap_date;
```

### Year-over-year monthly comparison

```sql
SELECT
    EXTRACT(MONTH FROM month) AS month_num,
    EXTRACT(YEAR FROM month)  AS year,
    total_energy_kwh,
    producing_days
FROM growatt.mv_monthly_production
WHERE plant_id = '12345'
ORDER BY month_num, year;
```

---

## Testing

This section describes how to verify the schema works correctly end-to-end.

### 1. Set Up a Test Database

Start a disposable TimescaleDB container:

```bash
docker run -d --name growatt-test \
  -p 5432:5432 \
  -e POSTGRES_PASSWORD=testpass \
  -e POSTGRES_DB=growatt_test \
  timescale/timescaledb:latest-pg16

# Wait for it to be ready
sleep 3
export PGURL="postgres://postgres:testpass@localhost:5432/growatt_test"
```

### 2. Run All CREATE TABLE Statements

Copy every SQL block from this document (Extension Setup through Retention Policy Statements) into a single file and execute it:

```bash
psql "$PGURL" -f schema.sql
```

Expected output: no `ERROR` lines. You will see `CREATE TABLE`, `CREATE INDEX`, `NOTICE` lines from TimescaleDB hypertable creation, and `SELECT 1` from policy statements. Any error here indicates a schema bug.

Alternatively, run each section one at a time to isolate failures:

```bash
psql "$PGURL" -c "CREATE EXTENSION IF NOT EXISTS timescaledb;"
psql "$PGURL" -c "CREATE EXTENSION IF NOT EXISTS pg_trgm;"
psql "$PGURL" -c "CREATE SCHEMA IF NOT EXISTS growatt;"
# ... then each CREATE TABLE block
```

### 3. Sample INSERT Statements with Realistic Data

These values come from real CSV exports captured on Feb 14, 2026.

```sql
-- Set up dimension data first
INSERT INTO growatt.plants (plant_id, plant_name, plant_type, country, city, latitude, longitude, peak_power_kw, status)
VALUES ('PLANT001', 'Home Solar', 1, 'US', 'Austin', 30.2672, -97.7431, 7.6, 1);

INSERT INTO growatt.devices (device_sn, plant_id, device_type, device_name, model, status)
VALUES ('DEVICE001', 'PLANT001', 4, 'Roof Inverter', 'MIN 6000TL-XH', 1);

-- Insert 5-minute readings from Feb 14 (real data points)
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a)
VALUES
    ('2026-02-14 12:51:00-06', 'DEVICE001', 2729.5, 2810.0, 312.4, 310.1, 4.5, 4.6, 241.2, 11.3),
    ('2026-02-14 12:56:00-06', 'DEVICE001', 5223.3, 5380.0, 315.7, 313.2, 8.5, 8.7, 240.8, 21.7);

-- Insert a plant-level energy summary
INSERT INTO growatt.energy_summaries (period_date, plant_id, time_unit, energy_kwh)
VALUES ('2026-02-14', 'PLANT001', 'day', 38.7);

-- Insert a device snapshot
INSERT INTO growatt.device_snapshots (device_sn, pac_w, etoday_kwh, etotal_kwh, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fac_hz, temperature_c, status)
VALUES ('DEVICE001', 5223.3, 28.4, 12450.0, 315.7, 313.2, 8.5, 8.7, 240.8, 21.7, 60.01, 42.5, 1);

-- Insert a plant snapshot
INSERT INTO growatt.plant_snapshots (plant_id, current_power_w, today_energy_kwh, month_energy_kwh, year_energy_kwh, total_energy_kwh, peak_power_today_kw)
VALUES ('PLANT001', 5223.3, 28.4, 410.2, 1250.0, 12450.0, 5.9);

-- Insert a collection run record
INSERT INTO growatt.collection_runs (source_type, source_id, query_date_start, query_date_end, points_collected, status)
VALUES ('device_history', 'DEVICE001', '2026-02-14', '2026-02-14', 144, 'success');
```

Verify the inserts:

```sql
SELECT ts, device_sn, pac_w, fetched_at FROM growatt.power_readings ORDER BY ts;
-- Expected: 2 rows, fetched_at should be approximately now()
```

### 4. Verification Queries

#### Confirm hypertables were created

```sql
SELECT hypertable_name, num_chunks
FROM timescaledb_information.hypertables
WHERE hypertable_schema = 'growatt'
ORDER BY hypertable_name;
-- Expected: power_readings, plant_snapshots, device_snapshots
```

#### Confirm indexes exist

```sql
SELECT indexname, tablename
FROM pg_indexes
WHERE schemaname = 'growatt'
ORDER BY tablename, indexname;
-- Expected: idx_power_readings_device_ts, idx_plant_snapshots_plant_time,
--           idx_device_snapshots_device_time, idx_energy_summaries_plant_unit_date,
--           idx_devices_plant_id, idx_plants_status, plus PK indexes
```

#### Confirm compression policies are set

```sql
SELECT hypertable_name, compress_segmentby, compress_orderby
FROM timescaledb_information.compression_settings
WHERE hypertable_schema = 'growatt';
-- Expected: 3 rows (power_readings, device_snapshots, plant_snapshots)
```

#### Confirm retention policies are set

```sql
SELECT application_name, schedule_interval, config
FROM timescaledb_information.jobs
WHERE application_name LIKE '%retention%'
ORDER BY application_name;
-- Expected: retention policies for power_readings (2 years),
--           device_snapshots (1 year), plant_snapshots (1 year)
```

### 5. Upsert Test

Verify that re-inserting the same `(ts, device_sn)` overwrites the old values:

```sql
-- Original row inserted above: pac_w = 2729.5 at 12:51
SELECT pac_w, fetched_at AS original_fetched_at
FROM growatt.power_readings
WHERE ts = '2026-02-14 12:51:00-06' AND device_sn = 'DEVICE001';
-- Expected: pac_w = 2729.5

-- Upsert with corrected value (simulating a re-fetch with updated data)
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
VALUES ('2026-02-14 12:51:00-06', 'DEVICE001', 2750.0, 2830.0, 312.4, 310.1, 4.5, 4.6, 241.2, 11.3, now())
ON CONFLICT (ts, device_sn) DO UPDATE SET
    pac_w      = EXCLUDED.pac_w,
    ppv_w      = EXCLUDED.ppv_w,
    vpv1_v     = EXCLUDED.vpv1_v,
    vpv2_v     = EXCLUDED.vpv2_v,
    ipv1_a     = EXCLUDED.ipv1_a,
    ipv2_a     = EXCLUDED.ipv2_a,
    vac1_v     = EXCLUDED.vac1_v,
    iac1_a     = EXCLUDED.iac1_a,
    fetched_at = EXCLUDED.fetched_at;

-- Verify the upsert overwrote the old value
SELECT pac_w, fetched_at AS updated_fetched_at
FROM growatt.power_readings
WHERE ts = '2026-02-14 12:51:00-06' AND device_sn = 'DEVICE001';
-- Expected: pac_w = 2750.0, fetched_at > original_fetched_at

-- Confirm still only 2 rows total (no duplicates)
SELECT COUNT(*) FROM growatt.power_readings;
-- Expected: 2
```

### 6. Materialized View Refresh Test

```sql
-- Refresh the materialized views
CALL growatt.refresh_materialized_views();

-- Verify hourly aggregation
SELECT hour_start, device_sn, samples, avg_pac_w, min_pac_w, max_pac_w
FROM growatt.mv_hourly_power;
-- Expected: 1 row for the 12:00 hour bucket
--   samples = 2 (the 12:51 and 12:56 readings)
--   min_pac_w = 2750.0 (updated value), max_pac_w = 5223.3
--   avg_pac_w = (2750.0 + 5223.3) / 2 = 3986.65

-- Verify daily production
SELECT production_date, device_sn, total_samples, peak_pac_w, estimated_energy_kwh
FROM growatt.mv_daily_production;
-- Expected: 1 row for 2026-02-14
--   total_samples = 2, peak_pac_w = 5223.3
--   estimated_energy_kwh = (2750.0 + 5223.3) * (5.0/60.0) / 1000.0 = ~0.664

-- Verify monthly production (from energy_summaries)
SELECT month, plant_id, total_energy_kwh, days_with_data
FROM growatt.mv_monthly_production;
-- Expected: 1 row for 2026-02-01, total_energy_kwh = 38.7, days_with_data = 1
```

### 7. Retention Policy Verification

Retention policies are background jobs managed by TimescaleDB. To verify they are registered:

```sql
SELECT j.job_id, j.application_name, j.schedule_interval,
       c.hypertable_name, c.config
FROM timescaledb_information.jobs j
JOIN timescaledb_information.job_stats js ON j.job_id = js.job_id
LEFT JOIN (
    SELECT job_id,
           config->>'hypertable_name' AS hypertable_name,
           config
    FROM timescaledb_information.jobs
) c ON c.job_id = j.job_id
WHERE j.application_name LIKE '%retention%';
-- Expected: 3 retention jobs
--   power_readings:    drop_after = '2 years'
--   device_snapshots:  drop_after = '1 year'
--   plant_snapshots:   drop_after = '1 year'
```

To functionally test retention (advanced), you can manually run a retention job:

```sql
-- Insert old data that should be dropped
INSERT INTO growatt.power_readings (ts, device_sn, pac_w)
VALUES ('2020-01-01 12:00:00+00', 'DEVICE001', 100.0);

-- Manually trigger the retention policy (find the job_id from query above)
-- CALL run_job(<job_id>);

-- Verify old data was removed
SELECT COUNT(*) FROM growatt.power_readings WHERE ts < '2021-01-01';
-- Expected: 0
```

### 8. Edge Cases

#### NULL handling for optional columns

```sql
-- Plant-level PowerDataPoint only has pac_w; all other columns are NULL
INSERT INTO growatt.power_readings (ts, device_sn, pac_w)
VALUES ('2026-02-14 13:01:00-06', 'DEVICE001', 4500.0);

SELECT ts, pac_w, ppv_w, vpv1_v
FROM growatt.power_readings
WHERE ts = '2026-02-14 13:01:00-06' AND device_sn = 'DEVICE001';
-- Expected: pac_w = 4500.0, ppv_w = NULL, vpv1_v = NULL
-- fetched_at should still be populated (DEFAULT now())
```

#### Zero-power nighttime readings

```sql
-- Inverters report 0W readings during night hours
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a)
VALUES
    ('2026-02-14 22:00:00-06', 'DEVICE001', 0, 0, 0, 0, 0, 0, 240.5, 0),
    ('2026-02-14 22:05:00-06', 'DEVICE001', 0, 0, 0, 0, 0, 0, 240.3, 0);

-- Verify zero readings are stored correctly
SELECT ts, pac_w, vac1_v FROM growatt.power_readings
WHERE device_sn = 'DEVICE001' AND pac_w = 0
ORDER BY ts;
-- Expected: 2 rows with pac_w = 0, vac1_v still has grid voltage

-- Confirm mv_daily_production excludes zero-power readings (WHERE pac_w > 0)
CALL growatt.refresh_materialized_views();

SELECT total_samples FROM growatt.mv_daily_production
WHERE production_date = '2026-02-14' AND device_sn = 'DEVICE001';
-- Expected: total_samples does NOT include the 2 nighttime rows
```

#### Timezone handling

```sql
-- Insert the same physical moment expressed in different timezones
-- These should resolve to the same instant and conflict on upsert
INSERT INTO growatt.power_readings (ts, device_sn, pac_w, fetched_at)
VALUES ('2026-02-14 18:51:00+00', 'DEVICE001', 999.9, now())
ON CONFLICT (ts, device_sn) DO UPDATE SET
    pac_w = EXCLUDED.pac_w,
    fetched_at = EXCLUDED.fetched_at;

-- '2026-02-14 18:51:00+00' is the same instant as '2026-02-14 12:51:00-06'
-- so the upsert should have updated the existing row, not created a new one
SELECT COUNT(*) FROM growatt.power_readings
WHERE ts = '2026-02-14 12:51:00-06' AND device_sn = 'DEVICE001';
-- Expected: 1 (not 2)

SELECT pac_w FROM growatt.power_readings
WHERE ts = '2026-02-14 12:51:00-06' AND device_sn = 'DEVICE001';
-- Expected: 999.9 (the UTC insert overwrote the earlier value)
```

### Cleanup

```bash
docker stop growatt-test && docker rm growatt-test
```
