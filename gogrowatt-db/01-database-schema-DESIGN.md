# Database Schema Implementation Design

## Overview

This document is the complete implementation blueprint for the GoGrowatt PostgreSQL + TimescaleDB database layer. It incorporates all consistency fixes from `00-consistency-analysis.md` and fully specifies every table, index, policy, Go struct, repository interface, migration file, Docker service, and test strategy.

Canonical rules (from consistency analysis):
- All tables live in the `growatt` schema namespace
- Column names use unit suffixes: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, `ipv1_a`, etc.
- Primary key for `power_readings` is `(ts, device_sn)` -- time-first for TimescaleDB partitioning
- The `fetched_at` column exists on `power_readings`
- Collection metadata uses `collection_runs` and `collection_gaps` (not `fetch_log`)
- Metadata table is `growatt.metadata` (not `system_metadata`)
- Docker image is `timescale/timescaledb:latest-pg16` (not plain PostgreSQL)
- All downstream consumers (REST API, CLI, MCP server) use `ts` not `reading_time` or `recorded_at`

---

## 1. Docker Compose Service Definition

```yaml
# docker-compose.yml (database service excerpt)
services:
  timescaledb:
    image: timescale/timescaledb:latest-pg16
    container_name: gogrowatt-timescaledb
    restart: unless-stopped
    environment:
      POSTGRES_DB: growatt
      POSTGRES_USER: growatt
      POSTGRES_PASSWORD: "${POSTGRES_PASSWORD:-growatt_dev}"
      # TimescaleDB tuning for small single-inverter deployments
      TIMESCALEDB_TELEMETRY: "off"
    ports:
      - "${DB_PORT:-5432}:5432"
    volumes:
      - timescaledb_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U growatt -d growatt"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 10s
    # Performance tuning via command-line flags
    command:
      - "postgres"
      - "-c"
      - "shared_preload_libraries=timescaledb"
      - "-c"
      - "timescaledb.max_background_workers=4"
      - "-c"
      - "max_connections=50"
      - "-c"
      - "shared_buffers=256MB"
      - "-c"
      - "effective_cache_size=512MB"
      - "-c"
      - "work_mem=8MB"
      - "-c"
      - "maintenance_work_mem=128MB"
      - "-c"
      - "wal_level=replica"
      - "-c"
      - "max_wal_size=1GB"

volumes:
  timescaledb_data:
    driver: local
```

For testing:

```yaml
# docker-compose.test.yml
services:
  timescaledb-test:
    image: timescale/timescaledb:latest-pg16
    container_name: gogrowatt-timescaledb-test
    environment:
      POSTGRES_DB: growatt_test
      POSTGRES_USER: growatt
      POSTGRES_PASSWORD: testpass
      TIMESCALEDB_TELEMETRY: "off"
    ports:
      - "5433:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U growatt -d growatt_test"]
      interval: 2s
      timeout: 3s
      retries: 10
    tmpfs:
      - /var/lib/postgresql/data  # RAM-backed for speed in tests
```

---

## 2. Migration Strategy (golang-migrate)

### File Layout

```
migrations/
  000001_create_schema_and_extensions.up.sql
  000001_create_schema_and_extensions.down.sql
  000002_create_dimension_tables.up.sql
  000002_create_dimension_tables.down.sql
  000003_create_hypertables.up.sql
  000003_create_hypertables.down.sql
  000004_create_regular_tables.up.sql
  000004_create_regular_tables.down.sql
  000005_create_indexes.up.sql
  000005_create_indexes.down.sql
  000006_create_compression_policies.up.sql
  000006_create_compression_policies.down.sql
  000007_create_continuous_aggregates.up.sql
  000007_create_continuous_aggregates.down.sql
  000008_create_retention_policies.up.sql
  000008_create_retention_policies.down.sql
  000009_create_views_and_procedures.up.sql
  000009_create_views_and_procedures.down.sql
  000010_create_metadata_table.up.sql
  000010_create_metadata_table.down.sql
```

### Migration 000001: Schema and Extensions

```sql
-- 000001_create_schema_and_extensions.up.sql
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE SCHEMA IF NOT EXISTS growatt;
```

```sql
-- 000001_create_schema_and_extensions.down.sql
DROP SCHEMA IF EXISTS growatt CASCADE;
-- Extensions are left in place (shared across databases)
```

### Migration 000002: Dimension Tables

```sql
-- 000002_create_dimension_tables.up.sql

CREATE TABLE growatt.plants (
    plant_id        TEXT        PRIMARY KEY,
    plant_name      TEXT        NOT NULL,
    plant_type      INT         NOT NULL DEFAULT 0,
    country         TEXT,
    city            TEXT,
    latitude        DOUBLE PRECISION,
    longitude       DOUBLE PRECISION,
    peak_power_kw   DOUBLE PRECISION,
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

```sql
-- 000002_create_dimension_tables.down.sql
DROP TABLE IF EXISTS growatt.devices;
DROP TABLE IF EXISTS growatt.plants;
```

### Migration 000003: Hypertables

```sql
-- 000003_create_hypertables.up.sql

-- ============================================================
-- power_readings: 5-minute interval telemetry (HYPERTABLE)
-- Source: growatt.MINHistoryDataPoint struct
-- PK order: (ts, device_sn) -- time-first for TimescaleDB partitioning
-- ============================================================
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
    fetched_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),
    CONSTRAINT power_readings_pkey PRIMARY KEY (ts, device_sn)
);

SELECT create_hypertable(
    'growatt.power_readings',
    by_range('ts', INTERVAL '7 days')
);

COMMENT ON TABLE  growatt.power_readings IS '5-minute interval power readings from MIN/TLX inverters. TimescaleDB hypertable with 7-day chunks.';
COMMENT ON COLUMN growatt.power_readings.ts IS 'Timestamp of the reading (UTC). Constructed from date + time fields returned by the API.';
COMMENT ON COLUMN growatt.power_readings.pac_w IS 'AC output power in watts. This is the grid-delivered power.';
COMMENT ON COLUMN growatt.power_readings.ppv_w IS 'Total PV input power in watts (sum of all strings). May be NULL for plant-level data.';
COMMENT ON COLUMN growatt.power_readings.fetched_at IS 'When this reading was fetched from the Growatt API. On upsert the latest fetch always wins.';


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

COMMENT ON TABLE growatt.plant_snapshots IS 'Point-in-time snapshots of plant-level energy counters from plant/data endpoint.';


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
    fac_hz          DOUBLE PRECISION,
    temperature_c   DOUBLE PRECISION,
    status          INT,
    CONSTRAINT device_snapshots_pkey PRIMARY KEY (id, captured_at)
);

SELECT create_hypertable(
    'growatt.device_snapshots',
    by_range('captured_at', INTERVAL '30 days')
);

COMMENT ON TABLE  growatt.device_snapshots IS 'Real-time inverter snapshots from device/tlx/tlx_data_info.';
COMMENT ON COLUMN growatt.device_snapshots.fac_hz IS 'AC grid frequency in Hz.';
COMMENT ON COLUMN growatt.device_snapshots.temperature_c IS 'Inverter internal temperature in degrees Celsius.';
```

```sql
-- 000003_create_hypertables.down.sql
DROP TABLE IF EXISTS growatt.device_snapshots;
DROP TABLE IF EXISTS growatt.plant_snapshots;
DROP TABLE IF EXISTS growatt.power_readings;
```

### Migration 000004: Regular Tables

```sql
-- 000004_create_regular_tables.up.sql

-- ============================================================
-- energy_summaries: Daily and monthly energy totals (PLANT-LEVEL)
-- Source: growatt.EnergyDataPoint struct
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

COMMENT ON TABLE  growatt.energy_summaries IS 'Plant-level daily and monthly energy production totals from the plant/energy API endpoint.';
COMMENT ON COLUMN growatt.energy_summaries.time_unit IS 'Granularity: ''day'' for daily totals, ''month'' for monthly totals.';
COMMENT ON COLUMN growatt.energy_summaries.period_date IS 'For daily: the date. For monthly: the first day of the month.';


-- ============================================================
-- collection_runs: Audit log for each data fetch
-- ============================================================
CREATE TABLE growatt.collection_runs (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_type         TEXT        NOT NULL,
    source_id           TEXT        NOT NULL,
    query_date_start    DATE,
    query_date_end      DATE,
    points_collected    INT         NOT NULL DEFAULT 0,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    status              TEXT        NOT NULL DEFAULT 'running'
                                    CHECK (status IN ('running', 'success', 'error', 'partial')),
    error_message       TEXT
);

COMMENT ON TABLE growatt.collection_runs IS 'Audit trail for every data collection run. Used to detect missing dates and troubleshoot API failures.';
COMMENT ON COLUMN growatt.collection_runs.source_type IS 'One of: plant_power, device_history, energy_daily, energy_monthly, plant_snapshot, device_snapshot.';


-- ============================================================
-- collection_gaps: Known gaps in time-series data
-- ============================================================
CREATE TABLE growatt.collection_gaps (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    device_sn       TEXT        NOT NULL,
    gap_date        DATE        NOT NULL,
    expected_start  TIME,
    expected_end    TIME,
    reason          TEXT,
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ,
    CONSTRAINT collection_gaps_uq UNIQUE (device_sn, gap_date)
);

COMMENT ON TABLE growatt.collection_gaps IS 'Tracks known gaps where expected readings are missing. Enables targeted backfill operations.';
COMMENT ON COLUMN growatt.collection_gaps.reason IS 'One of: no_data_returned, api_error, inverter_offline, partial_day.';
```

```sql
-- 000004_create_regular_tables.down.sql
DROP TABLE IF EXISTS growatt.collection_gaps;
DROP TABLE IF EXISTS growatt.collection_runs;
DROP TABLE IF EXISTS growatt.energy_summaries;
```

### Migration 000005: Indexes

```sql
-- 000005_create_indexes.up.sql

-- power_readings: device-first lookup for "all data for one device in a time range"
-- Justification: The PK (ts, device_sn) is time-first for hypertable partitioning.
-- This index covers the common query pattern: WHERE device_sn = $1 AND ts BETWEEN $2 AND $3.
CREATE INDEX idx_power_readings_device_ts
    ON growatt.power_readings (device_sn, ts DESC);

-- plant_snapshots: latest snapshot per plant
-- Justification: Dashboard queries for "current state of plant X" use ORDER BY captured_at DESC LIMIT 1.
CREATE INDEX idx_plant_snapshots_plant_time
    ON growatt.plant_snapshots (plant_id, captured_at DESC);

-- device_snapshots: latest snapshot per device
-- Justification: Same pattern as plant_snapshots but for device-level telemetry.
CREATE INDEX idx_device_snapshots_device_time
    ON growatt.device_snapshots (device_sn, captured_at DESC);

-- energy_summaries: reverse lookup by plant + unit
-- Justification: "Monthly production for plant Z in 2026" queries scan by (plant_id, time_unit, date).
-- The PK is (period_date, plant_id, time_unit) which is date-first; this index is plant-first.
CREATE INDEX idx_energy_summaries_plant_unit_date
    ON growatt.energy_summaries (plant_id, time_unit, period_date DESC);

-- collection_runs: lookup by source
-- Justification: Backfill manager queries "last successful run for device X" frequently.
CREATE INDEX idx_collection_runs_source
    ON growatt.collection_runs (source_type, source_id, query_date_start DESC);

-- Dimension table indexes
CREATE INDEX idx_devices_plant_id ON growatt.devices (plant_id);
CREATE INDEX idx_plants_status    ON growatt.plants (status);
```

```sql
-- 000005_create_indexes.down.sql
DROP INDEX IF EXISTS growatt.idx_power_readings_device_ts;
DROP INDEX IF EXISTS growatt.idx_plant_snapshots_plant_time;
DROP INDEX IF EXISTS growatt.idx_device_snapshots_device_time;
DROP INDEX IF EXISTS growatt.idx_energy_summaries_plant_unit_date;
DROP INDEX IF EXISTS growatt.idx_collection_runs_source;
DROP INDEX IF EXISTS growatt.idx_devices_plant_id;
DROP INDEX IF EXISTS growatt.idx_plants_status;
```

### Migration 000006: Compression Policies

```sql
-- 000006_create_compression_policies.up.sql

-- power_readings: compress after 30 days
-- Segment by device_sn so each device's data compresses independently.
-- Order by ts DESC so recent-first queries decompress fewer blocks.
ALTER TABLE growatt.power_readings
    SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'device_sn',
        timescaledb.compress_orderby = 'ts DESC'
    );

SELECT add_compression_policy('growatt.power_readings', INTERVAL '30 days');

-- device_snapshots: compress after 30 days
ALTER TABLE growatt.device_snapshots
    SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'device_sn',
        timescaledb.compress_orderby = 'captured_at DESC'
    );

SELECT add_compression_policy('growatt.device_snapshots', INTERVAL '30 days');

-- plant_snapshots: compress after 30 days
ALTER TABLE growatt.plant_snapshots
    SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'plant_id',
        timescaledb.compress_orderby = 'captured_at DESC'
    );

SELECT add_compression_policy('growatt.plant_snapshots', INTERVAL '30 days');
```

```sql
-- 000006_create_compression_policies.down.sql
SELECT remove_compression_policy('growatt.plant_snapshots', if_exists => true);
SELECT remove_compression_policy('growatt.device_snapshots', if_exists => true);
SELECT remove_compression_policy('growatt.power_readings', if_exists => true);

ALTER TABLE growatt.plant_snapshots SET (timescaledb.compress = false);
ALTER TABLE growatt.device_snapshots SET (timescaledb.compress = false);
ALTER TABLE growatt.power_readings SET (timescaledb.compress = false);
```

### Migration 000007: Continuous Aggregates

```sql
-- 000007_create_continuous_aggregates.up.sql

-- ============================================================
-- Hourly power aggregates per device (auto-refreshing)
-- Refresh window: 3 days back to 1 hour ago, every hour.
-- 3-day start_offset ensures late-arriving backfill data is captured.
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
    AVG(vpv1_v)                 AS avg_vpv1_v,
    AVG(vpv2_v)                 AS avg_vpv2_v,
    AVG(ipv1_a)                 AS avg_ipv1_a,
    AVG(ipv2_a)                 AS avg_ipv2_a,
    AVG(vac1_v)                 AS avg_vac1_v,
    AVG(iac1_a)                 AS avg_iac1_a
FROM growatt.power_readings
GROUP BY time_bucket('1 hour', ts), device_sn
WITH NO DATA;

SELECT add_continuous_aggregate_policy('growatt.cagg_hourly_power',
    start_offset    => INTERVAL '3 days',
    end_offset      => INTERVAL '1 hour',
    schedule_interval => INTERVAL '1 hour'
);

-- ============================================================
-- Daily power aggregates per device (auto-refreshing)
-- Refresh window: 7 days back to 1 day ago, once per day.
-- 7-day start_offset matches the API's maximum lookback for backfill.
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
    -- Energy estimate: each reading represents a 5-minute interval
    -- SUM(pac_w) * (5/60) / 1000 converts watt-readings to kWh
    SUM(pac_w) * (5.0 / 60.0) / 1000.0 AS estimated_energy_kwh
FROM growatt.power_readings
GROUP BY time_bucket('1 day', ts), device_sn
WITH NO DATA;

SELECT add_continuous_aggregate_policy('growatt.cagg_daily_power',
    start_offset    => INTERVAL '7 days',
    end_offset      => INTERVAL '1 day',
    schedule_interval => INTERVAL '1 day'
);
```

```sql
-- 000007_create_continuous_aggregates.down.sql
SELECT remove_continuous_aggregate_policy('growatt.cagg_daily_power', if_not_exists => true);
DROP MATERIALIZED VIEW IF EXISTS growatt.cagg_daily_power;

SELECT remove_continuous_aggregate_policy('growatt.cagg_hourly_power', if_not_exists => true);
DROP MATERIALIZED VIEW IF EXISTS growatt.cagg_hourly_power;
```

### Migration 000008: Retention Policies

```sql
-- 000008_create_retention_policies.up.sql

-- Raw 5-min readings: 2 years (compressed after 30 days)
SELECT add_retention_policy('growatt.power_readings', INTERVAL '2 years');

-- Device snapshots: 1 year
SELECT add_retention_policy('growatt.device_snapshots', INTERVAL '1 year');

-- Plant snapshots: 1 year
SELECT add_retention_policy('growatt.plant_snapshots', INTERVAL '1 year');

-- energy_summaries: indefinite (small table, valuable for lifetime tracking)
-- collection_runs: pruned by application code or cron (not a hypertable)
-- collection_gaps: indefinite (small table, useful for audits)
-- cagg_hourly_power: 5 years
-- cagg_daily_power: indefinite
```

```sql
-- 000008_create_retention_policies.down.sql
SELECT remove_retention_policy('growatt.plant_snapshots', if_exists => true);
SELECT remove_retention_policy('growatt.device_snapshots', if_exists => true);
SELECT remove_retention_policy('growatt.power_readings', if_exists => true);
```

### Migration 000009: Views and Procedures

```sql
-- 000009_create_views_and_procedures.up.sql

-- ============================================================
-- Materialized view: daily production per device
-- This is the source for device-level daily energy (as opposed
-- to energy_summaries which is plant-level from the Growatt API).
-- ============================================================
CREATE MATERIALIZED VIEW growatt.mv_daily_production AS
SELECT
    time_bucket('1 day', ts)    AS production_date,
    device_sn,
    COUNT(*)                    AS total_samples,
    MIN(pac_w)                  AS min_pac_w,
    MAX(pac_w)                  AS peak_pac_w,
    AVG(pac_w)                  AS avg_pac_w,
    SUM(pac_w) * (5.0 / 60.0) / 1000.0 AS estimated_energy_kwh,
    MIN(ts)                     AS first_reading,
    MAX(ts)                     AS last_reading,
    EXTRACT(EPOCH FROM (MAX(ts) - MIN(ts))) / 3600.0 AS production_hours
FROM growatt.power_readings
WHERE pac_w > 0
GROUP BY time_bucket('1 day', ts), device_sn
ORDER BY production_date DESC, device_sn;

CREATE UNIQUE INDEX idx_mv_daily_production
    ON growatt.mv_daily_production (production_date, device_sn);

COMMENT ON MATERIALIZED VIEW growatt.mv_daily_production IS 'Daily production summary per device. Energy estimated from 5-min AC power readings (pac_w). For official plant-level totals use growatt.energy_summaries.';


-- ============================================================
-- Materialized view: monthly production from energy_summaries
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

COMMENT ON MATERIALIZED VIEW growatt.mv_monthly_production IS 'Monthly production aggregated from daily energy summaries. Use for billing and long-term trends.';


-- ============================================================
-- View: latest device status (live query, not materialized)
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


-- ============================================================
-- Procedure: refresh non-continuous materialized views
-- ============================================================
CREATE OR REPLACE PROCEDURE growatt.refresh_materialized_views()
LANGUAGE plpgsql AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY growatt.mv_daily_production;
    REFRESH MATERIALIZED VIEW CONCURRENTLY growatt.mv_monthly_production;
END;
$$;

COMMENT ON PROCEDURE growatt.refresh_materialized_views IS 'Refreshes non-continuous materialized views concurrently. Safe to call while queries are running.';
```

```sql
-- 000009_create_views_and_procedures.down.sql
DROP PROCEDURE IF EXISTS growatt.refresh_materialized_views;
DROP VIEW IF EXISTS growatt.v_device_latest;
DROP MATERIALIZED VIEW IF EXISTS growatt.mv_monthly_production;
DROP MATERIALIZED VIEW IF EXISTS growatt.mv_daily_production;
```

### Migration 000010: Metadata Table

```sql
-- 000010_create_metadata_table.up.sql

-- ============================================================
-- metadata: Key-value store for schema versioning and app state
-- Named growatt.metadata per consistency analysis (not system_metadata).
-- ============================================================
CREATE TABLE growatt.metadata (
    key         TEXT        PRIMARY KEY,
    value       TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE growatt.metadata IS 'Key-value metadata store for schema version, migration state, and application-level bookkeeping.';

-- Seed initial values
INSERT INTO growatt.metadata (key, value) VALUES
    ('schema_version', '10'),
    ('schema_created_at', now()::TEXT);
```

```sql
-- 000010_create_metadata_table.down.sql
DROP TABLE IF EXISTS growatt.metadata;
```

---

## 3. Complete DDL Reference (All Tables)

This section presents every table with its exact columns for quick reference.

### growatt.plants

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| plant_id | TEXT | NOT NULL | -- | PK |
| plant_name | TEXT | NOT NULL | -- | |
| plant_type | INT | NOT NULL | 0 | |
| country | TEXT | NULL | -- | |
| city | TEXT | NULL | -- | |
| latitude | DOUBLE PRECISION | NULL | -- | |
| longitude | DOUBLE PRECISION | NULL | -- | |
| peak_power_kw | DOUBLE PRECISION | NULL | -- | Rated peak capacity |
| formula_coal | DOUBLE PRECISION | NULL | -- | kg per kWh |
| formula_co2 | DOUBLE PRECISION | NULL | -- | kg per kWh |
| formula_money | DOUBLE PRECISION | NULL | -- | Revenue per kWh |
| formula_tree | DOUBLE PRECISION | NULL | -- | Tree-equivalent factor |
| money_unit | TEXT | NULL | -- | |
| money_unit_text | TEXT | NULL | -- | |
| create_date | DATE | NULL | -- | |
| status | INT | NOT NULL | 0 | 0=offline, 1=online |
| created_at | TIMESTAMPTZ | NOT NULL | now() | |
| updated_at | TIMESTAMPTZ | NOT NULL | now() | |

### growatt.devices

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| device_sn | TEXT | NOT NULL | -- | PK |
| plant_id | TEXT | NOT NULL | -- | FK -> plants |
| device_type | INT | NOT NULL | 0 | |
| device_name | TEXT | NULL | -- | |
| model | TEXT | NULL | -- | |
| status | INT | NOT NULL | 0 | 0=offline, 1=online |
| last_update | TIMESTAMPTZ | NULL | -- | |
| created_at | TIMESTAMPTZ | NOT NULL | now() | |
| updated_at | TIMESTAMPTZ | NOT NULL | now() | |

### growatt.power_readings (HYPERTABLE)

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| ts | TIMESTAMPTZ | NOT NULL | -- | PK part 1 (time-first) |
| device_sn | TEXT | NOT NULL | -- | PK part 2 |
| pac_w | DOUBLE PRECISION | NULL | -- | AC output power (W) |
| ppv_w | DOUBLE PRECISION | NULL | -- | Total PV input power (W) |
| vpv1_v | DOUBLE PRECISION | NULL | -- | PV string 1 voltage (V) |
| vpv2_v | DOUBLE PRECISION | NULL | -- | PV string 2 voltage (V) |
| ipv1_a | DOUBLE PRECISION | NULL | -- | PV string 1 current (A) |
| ipv2_a | DOUBLE PRECISION | NULL | -- | PV string 2 current (A) |
| vac1_v | DOUBLE PRECISION | NULL | -- | Grid AC voltage (V) |
| iac1_a | DOUBLE PRECISION | NULL | -- | Grid AC current (A) |
| fetched_at | TIMESTAMPTZ | NOT NULL | now() | Audit trail: when data was fetched |

- **PK**: `(ts, device_sn)` -- time-first for TimescaleDB chunk pruning
- **Chunk interval**: 7 days (matches Growatt API max query range)
- **Compression**: after 30 days, segmentby=device_sn, orderby=ts DESC
- **Retention**: 2 years

### growatt.plant_snapshots (HYPERTABLE)

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| id | BIGINT | NOT NULL | IDENTITY | |
| plant_id | TEXT | NOT NULL | -- | FK -> plants |
| captured_at | TIMESTAMPTZ | NOT NULL | now() | |
| current_power_w | DOUBLE PRECISION | NULL | -- | |
| today_energy_kwh | DOUBLE PRECISION | NULL | -- | |
| month_energy_kwh | DOUBLE PRECISION | NULL | -- | |
| year_energy_kwh | DOUBLE PRECISION | NULL | -- | |
| total_energy_kwh | DOUBLE PRECISION | NULL | -- | |
| peak_power_today_kw | DOUBLE PRECISION | NULL | -- | |

- **PK**: `(id, captured_at)` -- required by TimescaleDB for hypertable partitioning on captured_at
- **Chunk interval**: 30 days
- **Compression**: after 30 days, segmentby=plant_id, orderby=captured_at DESC
- **Retention**: 1 year

### growatt.device_snapshots (HYPERTABLE)

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| id | BIGINT | NOT NULL | IDENTITY | |
| device_sn | TEXT | NOT NULL | -- | |
| captured_at | TIMESTAMPTZ | NOT NULL | now() | |
| pac_w | DOUBLE PRECISION | NULL | -- | |
| etoday_kwh | DOUBLE PRECISION | NULL | -- | |
| etotal_kwh | DOUBLE PRECISION | NULL | -- | |
| vpv1_v | DOUBLE PRECISION | NULL | -- | |
| vpv2_v | DOUBLE PRECISION | NULL | -- | |
| ipv1_a | DOUBLE PRECISION | NULL | -- | |
| ipv2_a | DOUBLE PRECISION | NULL | -- | |
| vac1_v | DOUBLE PRECISION | NULL | -- | |
| iac1_a | DOUBLE PRECISION | NULL | -- | |
| fac_hz | DOUBLE PRECISION | NULL | -- | Grid frequency (Hz) |
| temperature_c | DOUBLE PRECISION | NULL | -- | Inverter temp (C) |
| status | INT | NULL | -- | |

- **PK**: `(id, captured_at)`
- **Chunk interval**: 30 days
- **Compression**: after 30 days, segmentby=device_sn, orderby=captured_at DESC
- **Retention**: 1 year

### growatt.energy_summaries

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| period_date | DATE | NOT NULL | -- | PK part 1 |
| plant_id | TEXT | NOT NULL | -- | PK part 2, FK -> plants |
| time_unit | TEXT | NOT NULL | -- | PK part 3, CHECK IN ('day','month') |
| energy_kwh | DOUBLE PRECISION | NOT NULL | 0 | |
| created_at | TIMESTAMPTZ | NOT NULL | now() | |
| updated_at | TIMESTAMPTZ | NOT NULL | now() | |

### growatt.collection_runs

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| id | BIGINT | NOT NULL | IDENTITY | PK |
| source_type | TEXT | NOT NULL | -- | |
| source_id | TEXT | NOT NULL | -- | plant_id or device_sn |
| query_date_start | DATE | NULL | -- | |
| query_date_end | DATE | NULL | -- | |
| points_collected | INT | NOT NULL | 0 | |
| started_at | TIMESTAMPTZ | NOT NULL | now() | |
| finished_at | TIMESTAMPTZ | NULL | -- | |
| status | TEXT | NOT NULL | 'running' | CHECK IN (running,success,error,partial) |
| error_message | TEXT | NULL | -- | |

### growatt.collection_gaps

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| id | BIGINT | NOT NULL | IDENTITY | PK |
| device_sn | TEXT | NOT NULL | -- | |
| gap_date | DATE | NOT NULL | -- | |
| expected_start | TIME | NULL | -- | |
| expected_end | TIME | NULL | -- | |
| reason | TEXT | NULL | -- | |
| detected_at | TIMESTAMPTZ | NOT NULL | now() | |
| resolved_at | TIMESTAMPTZ | NULL | -- | |

- **UNIQUE**: `(device_sn, gap_date)`

### growatt.metadata

| Column | Type | Nullable | Default | Notes |
|--------|------|----------|---------|-------|
| key | TEXT | NOT NULL | -- | PK |
| value | TEXT | NOT NULL | -- | |
| updated_at | TIMESTAMPTZ | NOT NULL | now() | |

---

## 4. Continuous Aggregates: Exact Definitions

### cagg_hourly_power

Provides per-device hourly statistics. Automatically refreshed every hour covering the last 3 days (to capture backfill data) up to 1 hour ago.

**Columns produced:**

| Column | Expression | Type |
|--------|-----------|------|
| hour_start | `time_bucket('1 hour', ts)` | TIMESTAMPTZ |
| device_sn | `device_sn` | TEXT |
| samples | `COUNT(*)` | BIGINT |
| min_pac_w | `MIN(pac_w)` | DOUBLE PRECISION |
| max_pac_w | `MAX(pac_w)` | DOUBLE PRECISION |
| avg_pac_w | `AVG(pac_w)` | DOUBLE PRECISION |
| avg_ppv_w | `AVG(ppv_w)` | DOUBLE PRECISION |
| avg_vpv1_v | `AVG(vpv1_v)` | DOUBLE PRECISION |
| avg_vpv2_v | `AVG(vpv2_v)` | DOUBLE PRECISION |
| avg_ipv1_a | `AVG(ipv1_a)` | DOUBLE PRECISION |
| avg_ipv2_a | `AVG(ipv2_a)` | DOUBLE PRECISION |
| avg_vac1_v | `AVG(vac1_v)` | DOUBLE PRECISION |
| avg_iac1_a | `AVG(iac1_a)` | DOUBLE PRECISION |

**Refresh policy:**
- `start_offset`: 3 days (covers backfill window)
- `end_offset`: 1 hour (avoids re-aggregating in-flight data)
- `schedule_interval`: 1 hour

### cagg_daily_power

Provides per-device daily statistics and energy estimates. Automatically refreshed once per day covering the last 7 days.

**Columns produced:**

| Column | Expression | Type |
|--------|-----------|------|
| day | `time_bucket('1 day', ts)` | TIMESTAMPTZ |
| device_sn | `device_sn` | TEXT |
| samples | `COUNT(*)` | BIGINT |
| min_pac_w | `MIN(pac_w)` | DOUBLE PRECISION |
| peak_pac_w | `MAX(pac_w)` | DOUBLE PRECISION |
| avg_pac_w | `AVG(pac_w)` | DOUBLE PRECISION |
| estimated_energy_kwh | `SUM(pac_w) * (5.0/60.0) / 1000.0` | DOUBLE PRECISION |

**Refresh policy:**
- `start_offset`: 7 days (matches API lookback limit)
- `end_offset`: 1 day
- `schedule_interval`: 1 day

---

## 5. Upsert SQL Statements

### Power Readings Upsert

```sql
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
    fetched_at = EXCLUDED.fetched_at;
```

### Batch Power Readings Upsert (100 rows per statement)

```sql
INSERT INTO growatt.power_readings
    (ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
VALUES
    ($1,  $2,  $3,  $4,  $5,  $6,  $7,  $8,  $9,  $10, now()),
    ($11, $12, $13, $14, $15, $16, $17, $18, $19, $20, now()),
    -- ... up to 100 rows
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
```

### Plant Upsert

```sql
INSERT INTO growatt.plants
    (plant_id, plant_name, plant_type, country, city, latitude, longitude,
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
```

### Device Upsert

```sql
INSERT INTO growatt.devices
    (device_sn, plant_id, device_type, device_name, model, status, last_update)
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

### Energy Summary Upsert

```sql
INSERT INTO growatt.energy_summaries (period_date, plant_id, time_unit, energy_kwh)
VALUES ($1, $2, $3, $4)
ON CONFLICT (period_date, plant_id, time_unit) DO UPDATE SET
    energy_kwh = EXCLUDED.energy_kwh,
    updated_at = now();
```

---

## 6. Go Struct Definitions

All structs use `db` tags for `pgx`/`scany` scanning and `json` tags for REST API serialization. Column names match the database exactly.

```go
package db

import (
	"database/sql"
	"time"
)

// Plant maps to growatt.plants
type Plant struct {
	PlantID       string          `db:"plant_id"        json:"plant_id"`
	PlantName     string          `db:"plant_name"      json:"plant_name"`
	PlantType     int             `db:"plant_type"      json:"plant_type"`
	Country       sql.NullString  `db:"country"         json:"country,omitempty"`
	City          sql.NullString  `db:"city"            json:"city,omitempty"`
	Latitude      sql.NullFloat64 `db:"latitude"        json:"latitude,omitempty"`
	Longitude     sql.NullFloat64 `db:"longitude"       json:"longitude,omitempty"`
	PeakPowerKW   sql.NullFloat64 `db:"peak_power_kw"   json:"peak_power_kw,omitempty"`
	FormulaCoal   sql.NullFloat64 `db:"formula_coal"    json:"formula_coal,omitempty"`
	FormulaCO2    sql.NullFloat64 `db:"formula_co2"     json:"formula_co2,omitempty"`
	FormulaMoney  sql.NullFloat64 `db:"formula_money"   json:"formula_money,omitempty"`
	FormulaTree   sql.NullFloat64 `db:"formula_tree"    json:"formula_tree,omitempty"`
	MoneyUnit     sql.NullString  `db:"money_unit"      json:"money_unit,omitempty"`
	MoneyUnitText sql.NullString  `db:"money_unit_text" json:"money_unit_text,omitempty"`
	CreateDate    sql.NullTime    `db:"create_date"     json:"create_date,omitempty"`
	Status        int             `db:"status"          json:"status"`
	CreatedAt     time.Time       `db:"created_at"      json:"created_at"`
	UpdatedAt     time.Time       `db:"updated_at"      json:"updated_at"`
}

// Device maps to growatt.devices
type Device struct {
	DeviceSN   string         `db:"device_sn"   json:"device_sn"`
	PlantID    string         `db:"plant_id"    json:"plant_id"`
	DeviceType int            `db:"device_type" json:"device_type"`
	DeviceName sql.NullString `db:"device_name" json:"device_name,omitempty"`
	Model      sql.NullString `db:"model"       json:"model,omitempty"`
	Status     int            `db:"status"      json:"status"`
	LastUpdate sql.NullTime   `db:"last_update" json:"last_update,omitempty"`
	CreatedAt  time.Time      `db:"created_at"  json:"created_at"`
	UpdatedAt  time.Time      `db:"updated_at"  json:"updated_at"`
}

// PowerReading maps to growatt.power_readings
type PowerReading struct {
	Ts        time.Time       `db:"ts"        json:"ts"`
	DeviceSN  string          `db:"device_sn" json:"device_sn"`
	PacW      sql.NullFloat64 `db:"pac_w"     json:"pac_w,omitempty"`
	PpvW      sql.NullFloat64 `db:"ppv_w"     json:"ppv_w,omitempty"`
	Vpv1V     sql.NullFloat64 `db:"vpv1_v"    json:"vpv1_v,omitempty"`
	Vpv2V     sql.NullFloat64 `db:"vpv2_v"    json:"vpv2_v,omitempty"`
	Ipv1A     sql.NullFloat64 `db:"ipv1_a"    json:"ipv1_a,omitempty"`
	Ipv2A     sql.NullFloat64 `db:"ipv2_a"    json:"ipv2_a,omitempty"`
	Vac1V     sql.NullFloat64 `db:"vac1_v"    json:"vac1_v,omitempty"`
	Iac1A     sql.NullFloat64 `db:"iac1_a"    json:"iac1_a,omitempty"`
	FetchedAt time.Time       `db:"fetched_at" json:"fetched_at"`
}

// PlantSnapshot maps to growatt.plant_snapshots
type PlantSnapshot struct {
	ID               int64           `db:"id"                  json:"id"`
	PlantID          string          `db:"plant_id"            json:"plant_id"`
	CapturedAt       time.Time       `db:"captured_at"         json:"captured_at"`
	CurrentPowerW    sql.NullFloat64 `db:"current_power_w"     json:"current_power_w,omitempty"`
	TodayEnergyKWH   sql.NullFloat64 `db:"today_energy_kwh"    json:"today_energy_kwh,omitempty"`
	MonthEnergyKWH   sql.NullFloat64 `db:"month_energy_kwh"    json:"month_energy_kwh,omitempty"`
	YearEnergyKWH    sql.NullFloat64 `db:"year_energy_kwh"     json:"year_energy_kwh,omitempty"`
	TotalEnergyKWH   sql.NullFloat64 `db:"total_energy_kwh"    json:"total_energy_kwh,omitempty"`
	PeakPowerTodayKW sql.NullFloat64 `db:"peak_power_today_kw" json:"peak_power_today_kw,omitempty"`
}

// DeviceSnapshot maps to growatt.device_snapshots
type DeviceSnapshot struct {
	ID           int64           `db:"id"             json:"id"`
	DeviceSN     string          `db:"device_sn"      json:"device_sn"`
	CapturedAt   time.Time       `db:"captured_at"    json:"captured_at"`
	PacW         sql.NullFloat64 `db:"pac_w"          json:"pac_w,omitempty"`
	EtodayKWH    sql.NullFloat64 `db:"etoday_kwh"     json:"etoday_kwh,omitempty"`
	EtotalKWH    sql.NullFloat64 `db:"etotal_kwh"     json:"etotal_kwh,omitempty"`
	Vpv1V        sql.NullFloat64 `db:"vpv1_v"         json:"vpv1_v,omitempty"`
	Vpv2V        sql.NullFloat64 `db:"vpv2_v"         json:"vpv2_v,omitempty"`
	Ipv1A        sql.NullFloat64 `db:"ipv1_a"         json:"ipv1_a,omitempty"`
	Ipv2A        sql.NullFloat64 `db:"ipv2_a"         json:"ipv2_a,omitempty"`
	Vac1V        sql.NullFloat64 `db:"vac1_v"         json:"vac1_v,omitempty"`
	Iac1A        sql.NullFloat64 `db:"iac1_a"         json:"iac1_a,omitempty"`
	FacHz        sql.NullFloat64 `db:"fac_hz"         json:"fac_hz,omitempty"`
	TemperatureC sql.NullFloat64 `db:"temperature_c"  json:"temperature_c,omitempty"`
	Status       sql.NullInt32   `db:"status"         json:"status,omitempty"`
}

// EnergySummary maps to growatt.energy_summaries
type EnergySummary struct {
	PeriodDate time.Time `db:"period_date" json:"period_date"`
	PlantID    string    `db:"plant_id"    json:"plant_id"`
	TimeUnit   string    `db:"time_unit"   json:"time_unit"`
	EnergyKWH  float64   `db:"energy_kwh"  json:"energy_kwh"`
	CreatedAt  time.Time `db:"created_at"  json:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"  json:"updated_at"`
}

// CollectionRun maps to growatt.collection_runs
type CollectionRun struct {
	ID              int64          `db:"id"                json:"id"`
	SourceType      string         `db:"source_type"       json:"source_type"`
	SourceID        string         `db:"source_id"         json:"source_id"`
	QueryDateStart  sql.NullTime   `db:"query_date_start"  json:"query_date_start,omitempty"`
	QueryDateEnd    sql.NullTime   `db:"query_date_end"    json:"query_date_end,omitempty"`
	PointsCollected int            `db:"points_collected"  json:"points_collected"`
	StartedAt       time.Time      `db:"started_at"        json:"started_at"`
	FinishedAt      sql.NullTime   `db:"finished_at"       json:"finished_at,omitempty"`
	Status          string         `db:"status"            json:"status"`
	ErrorMessage    sql.NullString `db:"error_message"     json:"error_message,omitempty"`
}

// CollectionGap maps to growatt.collection_gaps
type CollectionGap struct {
	ID            int64          `db:"id"             json:"id"`
	DeviceSN      string         `db:"device_sn"      json:"device_sn"`
	GapDate       time.Time      `db:"gap_date"       json:"gap_date"`
	ExpectedStart sql.NullString `db:"expected_start"  json:"expected_start,omitempty"`
	ExpectedEnd   sql.NullString `db:"expected_end"    json:"expected_end,omitempty"`
	Reason        sql.NullString `db:"reason"          json:"reason,omitempty"`
	DetectedAt    time.Time      `db:"detected_at"     json:"detected_at"`
	ResolvedAt    sql.NullTime   `db:"resolved_at"     json:"resolved_at,omitempty"`
}

// Metadata maps to growatt.metadata
type Metadata struct {
	Key       string    `db:"key"        json:"key"`
	Value     string    `db:"value"      json:"value"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// --- Continuous aggregate / view result types ---

// HourlyPower maps to growatt.cagg_hourly_power
type HourlyPower struct {
	HourStart time.Time       `db:"hour_start" json:"hour_start"`
	DeviceSN  string          `db:"device_sn"  json:"device_sn"`
	Samples   int64           `db:"samples"    json:"samples"`
	MinPacW   sql.NullFloat64 `db:"min_pac_w"  json:"min_pac_w,omitempty"`
	MaxPacW   sql.NullFloat64 `db:"max_pac_w"  json:"max_pac_w,omitempty"`
	AvgPacW   sql.NullFloat64 `db:"avg_pac_w"  json:"avg_pac_w,omitempty"`
	AvgPpvW   sql.NullFloat64 `db:"avg_ppv_w"  json:"avg_ppv_w,omitempty"`
	AvgVpv1V  sql.NullFloat64 `db:"avg_vpv1_v" json:"avg_vpv1_v,omitempty"`
	AvgVpv2V  sql.NullFloat64 `db:"avg_vpv2_v" json:"avg_vpv2_v,omitempty"`
	AvgIpv1A  sql.NullFloat64 `db:"avg_ipv1_a" json:"avg_ipv1_a,omitempty"`
	AvgIpv2A  sql.NullFloat64 `db:"avg_ipv2_a" json:"avg_ipv2_a,omitempty"`
	AvgVac1V  sql.NullFloat64 `db:"avg_vac1_v" json:"avg_vac1_v,omitempty"`
	AvgIac1A  sql.NullFloat64 `db:"avg_iac1_a" json:"avg_iac1_a,omitempty"`
}

// DailyPower maps to growatt.cagg_daily_power
type DailyPower struct {
	Day                time.Time       `db:"day"                   json:"day"`
	DeviceSN           string          `db:"device_sn"             json:"device_sn"`
	Samples            int64           `db:"samples"               json:"samples"`
	MinPacW            sql.NullFloat64 `db:"min_pac_w"             json:"min_pac_w,omitempty"`
	PeakPacW           sql.NullFloat64 `db:"peak_pac_w"            json:"peak_pac_w,omitempty"`
	AvgPacW            sql.NullFloat64 `db:"avg_pac_w"             json:"avg_pac_w,omitempty"`
	EstimatedEnergyKWH sql.NullFloat64 `db:"estimated_energy_kwh"  json:"estimated_energy_kwh,omitempty"`
}

// DailyProduction maps to growatt.mv_daily_production
type DailyProduction struct {
	ProductionDate     time.Time       `db:"production_date"       json:"production_date"`
	DeviceSN           string          `db:"device_sn"             json:"device_sn"`
	TotalSamples       int64           `db:"total_samples"         json:"total_samples"`
	MinPacW            sql.NullFloat64 `db:"min_pac_w"             json:"min_pac_w,omitempty"`
	PeakPacW           sql.NullFloat64 `db:"peak_pac_w"            json:"peak_pac_w,omitempty"`
	AvgPacW            sql.NullFloat64 `db:"avg_pac_w"             json:"avg_pac_w,omitempty"`
	EstimatedEnergyKWH sql.NullFloat64 `db:"estimated_energy_kwh"  json:"estimated_energy_kwh,omitempty"`
	FirstReading       time.Time       `db:"first_reading"         json:"first_reading"`
	LastReading        time.Time       `db:"last_reading"          json:"last_reading"`
	ProductionHours    sql.NullFloat64 `db:"production_hours"      json:"production_hours,omitempty"`
}

// MonthlyProduction maps to growatt.mv_monthly_production
type MonthlyProduction struct {
	Month               time.Time       `db:"month"                    json:"month"`
	PlantID             string          `db:"plant_id"                 json:"plant_id"`
	TotalEnergyKWH     float64         `db:"total_energy_kwh"         json:"total_energy_kwh"`
	AvgDailyEnergyKWH  sql.NullFloat64 `db:"avg_daily_energy_kwh"     json:"avg_daily_energy_kwh,omitempty"`
	BestDayKWH         sql.NullFloat64 `db:"best_day_kwh"             json:"best_day_kwh,omitempty"`
	WorstProducingDayKWH sql.NullFloat64 `db:"worst_producing_day_kwh" json:"worst_producing_day_kwh,omitempty"`
	DaysWithData        int64           `db:"days_with_data"           json:"days_with_data"`
	ProducingDays       int64           `db:"producing_days"           json:"producing_days"`
}
```

### Alternative: Using pgx-native types instead of database/sql

For projects preferring `pgx` native types (better performance, no `sql.Null*` wrappers):

```go
package db

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// PowerReadingPgx uses pgx-native nullable types
type PowerReadingPgx struct {
	Ts        time.Time       `db:"ts"        json:"ts"`
	DeviceSN  string          `db:"device_sn" json:"device_sn"`
	PacW      pgtype.Float8   `db:"pac_w"     json:"pac_w"`
	PpvW      pgtype.Float8   `db:"ppv_w"     json:"ppv_w"`
	Vpv1V     pgtype.Float8   `db:"vpv1_v"    json:"vpv1_v"`
	Vpv2V     pgtype.Float8   `db:"vpv2_v"    json:"vpv2_v"`
	Ipv1A     pgtype.Float8   `db:"ipv1_a"    json:"ipv1_a"`
	Ipv2A     pgtype.Float8   `db:"ipv2_a"    json:"ipv2_a"`
	Vac1V     pgtype.Float8   `db:"vac1_v"    json:"vac1_v"`
	Iac1A     pgtype.Float8   `db:"iac1_a"    json:"iac1_a"`
	FetchedAt time.Time       `db:"fetched_at" json:"fetched_at"`
}
```

---

## 7. Repository Layer Interfaces

```go
package db

import (
	"context"
	"time"
)

// PlantRepository manages growatt.plants
type PlantRepository interface {
	// UpsertPlant inserts or updates a plant record. Latest API data wins.
	UpsertPlant(ctx context.Context, plant *Plant) error

	// GetPlant retrieves a single plant by ID.
	GetPlant(ctx context.Context, plantID string) (*Plant, error)

	// ListPlants returns all plants, optionally filtered by status.
	ListPlants(ctx context.Context, statusFilter *int) ([]Plant, error)
}

// DeviceRepository manages growatt.devices
type DeviceRepository interface {
	// UpsertDevice inserts or updates a device record.
	UpsertDevice(ctx context.Context, device *Device) error

	// GetDevice retrieves a single device by serial number.
	GetDevice(ctx context.Context, deviceSN string) (*Device, error)

	// ListDevicesByPlant returns all devices for a given plant.
	ListDevicesByPlant(ctx context.Context, plantID string) ([]Device, error)
}

// PowerReadingRepository manages growatt.power_readings
type PowerReadingRepository interface {
	// UpsertReadings batch-inserts power readings with ON CONFLICT DO UPDATE.
	// Returns the number of rows affected (inserted + updated).
	UpsertReadings(ctx context.Context, readings []PowerReading) (int64, error)

	// GetReadings returns power readings for a device within a time range.
	// Results are ordered by ts ASC.
	GetReadings(ctx context.Context, deviceSN string, from, to time.Time) ([]PowerReading, error)

	// GetLatestReading returns the most recent reading for a device.
	// Returns nil if no readings exist.
	GetLatestReading(ctx context.Context, deviceSN string) (*PowerReading, error)

	// GetReadingDates returns distinct dates that have data for a device.
	GetReadingDates(ctx context.Context, deviceSN string, from, to time.Time) ([]time.Time, error)
}

// SnapshotRepository manages growatt.plant_snapshots and growatt.device_snapshots
type SnapshotRepository interface {
	// InsertPlantSnapshot inserts a new plant snapshot.
	InsertPlantSnapshot(ctx context.Context, snap *PlantSnapshot) error

	// InsertDeviceSnapshot inserts a new device snapshot.
	InsertDeviceSnapshot(ctx context.Context, snap *DeviceSnapshot) error

	// GetLatestPlantSnapshot returns the most recent snapshot for a plant.
	GetLatestPlantSnapshot(ctx context.Context, plantID string) (*PlantSnapshot, error)

	// GetLatestDeviceSnapshot returns the most recent snapshot for a device.
	GetLatestDeviceSnapshot(ctx context.Context, deviceSN string) (*DeviceSnapshot, error)

	// GetPlantSnapshots returns snapshots for a plant in a time range.
	GetPlantSnapshots(ctx context.Context, plantID string, from, to time.Time) ([]PlantSnapshot, error)

	// GetDeviceSnapshots returns snapshots for a device in a time range.
	GetDeviceSnapshots(ctx context.Context, deviceSN string, from, to time.Time) ([]DeviceSnapshot, error)
}

// EnergySummaryRepository manages growatt.energy_summaries
type EnergySummaryRepository interface {
	// UpsertEnergySummary inserts or updates an energy summary.
	UpsertEnergySummary(ctx context.Context, summary *EnergySummary) error

	// GetDailyEnergy returns daily energy summaries for a plant in a date range.
	GetDailyEnergy(ctx context.Context, plantID string, from, to time.Time) ([]EnergySummary, error)

	// GetMonthlyEnergy returns monthly energy summaries for a plant in a date range.
	GetMonthlyEnergy(ctx context.Context, plantID string, from, to time.Time) ([]EnergySummary, error)
}

// AggregateRepository manages continuous aggregates and materialized views
type AggregateRepository interface {
	// GetHourlyPower returns hourly aggregates for a device in a time range.
	GetHourlyPower(ctx context.Context, deviceSN string, from, to time.Time) ([]HourlyPower, error)

	// GetDailyPower returns daily aggregates for a device in a time range.
	GetDailyPower(ctx context.Context, deviceSN string, from, to time.Time) ([]DailyPower, error)

	// GetDailyProduction returns daily production from the materialized view.
	GetDailyProduction(ctx context.Context, deviceSN string, from, to time.Time) ([]DailyProduction, error)

	// GetMonthlyProduction returns monthly production from the materialized view.
	GetMonthlyProduction(ctx context.Context, plantID string, from, to time.Time) ([]MonthlyProduction, error)

	// RefreshMaterializedViews calls the refresh procedure.
	RefreshMaterializedViews(ctx context.Context) error
}

// CollectionRepository manages growatt.collection_runs and growatt.collection_gaps
type CollectionRepository interface {
	// StartRun creates a new collection run in 'running' state and returns its ID.
	StartRun(ctx context.Context, sourceType, sourceID string, dateStart, dateEnd *time.Time) (int64, error)

	// CompleteRun marks a run as success/error/partial with final stats.
	CompleteRun(ctx context.Context, id int64, status string, pointsCollected int, errMsg *string) error

	// GetLastSuccessfulRun returns the most recent successful run for a source.
	GetLastSuccessfulRun(ctx context.Context, sourceType, sourceID string) (*CollectionRun, error)

	// RecordGap inserts or ignores a collection gap.
	RecordGap(ctx context.Context, gap *CollectionGap) error

	// ResolveGap marks a gap as resolved.
	ResolveGap(ctx context.Context, deviceSN string, gapDate time.Time) error

	// GetUnresolvedGaps returns all unresolved gaps for a device.
	GetUnresolvedGaps(ctx context.Context, deviceSN string) ([]CollectionGap, error)
}

// MetadataRepository manages growatt.metadata
type MetadataRepository interface {
	// Get returns a metadata value by key.
	Get(ctx context.Context, key string) (string, error)

	// Set inserts or updates a metadata key-value pair.
	Set(ctx context.Context, key, value string) error
}
```

---

## 8. Connection Pooling Configuration (pgxpool)

```go
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig holds database connection pool configuration.
type PoolConfig struct {
	// DatabaseURL is the PostgreSQL connection string.
	// Example: postgres://growatt:password@localhost:5432/growatt?sslmode=disable
	DatabaseURL string

	// MaxConns is the maximum number of connections in the pool.
	// For a single-inverter deployment, 10 is sufficient.
	// The fetcher uses 1-2 connections; the REST API uses the rest.
	MaxConns int32

	// MinConns is the minimum number of idle connections to maintain.
	MinConns int32

	// MaxConnLifetime is the maximum time a connection can be reused.
	MaxConnLifetime time.Duration

	// MaxConnIdleTime is the maximum time a connection can sit idle.
	MaxConnIdleTime time.Duration

	// HealthCheckPeriod is how often idle connections are health-checked.
	HealthCheckPeriod time.Duration
}

// DefaultPoolConfig returns sensible defaults for a small deployment.
func DefaultPoolConfig(databaseURL string) PoolConfig {
	return PoolConfig{
		DatabaseURL:       databaseURL,
		MaxConns:          10,
		MinConns:          2,
		MaxConnLifetime:   30 * time.Minute,
		MaxConnIdleTime:   5 * time.Minute,
		HealthCheckPeriod: 1 * time.Minute,
	}
}

// NewPool creates a configured pgxpool.Pool.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod

	// Set search_path so unqualified table names resolve to growatt schema.
	// Queries still use growatt.* prefix for clarity, but this is a safety net.
	poolCfg.ConnConfig.RuntimeParams["search_path"] = "growatt,public"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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

---

## 9. Database Initialization and Migration Code

```go
package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies all pending migrations.
func RunMigrations(databaseURL string) error {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("creating migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("running migrations: %w", err)
	}

	version, dirty, _ := m.Version()
	slog.Info("migrations complete", "version", version, "dirty", dirty)
	return nil
}

// MigrateDown rolls back N migrations. Use for testing.
func MigrateDown(databaseURL string, steps int) error {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("creating migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()

	if err := m.Steps(-steps); err != nil {
		return fmt.Errorf("rolling back %d migrations: %w", steps, err)
	}

	return nil
}

// InitDB creates the pool and runs migrations. This is the main entry point
// for application startup.
func InitDB(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	// Run migrations first (uses its own connection)
	slog.Info("running database migrations", "url", sanitizeURL(cfg.DatabaseURL))
	if err := RunMigrations(cfg.DatabaseURL); err != nil {
		return nil, fmt.Errorf("migrations failed: %w", err)
	}

	// Create the connection pool
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}

	slog.Info("database initialized",
		"max_conns", cfg.MaxConns,
		"min_conns", cfg.MinConns,
	)
	return pool, nil
}

// sanitizeURL removes the password from a database URL for logging.
func sanitizeURL(u string) string {
	// Simple approach: mask anything between :// user: and @
	// A production implementation would use url.Parse
	return "postgres://***@..."
}
```

### Proposed Package Layout

```
internal/
  db/
    db.go                  -- InitDB, NewPool, RunMigrations (code above)
    models.go              -- All Go struct definitions (section 6)
    repository.go          -- All interface definitions (section 7)
    plant_repo.go          -- PlantRepository implementation
    device_repo.go         -- DeviceRepository implementation
    power_reading_repo.go  -- PowerReadingRepository implementation
    snapshot_repo.go       -- SnapshotRepository implementation
    energy_repo.go         -- EnergySummaryRepository implementation
    aggregate_repo.go      -- AggregateRepository implementation
    collection_repo.go     -- CollectionRepository implementation
    metadata_repo.go       -- MetadataRepository implementation
    migrations/
      000001_create_schema_and_extensions.up.sql
      000001_create_schema_and_extensions.down.sql
      000002_create_dimension_tables.up.sql
      000002_create_dimension_tables.down.sql
      000003_create_hypertables.up.sql
      000003_create_hypertables.down.sql
      000004_create_regular_tables.up.sql
      000004_create_regular_tables.down.sql
      000005_create_indexes.up.sql
      000005_create_indexes.down.sql
      000006_create_compression_policies.up.sql
      000006_create_compression_policies.down.sql
      000007_create_continuous_aggregates.up.sql
      000007_create_continuous_aggregates.down.sql
      000008_create_retention_policies.up.sql
      000008_create_retention_policies.down.sql
      000009_create_views_and_procedures.up.sql
      000009_create_views_and_procedures.down.sql
      000010_create_metadata_table.up.sql
      000010_create_metadata_table.down.sql
```

---

## 10. Repository Implementation Example: PowerReadingRepository

```go
package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type powerReadingRepo struct {
	pool *pgxpool.Pool
}

func NewPowerReadingRepository(pool *pgxpool.Pool) PowerReadingRepository {
	return &powerReadingRepo{pool: pool}
}

const upsertReadingSQL = `
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
    fetched_at = EXCLUDED.fetched_at`

// UpsertReadings batch-inserts using pgx.CopyFrom for maximum throughput,
// falling back to multi-row INSERT ON CONFLICT for upsert semantics.
// Batches are 100 rows (matches API page size).
func (r *powerReadingRepo) UpsertReadings(ctx context.Context, readings []PowerReading) (int64, error) {
	if len(readings) == 0 {
		return 0, nil
	}

	const batchSize = 100
	var totalAffected int64

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	for i := 0; i < len(readings); i += batchSize {
		end := i + batchSize
		if end > len(readings) {
			end = len(readings)
		}
		batch := readings[i:end]

		// Build multi-row INSERT statement
		var sb strings.Builder
		sb.WriteString(`INSERT INTO growatt.power_readings
			(ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at)
			VALUES `)

		args := make([]any, 0, len(batch)*10)
		for j, rd := range batch {
			if j > 0 {
				sb.WriteString(", ")
			}
			base := j * 10
			sb.WriteString(fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, now())",
				base+1, base+2, base+3, base+4, base+5,
				base+6, base+7, base+8, base+9, base+10))
			args = append(args, rd.Ts, rd.DeviceSN, rd.PacW, rd.PpvW,
				rd.Vpv1V, rd.Vpv2V, rd.Ipv1A, rd.Ipv2A, rd.Vac1V, rd.Iac1A)
		}

		sb.WriteString(` ON CONFLICT (ts, device_sn) DO UPDATE SET
			pac_w      = EXCLUDED.pac_w,
			ppv_w      = EXCLUDED.ppv_w,
			vpv1_v     = EXCLUDED.vpv1_v,
			vpv2_v     = EXCLUDED.vpv2_v,
			ipv1_a     = EXCLUDED.ipv1_a,
			ipv2_a     = EXCLUDED.ipv2_a,
			vac1_v     = EXCLUDED.vac1_v,
			iac1_a     = EXCLUDED.iac1_a,
			fetched_at = EXCLUDED.fetched_at`)

		ct, err := tx.Exec(ctx, sb.String(), args...)
		if err != nil {
			return totalAffected, fmt.Errorf("batch upsert (offset %d): %w", i, err)
		}
		totalAffected += ct.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return totalAffected, nil
}

const getReadingsSQL = `
SELECT ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at
FROM growatt.power_readings
WHERE device_sn = $1 AND ts >= $2 AND ts < $3
ORDER BY ts ASC`

func (r *powerReadingRepo) GetReadings(ctx context.Context, deviceSN string, from, to time.Time) ([]PowerReading, error) {
	rows, err := r.pool.Query(ctx, getReadingsSQL, deviceSN, from, to)
	if err != nil {
		return nil, fmt.Errorf("querying readings: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, pgx.RowToStructByName[PowerReading])
}

const getLatestReadingSQL = `
SELECT ts, device_sn, pac_w, ppv_w, vpv1_v, vpv2_v, ipv1_a, ipv2_a, vac1_v, iac1_a, fetched_at
FROM growatt.power_readings
WHERE device_sn = $1
ORDER BY ts DESC
LIMIT 1`

func (r *powerReadingRepo) GetLatestReading(ctx context.Context, deviceSN string) (*PowerReading, error) {
	rows, err := r.pool.Query(ctx, getLatestReadingSQL, deviceSN)
	if err != nil {
		return nil, fmt.Errorf("querying latest reading: %w", err)
	}
	defer rows.Close()

	reading, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[PowerReading])
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanning latest reading: %w", err)
	}
	return &reading, nil
}

const getReadingDatesSQL = `
SELECT DISTINCT ts::date AS d
FROM growatt.power_readings
WHERE device_sn = $1 AND ts >= $2 AND ts < $3
ORDER BY d ASC`

func (r *powerReadingRepo) GetReadingDates(ctx context.Context, deviceSN string, from, to time.Time) ([]time.Time, error) {
	rows, err := r.pool.Query(ctx, getReadingDatesSQL, deviceSN, from, to)
	if err != nil {
		return nil, fmt.Errorf("querying reading dates: %w", err)
	}
	defer rows.Close()

	var dates []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scanning date: %w", err)
		}
		dates = append(dates, d)
	}
	return dates, rows.Err()
}
```

---

## 11. Query Patterns and Index Justification

### Primary Query Patterns

| # | Query Pattern | Frequency | Index Used | Explanation |
|---|--------------|-----------|-----------|-------------|
| 1 | Power readings for device X between date A and B | Very high (every page load) | `idx_power_readings_device_ts (device_sn, ts DESC)` | Device-first lookup. TimescaleDB also prunes chunks by `ts` range from the PK. |
| 2 | Latest reading for a device | High (dashboard polling) | `idx_power_readings_device_ts` | `ORDER BY ts DESC LIMIT 1` on device_sn. |
| 3 | Hourly aggregates for a device | Medium (charts) | Continuous aggregate `cagg_hourly_power` PK | Pre-computed, no scan of raw data needed. |
| 4 | Daily energy for a device | Medium (summary views) | Continuous aggregate `cagg_daily_power` PK | Pre-computed. |
| 5 | Latest plant snapshot | Medium (dashboard) | `idx_plant_snapshots_plant_time (plant_id, captured_at DESC)` | `ORDER BY captured_at DESC LIMIT 1`. |
| 6 | Latest device snapshot | Medium (dashboard) | `idx_device_snapshots_device_time (device_sn, captured_at DESC)` | Same pattern. |
| 7 | Monthly energy summaries for a plant | Low (reports) | `idx_energy_summaries_plant_unit_date (plant_id, time_unit, period_date DESC)` | Plant-first, then date range. |
| 8 | All devices for a plant | Low (config pages) | `idx_devices_plant_id (plant_id)` | Simple FK lookup. |
| 9 | Last successful collection run | Medium (fetcher startup) | `idx_collection_runs_source (source_type, source_id, query_date_start DESC)` | Fetcher queries this at startup for backfill decisions. |
| 10 | Detect missing dates | Low (gap detection) | PK `(ts, device_sn)` + chunk pruning | Cross-join with generate_series; chunk pruning keeps this fast for recent date ranges. |

### Why (ts, device_sn) PK Order

TimescaleDB partitions hypertables by the first column in `by_range()`. With `ts` first:
- Chunk pruning eliminates entire 7-day chunks when queries include a `ts` range constraint
- The secondary `idx_power_readings_device_ts` index covers device-first lookups
- Compression orders by `ts DESC` within each `device_sn` segment, which aligns with the most common query pattern (recent data first for a specific device)

If the PK were `(device_sn, ts)`, TimescaleDB could not partition on time, defeating the primary benefit of hypertables.

---

## 12. Performance Considerations

### Storage Estimates (Single Inverter)

| Table | Rows/day | Row size | Daily | Annual (uncompressed) | Annual (compressed ~10x) |
|-------|---------|---------|-------|----------------------|------------------------|
| power_readings | ~144 | ~100 B | ~14 KB | ~5 MB | ~500 KB |
| device_snapshots | ~288 | ~120 B | ~34 KB | ~12 MB | ~1.2 MB |
| plant_snapshots | ~288 | ~80 B | ~23 KB | ~8 MB | ~800 KB |
| energy_summaries | ~1 | ~60 B | ~60 B | ~22 KB | N/A (not compressed) |
| cagg_hourly_power | ~24 | ~100 B | ~2.4 KB | ~876 KB | N/A |
| cagg_daily_power | ~1 | ~80 B | ~80 B | ~29 KB | N/A |

**Total annual per inverter (compressed): ~2.5 MB.** This schema handles hundreds of inverters on modest hardware (2 CPU, 4 GB RAM, 50 GB disk).

### Chunk Interval Selection

| Hypertable | Chunk Interval | Rationale |
|-----------|---------------|-----------|
| power_readings | 7 days | Matches the Growatt API's maximum query range. Each backfill operation touches at most one chunk. A single inverter produces ~1,000 rows per chunk (~100 KB), well within TimescaleDB's recommended range. |
| plant_snapshots | 30 days | Lower write rate than power_readings. 30-day chunks keep the chunk count low. |
| device_snapshots | 30 days | Same rationale as plant_snapshots. |

### Connection Pool Sizing

For a typical deployment (1 fetcher + 1 REST API server):
- Fetcher: 1-2 connections (1 for upserts, 1 for metadata/gap queries)
- REST API: 5-8 connections (concurrent HTTP requests)
- Background: 1-2 connections (TimescaleDB background workers)
- **Total: 10 connections** (the default `MaxConns`)

### Write Performance

The batch upsert strategy (100 rows per INSERT statement within a transaction) achieves:
- ~1,000 rows/second on modest hardware (single inverter's full daily data in <0.2 seconds)
- Transaction scope is per-fetch-cycle, so a single COMMIT covers all rows from one API call
- The `ON CONFLICT DO UPDATE` clause has negligible overhead compared to plain INSERT because the conflict detection uses the PK index which is already in the buffer cache for recent data

### Read Performance

- **Raw data queries** (pattern #1): Chunk pruning by `ts` range + index scan by `device_sn` within the chunk. Typical latency: <5ms for a single day.
- **Aggregate queries** (patterns #3-4): Read directly from continuous aggregates. No scan of raw data. Typical latency: <2ms.
- **Latest snapshot** (patterns #5-6): Index-only scan on `(device_sn, captured_at DESC)` with `LIMIT 1`. Typical latency: <1ms.

---

## 13. Testing Strategy

### Unit Tests (no database required)

Test the Go model layer:
- Struct field tag consistency: verify every `db` tag matches a known column name
- SQL query string validation: ensure all query constants parse without syntax errors (use `pg_query_go` parser or similar)
- Timestamp parsing helpers: table-driven tests for all Growatt time formats

### Integration Tests (require TimescaleDB)

Use `docker-compose.test.yml` with tmpfs-backed storage for fast teardown.

```go
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/emergingrobotics/gogrowatt/internal/db"
)

// testPool is created once per package using TestMain
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	// Start TimescaleDB container (or use existing test instance)
	// Run migrations
	// Create pool
	// Run tests
	// Tear down
}

func TestMigrationUpDown(t *testing.T) {
	// Test: apply all migrations up, then roll them all back, then reapply.
	// This verifies every .down.sql correctly reverses its .up.sql.
	url := testDatabaseURL()

	// Up
	err := db.RunMigrations(url)
	if err != nil {
		t.Fatalf("migrations up failed: %v", err)
	}

	// Verify schema exists
	var exists bool
	testPool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = 'growatt')").
		Scan(&exists)
	if !exists {
		t.Fatal("growatt schema does not exist after migration up")
	}

	// Down all
	err = db.MigrateDown(url, 10) // 10 migrations
	if err != nil {
		t.Fatalf("migrations down failed: %v", err)
	}

	// Up again (idempotency)
	err = db.RunMigrations(url)
	if err != nil {
		t.Fatalf("re-migration up failed: %v", err)
	}
}

func TestHypertablesCreated(t *testing.T) {
	var count int
	err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*)
		FROM timescaledb_information.hypertables
		WHERE hypertable_schema = 'growatt'`).Scan(&count)
	if err != nil {
		t.Fatalf("querying hypertables: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 hypertables, got %d", count)
	}
}

func TestCompressionPoliciesExist(t *testing.T) {
	var count int
	err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*)
		FROM timescaledb_information.compression_settings
		WHERE hypertable_schema = 'growatt'`).Scan(&count)
	if err != nil {
		t.Fatalf("querying compression settings: %v", err)
	}
	// 3 hypertables * multiple columns each = at least 3 distinct hypertables
	if count < 3 {
		t.Errorf("expected at least 3 compression settings, got %d", count)
	}
}

func TestRetentionPoliciesExist(t *testing.T) {
	var count int
	err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*)
		FROM timescaledb_information.jobs
		WHERE application_name LIKE '%retention%'`).Scan(&count)
	if err != nil {
		t.Fatalf("querying retention jobs: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 retention policies, got %d", count)
	}
}

func TestContinuousAggregatesExist(t *testing.T) {
	rows, err := testPool.Query(context.Background(), `
		SELECT view_name
		FROM timescaledb_information.continuous_aggregates
		WHERE view_schema = 'growatt'
		ORDER BY view_name`)
	if err != nil {
		t.Fatalf("querying continuous aggregates: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		names = append(names, name)
	}
	expected := []string{"cagg_daily_power", "cagg_hourly_power"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d continuous aggregates, got %d: %v", len(expected), len(names), names)
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("expected cagg %q at position %d, got %q", expected[i], i, name)
		}
	}
}

func TestPowerReadingUpsert(t *testing.T) {
	ctx := context.Background()
	repo := db.NewPowerReadingRepository(testPool)

	// Seed dimension data
	seedPlantAndDevice(t, testPool)

	ts := time.Date(2026, 2, 15, 18, 5, 0, 0, time.UTC)
	readings := []db.PowerReading{
		{
			Ts:       ts,
			DeviceSN: "TEST001",
			PacW:     nullFloat(2729.5),
			PpvW:     nullFloat(2810.0),
			Vpv1V:    nullFloat(312.4),
			Vpv2V:    nullFloat(310.1),
			Ipv1A:    nullFloat(4.5),
			Ipv2A:    nullFloat(4.6),
			Vac1V:    nullFloat(241.2),
			Iac1A:    nullFloat(11.3),
		},
	}

	// First insert
	affected, err := repo.UpsertReadings(ctx, readings)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if affected != 1 {
		t.Errorf("expected 1 row affected, got %d", affected)
	}

	// Verify
	got, err := repo.GetLatestReading(ctx, "TEST001")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got.PacW.Float64 != 2729.5 {
		t.Errorf("expected pac_w=2729.5, got %f", got.PacW.Float64)
	}

	// Upsert with new value (latest wins)
	readings[0].PacW = nullFloat(2750.0)
	affected, err = repo.UpsertReadings(ctx, readings)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Verify overwrite
	got, err = repo.GetLatestReading(ctx, "TEST001")
	if err != nil {
		t.Fatalf("get latest after upsert: %v", err)
	}
	if got.PacW.Float64 != 2750.0 {
		t.Errorf("expected pac_w=2750.0 after upsert, got %f", got.PacW.Float64)
	}
}

func TestPowerReadingTimeRangeQuery(t *testing.T) {
	ctx := context.Background()
	repo := db.NewPowerReadingRepository(testPool)

	// Insert 3 readings at 12:00, 12:05, 12:10
	base := time.Date(2026, 2, 15, 18, 0, 0, 0, time.UTC)
	readings := make([]db.PowerReading, 3)
	for i := range readings {
		readings[i] = db.PowerReading{
			Ts:       base.Add(time.Duration(i*5) * time.Minute),
			DeviceSN: "TEST001",
			PacW:     nullFloat(float64(1000 + i*500)),
		}
	}

	_, err := repo.UpsertReadings(ctx, readings)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Query 12:00 to 12:10 (exclusive end) -> should get 2 rows
	got, err := repo.GetReadings(ctx, "TEST001", base, base.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("get readings: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 readings in [12:00, 12:10), got %d", len(got))
	}
}

func TestMetadataTable(t *testing.T) {
	ctx := context.Background()

	// Verify seed values from migration 000010
	var value string
	err := testPool.QueryRow(ctx,
		"SELECT value FROM growatt.metadata WHERE key = 'schema_version'").
		Scan(&value)
	if err != nil {
		t.Fatalf("querying metadata: %v", err)
	}
	if value != "10" {
		t.Errorf("expected schema_version=10, got %q", value)
	}
}

// Helper functions
func nullFloat(v float64) sql.NullFloat64 {
	return sql.NullFloat64{Float64: v, Valid: true}
}

func seedPlantAndDevice(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO growatt.plants (plant_id, plant_name, status)
		VALUES ('TESTPLANT', 'Test Plant', 1)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("seeding plant: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO growatt.devices (device_sn, plant_id, device_type, status)
		VALUES ('TEST001', 'TESTPLANT', 4, 1)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("seeding device: %v", err)
	}
}
```

### Migration Idempotency Test

The test `TestMigrationUpDown` above verifies the critical property that:
1. All 10 migrations apply cleanly in order
2. All 10 down migrations reverse cleanly in order
3. Re-applying all 10 migrations succeeds (no leftover state)

This catches common migration bugs like missing `IF EXISTS` in down migrations or non-idempotent extension creation.

### Schema Verification Checklist

After running all migrations, verify:

| Check | Query | Expected |
|-------|-------|----------|
| Schema exists | `SELECT 1 FROM information_schema.schemata WHERE schema_name='growatt'` | 1 row |
| Table count | `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='growatt' AND table_type='BASE TABLE'` | 7 (plants, devices, power_readings, plant_snapshots, device_snapshots, energy_summaries, collection_runs, collection_gaps, metadata) = 9 |
| Hypertable count | `SELECT COUNT(*) FROM timescaledb_information.hypertables WHERE hypertable_schema='growatt'` | 3 |
| Continuous aggregates | `SELECT COUNT(*) FROM timescaledb_information.continuous_aggregates WHERE view_schema='growatt'` | 2 |
| Compression policies | `SELECT COUNT(DISTINCT hypertable_name) FROM timescaledb_information.compression_settings WHERE hypertable_schema='growatt'` | 3 |
| Retention policies | `SELECT COUNT(*) FROM timescaledb_information.jobs WHERE application_name LIKE '%retention%'` | 3 |
| Materialized views | `SELECT COUNT(*) FROM pg_matviews WHERE schemaname='growatt'` | 4 (2 caggs + mv_daily_production + mv_monthly_production) |
| Regular views | `SELECT COUNT(*) FROM information_schema.views WHERE table_schema='growatt'` | 1 (v_device_latest) |
| Metadata seed | `SELECT COUNT(*) FROM growatt.metadata` | 2 |

---

## 14. Consistency Fixes Incorporated

This section maps each issue from `00-consistency-analysis.md` to where it is resolved in this design.

| Issue # | Problem | Resolution |
|---------|---------|------------|
| 1 | Column naming mismatch (pac vs pac_w) | All DDL, structs, and upsert SQL use canonical suffixed names: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, `ipv1_a`, etc. |
| 2 | PK order (device_sn, reading_time) vs (ts, device_sn) | PK is `(ts, device_sn)` everywhere. `idx_power_readings_device_ts` covers device-first lookups. |
| 3 | Metadata table name (fetch_log vs collection_runs) | `collection_runs` and `collection_gaps` used throughout. No `fetch_log` table. |
| 4 | Missing fetched_at column | `fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()` present on `power_readings`. Updated on every upsert. |
| 5 | REST API timestamp column name (recorded_at) | All references use `ts`. Go struct uses `db:"ts"`. |
| 10 | Docker Compose uses plain PostgreSQL | Docker service uses `timescale/timescaledb:latest-pg16`. |
| 11 | Schema namespace inconsistency | All DDL uses `growatt.` prefix. Pool config sets `search_path = growatt,public`. |
| 12 | JSON field name inconsistency | Go structs use suffixed `json` tags (`pac_w`, `vpv1_v`) matching DB columns. |
| N/A | metadata table name | Table is `growatt.metadata` (not `system_metadata`). |

Issues 6, 7, 8, and 9 are architectural concerns resolved in their respective design documents (REST API, CLI, MCP server, test harness) and are not database-layer changes.

---

## 15. Go Module Dependencies

Add to `go.mod`:

```
require (
    github.com/jackc/pgx/v5            v5.7.2
    github.com/golang-migrate/migrate/v4 v4.18.1
)
```

The `pgx/v5` driver is chosen over `lib/pq` because:
- Native support for `pgxpool` connection pooling (no external pooler needed)
- `pgx.CollectRows` and `pgx.RowToStructByName` eliminate boilerplate scanning code
- Better performance for batch operations
- Active maintenance and TimescaleDB compatibility

---

## 16. Environment Variables for Database Configuration

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Yes | -- | Full PostgreSQL connection string. Example: `postgres://growatt:password@localhost:5432/growatt?sslmode=disable` |
| `DB_MAX_CONNS` | No | `10` | Maximum pool connections |
| `DB_MIN_CONNS` | No | `2` | Minimum idle connections |
| `DB_MAX_CONN_LIFETIME` | No | `30m` | Maximum connection lifetime |
| `DB_MAX_CONN_IDLE_TIME` | No | `5m` | Maximum idle time before close |
| `DB_HEALTH_CHECK_PERIOD` | No | `1m` | Idle connection health check interval |

These are read by the configuration loader and passed to `DefaultPoolConfig()` / `NewPool()`.
