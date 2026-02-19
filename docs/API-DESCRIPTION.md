# Growatt OpenAPI v1 - Tested Endpoint Results

Tested 2026-02-19 with token-based authentication against `https://openapi.growatt.com/v1/`.

**Device**: MIN 9000TL-X (type 7), serial `RZNCFYC0GU`
**Plant ID**: `10700435`

## Endpoint Status Summary

| Endpoint | Method | Status | Notes |
|----------|--------|--------|-------|
| `plant/list` | GET | **Works** | Returns plant metadata including current_power, total_energy |
| `plant/data` | GET | **Works** | Energy overview: today, monthly, yearly, total, current power |
| `plant/details` | GET | **Works** | Plant metadata, address, arrays, inverters, dataloggers |
| `plant/power` | GET | **Works** | 288 data points (5-min intervals), power in watts, null for no-data |
| `plant/energy` | GET | **Permission denied** | Historical daily/monthly energy aggregation |
| `device/list` | GET | **Works** | Lists devices with type, model, status, last_update_time |
| `device/tlx/tlx_last_data` | POST | **Works** | Rich real-time inverter data (see fields below) |
| `device/tlx/tlx_data_info` | GET | **Permission denied** | Real-time inverter details (alternative to tlx_last_data) |
| `device/tlx/tlx_data` | POST | **Permission denied** | Historical inverter data |
| `device/tlx/tlx_set_info` | GET | **Works** | Returns empty data (inverter settings) |
| `readMinParam` | POST | Requires paramId | Parameter read (not tested beyond discovery) |

## Working Endpoints Detail

### plant/list (GET)

```
GET /v1/plant/list
Header: token: <api_key>
```

Response fields per plant:
- `plant_id` (int)
- `name` (string)
- `country`, `city`, `latitude`, `longitude`
- `current_power` (string, watts)
- `total_energy` (string, kWh)
- `peak_power` (int, kW nameplate)
- `status` (int, 1 = online)
- `create_date` (string, YYYY-MM-DD)
- `user_id` (int)
- `locale` (string)
- `image_url` (string)

### plant/data (GET)

```
GET /v1/plant/data?plant_id={id}
Header: token: <api_key>
```

Response fields:
- `current_power` (float, watts) - 0 when not producing
- `today_energy` (string, kWh)
- `monthly_energy` (string, kWh)
- `yearly_energy` (string, kWh)
- `total_energy` (string, kWh)
- `peak_power_actual` (int, kW nameplate)
- `last_update_time` (string, "YYYY-MM-DD HH:MM:SS")
- `timezone` (string, e.g. "GMT-6")
- `carbon_offset` (string)

### plant/details (GET)

```
GET /v1/plant/details?plant_id={id}
Header: token: <api_key>
```

Metadata-heavy response: address, arrays, inverters, dataloggers, design parameters. Not useful for real-time monitoring.

### plant/power (GET)

```
GET /v1/plant/power?plant_id={id}&date={YYYY-MM-DD}
Header: token: <api_key>
```

Returns 288 data points (5-minute intervals for 24 hours):
- `powers[]` array of `{time, power}` objects
- `time`: "YYYY-MM-DD HH:MM" format
- `power`: float (watts) or null for no-data periods
- `count`: 288
- Data points are NOT sorted by time in the response
- Null values appear for nighttime and future time slots

### device/list (GET)

```
GET /v1/device/list?plant_id={id}
Header: token: <api_key>
```

Response fields per device:
- `device_sn` (string)
- `device_id` (int)
- `type` (int, 7 = MIN/TLX)
- `model` (string)
- `status` (int, 1 = online)
- `last_update_time` (string, "YYYY-MM-DD HH:MM:SS")
- `datalogger_sn` (string)
- `lost` (bool)
- `manufacturer` (string)

### device/tlx/tlx_last_data (POST) -- PRIMARY DATA ENDPOINT

```
POST /v1/device/tlx/tlx_last_data
Header: token: <api_key>
Body: tlx_sn={serial}  (form-encoded)
```

This is the richest endpoint available. Returns comprehensive real-time inverter telemetry.

#### Core Power Fields

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `pac` | float | W | AC power output (total) |
| `pac1` | float | W | AC power phase 1 |
| `ppv` | float | W | Total PV (DC) power |
| `ppv1` | float | W | PV string 1 power |
| `ppv2` | float | W | PV string 2 power |

#### PV String Fields

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `vpv1` | float | V | PV string 1 voltage |
| `vpv2` | float | V | PV string 2 voltage |
| `ipv1` | float | A | PV string 1 current |
| `ipv2` | float | A | PV string 2 current |
| `epv1Today` | float | kWh | PV string 1 energy today |
| `epv2Today` | float | kWh | PV string 2 energy today |
| `epv1Total` | float | kWh | PV string 1 energy total |
| `epv2Total` | float | kWh | PV string 2 energy total |
| `epvTotal` | float | kWh | Total PV energy lifetime |

#### AC / Grid Fields

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `vac1` | float | V | Grid voltage |
| `iac1` | float | A | Grid current |
| `fac` | float | Hz | Grid frequency |
| `pf` | float | - | Power factor (1.0 = unity) |
| `vacRs` | float | V | Grid voltage (redundant?) |

#### Energy Counters

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `eacToday` | float | kWh | AC energy produced today |
| `eacTotal` | float | kWh | AC energy produced lifetime |
| `esystemToday` | float | kWh | System energy today |
| `esystemTotal` | float | kWh | System energy lifetime |
| `eselfToday` | float | kWh | Self-consumed energy today |
| `eselfTotal` | float | kWh | Self-consumed energy lifetime |
| `elocalLoadToday` | float | kWh | Local load energy today |
| `elocalLoadTotal` | float | kWh | Local load energy lifetime |

#### Temperature

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `temp1` | float | C | Temperature sensor 1 (board/ambient) |
| `temp5` | float | C | Temperature sensor 5 (heatsink/IGBT) |
| `temp2` | float | C | Temperature sensor 2 (0 if unused) |
| `temp3` | float | C | Temperature sensor 3 (0 if unused) |
| `temp4` | float | C | Temperature sensor 4 (0 if unused) |

#### Status and Safety

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `status` | int | - | Inverter status code (100 = normal producing) |
| `iso` | int | - | Insulation resistance (ohms) |
| `gfci` | int | - | Ground fault current (mA) |
| `faultType` | int | - | Fault type code (0 = no fault) |
| `faultType1` | int | - | Extended fault code |
| `warnCode` | int | - | Warning code (0 = no warning) |
| `warnCode1` | int | - | Extended warning code |
| `errorText` | string | - | Error description |
| `warnText` | string | - | Warning description |
| `statusText` | string | - | Status description |

#### System Fields

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `serialNum` | string | - | Inverter serial number |
| `dataLogSn` | string | - | Datalogger serial number |
| `time` | string | - | Data timestamp ("YYYY-MM-DD HH:MM:SS") |
| `timeTotal` | float | hours | Total operating hours |
| `realOPPercent` | int | % | Real operating percentage |
| `pBusVoltage` | float | V | DC bus voltage (positive) |
| `nBusVoltage` | float | V | DC bus voltage (negative) |
| `operatingMode` | int | - | Operating mode |
| `lost` | bool | - | Communication lost flag |

#### Battery/Storage Fields (zeros for non-hybrid)

Fields prefixed with `bdc1`, `bdc2`, `bms`, `bat`, `soc` are for hybrid inverters with battery storage. All return 0/empty for pure grid-tie MIN inverters.

#### Unused/Irrelevant Fields

- `ppv3`, `ppv4`, `vpv3`, `vpv4`, `ipv3`, `ipv4`: PV strings 3-4 (0 for 2-string inverters)
- `vac2`, `vac3`, `pac2`, `pac3`, `iac2`, `iac3`: Phases 2-3 (0 for single-phase)
- `eps*`: Emergency power supply fields (0 if no EPS)
- `calendar`: Java calendar object (not useful)
- `debug1`, `debug2`: Internal debug strings

## Authentication

All requests use the `token` HTTP header:

```
token: <api_key>
```

The token is obtained from the Growatt ShinePhone app or OpenAPI portal.

## Rate Limiting

- Minimum recommended delay: 3 seconds between requests
- Error code 10012 with "frequently" in message indicates rate limiting
- The `device/tlx/tlx_last_data` endpoint updates approximately every 5 minutes

## Regional Servers

| Region | Base URL |
|--------|----------|
| Europe/Other | `https://openapi.growatt.com/v1/` |
| North America | `https://openapi-us.growatt.com/v1/` |
| Australia/NZ | `https://openapi-au.growatt.com/v1/` |
| China | `https://openapi-cn.growatt.com/v1/` |

Note: Testing against both `openapi.growatt.com` and `openapi-us.growatt.com` returned the same results for this account (plant located in Mexico).
