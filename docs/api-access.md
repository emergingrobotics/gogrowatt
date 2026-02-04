# GrowWatt API Access Guide

## Inverter: GrowWatt 9kW (ShinePhone App)

---

## REST API Documentation (Direct Access)

### Authentication

**Header Name:** `token`
**Value:** Your API token from ShinePhone

```bash
curl -H "token: $GROWATT_API_KEY" https://openapi.growatt.com/v1/plant/list
```

### Server Endpoints by Region

| Region | Base URL |
|--------|----------|
| Europe/Other | `https://openapi.growatt.com/v1/` |
| North America | `https://openapi-us.growatt.com/v1/` |
| Australia/NZ | `https://openapi-au.growatt.com/v1/` |
| China | `https://openapi-cn.growatt.com/v1/` |

---

## API Endpoints

### Plant Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `plant/list` | GET | List all registered power stations |
| `plant/details?plant_id={id}` | GET | Get station information |
| `plant/data?plant_id={id}` | GET | Energy overview (today/total) |
| `plant/power?plant_id={id}&date={YYYY-MM-DD}` | GET | Power data (5-min intervals) |
| `plant/energy?plant_id={id}&start_date={date}&end_date={date}&time_unit={day|month}` | GET | Historical energy data |

### Device Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `device/list?plant_id={id}` | GET | List devices in a plant |

### MIN Inverter Endpoints (TL-X series)

Your inverter is classified as "MIN" (type 7) in the API.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `device/tlx/tlx_data_info?tlx_sn={serial}` | GET | Current inverter details |
| `device/tlx/tlx_last_data` | POST | Current energy data |
| `device/tlx/tlx_data` | POST | Historical energy data |
| `device/tlx/tlx_set_info?tlx_sn={serial}` | GET | Read inverter settings |
| `readMinParam` | POST | Read inverter parameters |
| `tlxSet` | POST | Write parameters/time segments |

### SPH Inverter Endpoints (Hybrid/Storage)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `device/mix/mix_data_info?mix_sn={serial}` | GET | Current inverter details |
| `device/mix/mix_last_data` | POST | Current energy data |
| `device/mix/mix_data` | POST | Historical energy data |
| `readMixParam` | POST | Read inverter parameters |
| `mixSet` | POST | Write parameters/charge times |

---

## Example API Calls

### List Plants
```bash
curl -H "token: $GROWATT_API_KEY" \
  "https://openapi.growatt.com/v1/plant/list"
```

### Get Plant Energy Overview
```bash
curl -H "token: $GROWATT_API_KEY" \
  "https://openapi.growatt.com/v1/plant/data?plant_id=YOUR_PLANT_ID"
```

### Get Today's Power Production (5-min intervals)
```bash
curl -H "token: $GROWATT_API_KEY" \
  "https://openapi.growatt.com/v1/plant/power?plant_id=YOUR_PLANT_ID&date=2025-02-03"
```

### Get Historical Energy Data
```bash
curl -H "token: $GROWATT_API_KEY" \
  "https://openapi.growatt.com/v1/plant/energy?plant_id=YOUR_PLANT_ID&start_date=2025-01-01&end_date=2025-01-31&time_unit=day"
```

### Get Device List
```bash
curl -H "token: $GROWATT_API_KEY" \
  "https://openapi.growatt.com/v1/device/list?plant_id=YOUR_PLANT_ID"
```

### Get MIN Inverter Details
```bash
curl -H "token: $GROWATT_API_KEY" \
  "https://openapi.growatt.com/v1/device/tlx/tlx_data_info?tlx_sn=YOUR_INVERTER_SERIAL"
```

---

## Response Format

All responses are JSON with this structure:

```json
{
  "error_code": 0,
  "error_msg": "success",
  "data": {
    // Response data varies by endpoint
  }
}
```

### Common Error Codes

| Code | Message | Description |
|------|---------|-------------|
| 0 | success | Request successful |
| 10011 | error_permission_denied | Invalid token or insufficient permissions |
| 10012 | error_plant_not_found | Plant ID not found |

---

## Python Libraries (Optional)

### growatt-public-api (Recommended for Python)

```bash
pip install growatt-public-api
```

```python
import os
from growatt_public_api import GrowattApi

api = GrowattApi(
    token=os.environ["GROWATT_API_KEY"],
    server_url="https://openapi.growatt.com"
)

# List plants
plants = api.plant.list()
plant_id = plants.data.plants[0].plant_id

# Get devices
devices = api.plant.list_devices(plant_id=plant_id)
device_sn = devices.data.devices[0].device_sn

# Get device-specific API
my_device = api.api_for_device(device_sn=device_sn)
details = my_device.details()
energy = my_device.energy()
```

### growattServer (Alternative)

```bash
pip install growattServer
```

```python
import os
from growattServer import GrowattApi, OpenApiV1

# V1 API with token
api = OpenApiV1(token=os.environ["GROWATT_API_KEY"])

# List plants
plants = api.plant_list()

# Get plant energy
energy = api.plant_energy_overview(plant_id="YOUR_PLANT_ID")
```

---

## Available Data Fields

### Plant Data
- `today_energy` - kWh generated today
- `total_energy` - Total kWh generated (lifetime)
- `current_power` - Current power output (W)
- `peak_power_today` - Peak power today (W)

### Inverter Data
- `pac` - Current AC power (W)
- `etoday` - Energy today (kWh)
- `etotal` - Total energy (kWh)
- `vpv1`, `vpv2` - PV string voltages (V)
- `ipv1`, `ipv2` - PV string currents (A)
- `vac1` - AC voltage (V)
- `iac1` - AC current (A)
- `fac` - AC frequency (Hz)
- `temperature` - Inverter temperature (°C)
- `status` - Operating status code

---

## Rate Limits

- **V1 API (token-based)**: Relaxed rate limits, preferred method
- **Legacy API (password-based)**: Strict limits, can lock account for 24 hours

---

## Local Alternative - Grott

If you want to avoid cloud API entirely, [Grott](https://github.com/johanmeijer/grott) intercepts data from your Shine WiFi stick locally.

---

## References

- [GrowattPublicApiPy](https://github.com/timohencken/GrowattPublicApiPy) - Python REST API implementation
- [growattServer on PyPI](https://pypi.org/project/growattServer/) - Python library
- [Home Assistant Growatt Integration](https://www.home-assistant.io/integrations/growatt_server/)
- [Growatt Official API Portal](https://openapi.growatt.com/)
