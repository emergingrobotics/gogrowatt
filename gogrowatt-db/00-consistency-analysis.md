# Cross-Document Consistency Analysis

## Issues Found

### 1. Column Naming Mismatch: Schema vs Fetcher
- **01-schema**: `power_readings` columns use unit suffixes: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, `ipv1_a`, etc.
- **02-fetcher**: References same table with NO suffixes: `reading_time`, `pac`, `ppv`, `vpv1`, `ipv1`, etc.
- **Resolution**: All documents must use the schema's canonical names: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, etc.

### 2. Primary Key Order Mismatch
- **01-schema**: PK is `(ts, device_sn)` — time-first for TimescaleDB partitioning
- **02-fetcher**: PK is `(device_sn, reading_time)` — device-first
- **Resolution**: Use `(ts, device_sn)` as schema defines. The `idx_power_readings_device_ts` index covers device-first lookups.

### 3. Metadata Table Name Mismatch
- **01-schema**: Uses `collection_runs` and `collection_gaps`
- **02-fetcher**: References `fetch_log` (different table name, different columns)
- **Resolution**: Fetcher must use `collection_runs` from the schema.

### 4. Missing `fetched_at` Column
- **01-schema**: `power_readings` has NO `fetched_at` column
- **02-fetcher**: Relies on `fetched_at` for "latest wins" audit trail
- **Resolution**: Add `fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()` to `power_readings` in the schema.

### 5. REST API Timestamp Column Name
- **03-rest-api**: SQL examples use `recorded_at` — matches neither `ts` (schema) nor `reading_time` (fetcher)
- **Resolution**: Use `ts` consistently.

### 6. MCP Server Architecture — Wrong Data Source
- **06-mcp-server**: Connects directly to Growatt cloud API via `pkg/growatt`
- **User requirement**: All readers go through the REST API; only the fetcher talks to Growatt
- **Resolution**: MCP server must query the REST API (`/api/v1/...`), not the Growatt API directly.

### 7. CLI Tool Direct API Fallback
- **05-cli-tool**: Shows fallback to direct Growatt API calls (`POST device/tlx/tlx_data`)
- **User requirement**: CLI hits the REST interface only
- **Resolution**: Remove direct API fallbacks. CLI is a pure REST API consumer.

### 8. Energy Endpoint Entity Mismatch
- **01-schema**: `energy_summaries` keyed by `plant_id` (not device_sn)
- **03-rest-api**: Has `/devices/{sn}/energy` endpoint
- **Resolution**: Either add `device_sn` to `energy_summaries` or route device energy queries through `mv_daily_production` (which IS per-device). Document this clearly.

### 9. Test Harness REST API Paths
- **07-test-harness**: Uses `/api/power?date=...`
- **03-rest-api**: Defines `/api/v1/devices/{sn}/power?from=...&to=...`
- **Resolution**: Test harness must use the canonical REST API paths from doc 03.

### 10. Docker Compose Missing TimescaleDB
- **07-test-harness**: Uses `postgres:16-alpine`
- **01-schema**: Requires TimescaleDB extension (`CREATE EXTENSION IF NOT EXISTS timescaledb`)
- **Resolution**: Use `timescale/timescaledb:latest-pg16` image.

### 11. Schema Namespace
- **01-schema**: All tables in `growatt.` schema namespace
- **02/03/07**: Don't reference the `growatt.` prefix
- **Resolution**: All documents should consistently use `growatt.` prefix or document that `search_path` is set.

### 12. REST API Field Names in JSON Responses
- **03-rest-api**: JSON uses `pac`, `ppv`, `vpv1` (no unit suffix) in some places, `pac_w`, `vpv1_v` in others
- **Resolution**: Standardize JSON field names. Use suffixed names (`pac_w`, `vpv1_v`) to match DB columns, or document the mapping.
