# Growatt Prometheus Exporter Design

## Overview

A service that periodically fetches solar production data from Growatt inverters and exports it to Prometheus for monitoring and visualization in Grafana.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                        growatt-exporter                              │
│  ┌─────────────┐   ┌─────────────┐   ┌──────────────────────────┐  │
│  │  Scheduler  │──▶│  Collector  │──▶│  Prometheus Pushgateway  │  │
│  │  (daily)    │   │             │   │  or /metrics endpoint    │  │
│  └─────────────┘   └─────────────┘   └──────────────────────────┘  │
│         │                │                       │                  │
│         │                ▼                       │                  │
│         │         ┌─────────────┐               │                  │
│         │         │  pkg/growatt │               │                  │
│         │         │  (API client)│               │                  │
│         │         └─────────────┘               │                  │
└─────────┼───────────────────────────────────────┼──────────────────┘
          │                                        │
          ▼                                        ▼
┌─────────────────┐                    ┌─────────────────────┐
│  Growatt API    │                    │  Prometheus         │
│  openapi.growatt│                    │  ─────────────────  │
│  .com/v1/       │                    │  Pushgateway or     │
└─────────────────┘                    │  Scrape Target      │
                                       └─────────────────────┘
                                                 │
                                                 ▼
                                       ┌─────────────────────┐
                                       │      Grafana        │
                                       └─────────────────────┘
```

## Push vs Pull Model

### Option A: Pushgateway (Recommended for Daily Collection)

Best for batch jobs that run periodically and then exit.

```
growatt-exporter ──push──▶ Pushgateway ◀──scrape── Prometheus
```

**Pros:**
- Natural fit for periodic/batch data collection
- Exporter doesn't need to be long-running
- Metrics persist between collection runs
- Can run as cron job or systemd timer

**Cons:**
- Requires Pushgateway component
- Metrics are "stale" between pushes

### Option B: Standard HTTP Exporter (For Real-time Monitoring)

Long-running process that Prometheus scrapes.

```
Prometheus ──scrape──▶ growatt-exporter:9120/metrics ──▶ Growatt API
```

**Pros:**
- Standard Prometheus pattern
- Real-time data on each scrape
- No additional components

**Cons:**
- Must be long-running
- API rate limits may conflict with frequent scrapes

### Recommendation

Support **both modes**:
- Default: HTTP exporter with configurable scrape interval (5-15 minutes)
- Optional: Push mode for daily batch collection or constrained environments

## Metrics Specification

### Gauge Metrics (Instantaneous Values)

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `growatt_current_power_watts` | Gauge | plant_id, device_sn | Current AC power output |
| `growatt_inverter_temperature_celsius` | Gauge | plant_id, device_sn | Inverter temperature |
| `growatt_pv1_voltage_volts` | Gauge | plant_id, device_sn | PV string 1 voltage |
| `growatt_pv2_voltage_volts` | Gauge | plant_id, device_sn | PV string 2 voltage |
| `growatt_pv1_current_amps` | Gauge | plant_id, device_sn | PV string 1 current |
| `growatt_pv2_current_amps` | Gauge | plant_id, device_sn | PV string 2 current |
| `growatt_pv_power_watts` | Gauge | plant_id, device_sn | Total PV input power |
| `growatt_ac_voltage_volts` | Gauge | plant_id, device_sn | AC output voltage |
| `growatt_ac_current_amps` | Gauge | plant_id, device_sn | AC output current |
| `growatt_ac_frequency_hz` | Gauge | plant_id, device_sn | AC frequency |
| `growatt_inverter_status` | Gauge | plant_id, device_sn | Status (1=normal, 0=offline) |
| `growatt_plant_status` | Gauge | plant_id | Plant status |

### Counter Metrics (Cumulative Values)

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `growatt_energy_today_kwh` | Gauge* | plant_id, device_sn | Energy generated today |
| `growatt_energy_total_kwh` | Counter | plant_id, device_sn | Total lifetime energy |
| `growatt_energy_month_kwh` | Gauge* | plant_id | Energy generated this month |
| `growatt_energy_year_kwh` | Gauge* | plant_id | Energy generated this year |

*Note: `energy_today/month/year` are Gauges because they reset periodically.

### Info Metrics (Metadata)

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `growatt_plant_info` | Gauge(1) | plant_id, plant_name, location, peak_power_kw | Plant metadata |
| `growatt_device_info` | Gauge(1) | device_sn, device_type, model, plant_id | Device metadata |

### Historical/Aggregated Metrics (Daily Collection)

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `growatt_daily_energy_kwh` | Gauge | plant_id, date | Energy for specific date |
| `growatt_daily_peak_power_watts` | Gauge | plant_id, date | Peak power for date |
| `growatt_hourly_avg_power_watts` | Gauge | plant_id, hour | Average power by hour |

### Example Metric Output

```prometheus
# HELP growatt_current_power_watts Current AC power output in watts
# TYPE growatt_current_power_watts gauge
growatt_current_power_watts{plant_id="12345",device_sn="ABC123"} 4523.5

# HELP growatt_energy_total_kwh Total lifetime energy generated
# TYPE growatt_energy_total_kwh counter
growatt_energy_total_kwh{plant_id="12345",device_sn="ABC123"} 15234.8

# HELP growatt_inverter_temperature_celsius Inverter temperature
# TYPE growatt_inverter_temperature_celsius gauge
growatt_inverter_temperature_celsius{plant_id="12345",device_sn="ABC123"} 42.5

# HELP growatt_plant_info Plant metadata
# TYPE growatt_plant_info gauge
growatt_plant_info{plant_id="12345",plant_name="Home Solar",location="37.7749,-122.4194",peak_power_kw="10.8"} 1
```

## Implementation

### Directory Structure

```
cmd/
└── growatt-exporter/
    └── main.go              # CLI entry point
pkg/
├── growatt/                 # Existing API client (reuse)
└── exporter/
    ├── exporter.go          # Main exporter logic
    ├── collector.go         # Prometheus collector implementation
    ├── metrics.go           # Metric definitions
    └── scheduler.go         # Collection scheduling
```

### Core Components

#### 1. Collector

Implements `prometheus.Collector` interface:

```go
type GrowattCollector struct {
    client      *growatt.Client
    plantIDs    []string
    deviceSNs   []string

    // Metric descriptors
    currentPower    *prometheus.Desc
    temperature     *prometheus.Desc
    pvVoltage       *prometheus.Desc
    // ... etc
}

func (c *GrowattCollector) Describe(ch chan<- *prometheus.Desc) {
    ch <- c.currentPower
    ch <- c.temperature
    // ...
}

func (c *GrowattCollector) Collect(ch chan<- prometheus.Metric) {
    // Fetch data from Growatt API
    // Convert to Prometheus metrics
    // Send to channel
}
```

#### 2. HTTP Server Mode

```go
func runHTTPServer(addr string, collector *GrowattCollector) {
    registry := prometheus.NewRegistry()
    registry.MustRegister(collector)

    http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
    http.ListenAndServe(addr, nil)
}
```

#### 3. Push Mode

```go
func runPushMode(pushgatewayURL string, collector *GrowattCollector) {
    registry := prometheus.NewRegistry()
    registry.MustRegister(collector)

    pusher := push.New(pushgatewayURL, "growatt_exporter").
        Gatherer(registry)

    if err := pusher.Push(); err != nil {
        log.Fatalf("Push failed: %v", err)
    }
}
```

#### 4. Scheduler (for background collection)

```go
type Scheduler struct {
    collector *GrowattCollector
    interval  time.Duration

    // For historical data collection
    collectHistory bool
    historyDays    int
}

func (s *Scheduler) Run(ctx context.Context) {
    ticker := time.NewTicker(s.interval)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            s.collect()
        case <-ctx.Done():
            return
        }
    }
}
```

### Data Flow

#### Real-time Collection (Scrape Mode)

```
1. Prometheus scrapes /metrics endpoint
2. Collector.Collect() called
3. For each configured plant:
   a. Call plant/data for current power/energy
   b. Call device/list to get devices
   c. For each device:
      - Call device/tlx/tlx_data_info for current readings
4. Convert API responses to prometheus.Metric
5. Return metrics to Prometheus
```

#### Historical Collection (Daily Batch)

```
1. Scheduler triggers at configured time (e.g., 00:05 daily)
2. For previous day:
   a. Fetch 5-minute power data via plant/power or device/tlx/tlx_data
   b. Calculate daily statistics (peak, total energy, hourly averages)
   c. Push to Pushgateway with date labels
3. Optionally: backfill missing days on startup
```

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GROWATT_TOKEN` | (required) | API authentication token |
| `GROWATT_REGION` | `eu` | API region (eu, us, au, cn) |
| `GROWATT_PLANT_ID` | (auto-detect) | Plant ID to monitor |
| `GROWATT_DEVICE_SN` | (all devices) | Specific device serial number |
| `EXPORTER_MODE` | `http` | Mode: `http` or `push` |
| `EXPORTER_LISTEN_ADDR` | `:9120` | HTTP server address |
| `PUSHGATEWAY_URL` | `http://localhost:9091` | Pushgateway URL for push mode |
| `COLLECT_INTERVAL` | `5m` | Collection interval (http mode) |
| `COLLECT_HISTORY` | `false` | Collect historical daily data |
| `HISTORY_DAYS` | `7` | Days of history to backfill |

### Config File (Optional)

```yaml
# growatt-exporter.yaml
token: ${GROWATT_TOKEN}
region: eu

plants:
  - id: "12345"
    devices:
      - sn: "ABC123"
      - sn: "DEF456"

exporter:
  mode: http
  listen_addr: ":9120"
  collect_interval: 5m

history:
  enabled: true
  days: 30
  schedule: "0 5 * * *"  # 00:05 daily
```

## Rate Limiting Considerations

The Growatt API has rate limits. The exporter must respect these:

- **Minimum 3 seconds** between API calls (already implemented in pkg/growatt)
- **Recommended scrape interval**: 5+ minutes
- **Multiple plants/devices**: Serial collection with delays

### Calculation

For 1 plant with 2 devices:
- `plant/data`: 1 call
- `device/tlx/tlx_data_info`: 2 calls
- Total: 3 calls × 3s = 9 seconds minimum

For 5 plants with 2 devices each:
- 5 × (1 + 2) = 15 calls × 3s = 45 seconds minimum

**Recommendation**: Set scrape interval ≥ (num_api_calls × 3s) + buffer

## Deployment

### Docker

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o growatt-exporter ./cmd/growatt-exporter

FROM alpine:latest
COPY --from=builder /app/growatt-exporter /usr/local/bin/
EXPOSE 9120
ENTRYPOINT ["growatt-exporter"]
```

### Docker Compose

```yaml
version: '3.8'

services:
  growatt-exporter:
    build: .
    environment:
      - GROWATT_TOKEN=${GROWATT_TOKEN}
      - GROWATT_REGION=eu
      - EXPORTER_MODE=http
    ports:
      - "9120:9120"

  prometheus:
    image: prom/prometheus
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
    ports:
      - "9090:9090"

  pushgateway:
    image: prom/pushgateway
    ports:
      - "9091:9091"

  grafana:
    image: grafana/grafana
    ports:
      - "3000:3000"
    volumes:
      - grafana-data:/var/lib/grafana

volumes:
  grafana-data:
```

### Prometheus Configuration

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'growatt'
    scrape_interval: 5m
    scrape_timeout: 60s
    static_configs:
      - targets: ['growatt-exporter:9120']

  # If using pushgateway
  - job_name: 'pushgateway'
    honor_labels: true
    static_configs:
      - targets: ['pushgateway:9091']
```

### Systemd Service (for push mode)

```ini
# /etc/systemd/system/growatt-exporter.service
[Unit]
Description=Growatt Prometheus Exporter
After=network.target

[Service]
Type=oneshot
Environment=GROWATT_TOKEN=xxx
Environment=EXPORTER_MODE=push
Environment=PUSHGATEWAY_URL=http://localhost:9091
ExecStart=/usr/local/bin/growatt-exporter

[Install]
WantedBy=multi-user.target
```

```ini
# /etc/systemd/system/growatt-exporter.timer
[Unit]
Description=Run Growatt Exporter daily

[Timer]
OnCalendar=*-*-* 00:05:00
Persistent=true

[Install]
WantedBy=timers.target
```

## Grafana Dashboard

### Suggested Panels

1. **Current Power** - Single stat with spark line
2. **Daily Energy** - Time series graph
3. **Monthly Energy Comparison** - Bar chart by month
4. **Inverter Status** - Status indicator (green/red)
5. **Temperature** - Gauge with warning thresholds
6. **PV String Comparison** - Dual line chart (PV1 vs PV2)
7. **AC Voltage/Frequency** - Time series with threshold lines
8. **Energy Heatmap** - Hour of day vs day of week
9. **Production Forecast vs Actual** - If weather integration added

### Example Queries

```promql
# Current power
growatt_current_power_watts{plant_id="12345"}

# Energy today
growatt_energy_today_kwh{plant_id="12345"}

# Power over time (5m rate)
rate(growatt_energy_total_kwh{plant_id="12345"}[5m]) * 1000

# Average power by hour of day
avg by (hour) (
  growatt_hourly_avg_power_watts{plant_id="12345"}
)

# Temperature alert query
growatt_inverter_temperature_celsius > 55
```

## Error Handling

### API Errors

| Error Code | Meaning | Action |
|------------|---------|--------|
| 10011 | Permission denied | Log error, skip plant/device |
| 10012 | Not found | Log warning, remove from config |
| Rate limit | Too many requests | Back off, increase interval |
| Network | Connection failed | Retry with exponential backoff |

### Metric Staleness

When API fails:
- HTTP mode: Return last known values with `up` metric = 0
- Push mode: Don't push, let existing metrics go stale

```prometheus
# HELP growatt_up Whether the last collection succeeded
# TYPE growatt_up gauge
growatt_up{plant_id="12345"} 1
```

## Future Enhancements

1. **Weather Integration** - Correlate production with weather data
2. **Alertmanager Rules** - Pre-built alert configurations
3. **Multi-tenant** - Support multiple Growatt accounts
4. **Battery Metrics** - For SPH inverters with battery storage
5. **Grid Export/Import** - For systems with smart meters
6. **Cost Calculations** - Energy value based on tariff rates

## Summary

The growatt-exporter will:

1. **Reuse existing `pkg/growatt`** - All API logic already implemented
2. **Support dual modes** - HTTP scrape (real-time) and Push (batch)
3. **Export rich metrics** - Power, energy, voltage, temperature, status
4. **Handle rate limits** - Built into existing client
5. **Deploy easily** - Docker, systemd, or standalone binary
6. **Integrate with Grafana** - Standard Prometheus metrics format

Estimated implementation effort: The core exporter leveraging existing code.
