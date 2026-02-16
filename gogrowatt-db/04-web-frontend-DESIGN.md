# GoGrowatt Web Frontend -- Detailed Implementation Design

This document is the implementation-level design for the GoGrowatt web frontend. It is derived from the spec in `04-web-frontend.md`, incorporates all consistency fixes from `00-consistency-analysis.md`, and aligns precisely with the REST API defined in `03-rest-api.md`.

**Consistency resolutions applied throughout this document:**
- All field names use unit-suffixed DB column names: `ts`, `pac_w`, `ppv_w`, `vpv1_v`, `ipv1_a`, etc. (issue #1, #12)
- The frontend is a pure REST API consumer -- it never contacts the Growatt cloud API directly (issues #6, #7)
- All REST API paths use the canonical `/api/v1/...` routes from doc 03 (issue #9)
- JSON response field names consistently use suffixed form: `pac_w`, `vpv1_v`, `vac1_v`, etc. (issue #12)
- The `ts` column name from the schema is represented as `time` in JSON responses per doc 03 (issue #5)

---

## 1. Project Structure

```
web/
  index.html                          # Vite entry HTML
  vite.config.ts                      # Vite build configuration
  tailwind.config.ts                  # Tailwind CSS configuration
  postcss.config.js                   # PostCSS (required by Tailwind)
  tsconfig.json                       # TypeScript configuration
  tsconfig.node.json                  # TS config for Vite/Node context
  .env                                # Default env vars (VITE_API_BASE_URL)
  .env.development                    # Dev overrides
  .env.production                     # Prod overrides
  package.json
  src/
    main.tsx                          # Entry: ReactDOM.createRoot, router, theme init
    App.tsx                           # Layout shell + React Router <Outlet />
    vite-env.d.ts                     # Vite client type declarations

    api/
      client.ts                       # Typed fetch wrapper, envelope unwrapping
      types.ts                        # All TypeScript interfaces matching REST API JSON
      endpoints.ts                    # Endpoint-specific functions (re-exports from client)

    store/
      index.ts                        # Combined Zustand store (all slices)
      slices/
        plantSlice.ts                 # Plant selection, plant list
        liveSlice.ts                  # Auto-refresh state, current power
        powerSlice.ts                 # Power time-series cache
        energySlice.ts                # Energy data cache
        statsSlice.ts                 # Statistics data
        uiSlice.ts                    # Dark mode, sidebar, toasts

    hooks/
      useAutoRefresh.ts               # setInterval with visibility API
      usePowerData.ts                 # Fetch + cache power time-series
      useEnergyData.ts                # Fetch + cache energy data
      useStatsData.ts                 # Fetch stats data
      useDeviceData.ts                # Fetch device detail + latest reading
      useTimezone.ts                  # Browser timezone detection + override

    components/
      layout/
        Layout.tsx                    # Shell: sidebar + topbar + outlet
        Sidebar.tsx                   # Navigation links, responsive collapse
        TopBar.tsx                    # Plant selector, refresh toggle, dark mode

      shared/
        ChartWrapper.tsx              # ECharts container, resize, theme switching
        ExportCSVButton.tsx           # CSV download trigger
        DateRangePicker.tsx           # From/to date inputs
        IntervalSelector.tsx          # 5min | 15min | 1h | 1d radio group
        LoadingSkeleton.tsx           # Shimmer placeholder
        EmptyState.tsx                # "No data" illustration + message
        ErrorBoundary.tsx             # React error boundary with fallback UI
        RefreshIndicator.tsx          # Countdown ring / "refreshing..." text
        Toast.tsx                     # Notification toast component

      dashboard/
        LivePowerGauge.tsx            # ECharts gauge for current watts
        TodayPowerCurve.tsx           # Area line chart for today's pac_w
        SummaryCards.tsx              # today_energy_kwh, total_energy_kwh, peak
        MiniEnergyCalendar.tsx        # 7-day energy bar mini chart

      historical/
        PowerTimeSeriesChart.tsx      # Full zoomable power chart
        EnergyBarChart.tsx            # Daily/monthly energy bars

      comparison/
        MultiDayPicker.tsx            # Add/remove dates for overlay
        OverlayTimeChart.tsx          # N-series overlay on shared time axis
        DifferenceTable.tsx           # Peak, total, peak-hour per selected day

      statistics/
        HourlyHeatmap.tsx             # Days x hours heatmap
        HourlyBoxPlots.tsx            # Box plot per active hour
        MonthlyEnergyBars.tsx         # Month-level energy bars
        StatsSummaryTable.tsx         # Tabular hour stats

      device/
        InverterInfoCard.tsx          # SN, model, status card
        StringVoltageCurrentChart.tsx  # Dual-axis vpv/ipv chart
        GridChart.tsx                 # vac1_v / iac1_a line chart
        TemperatureChart.tsx          # temperature_c with warning zones

    pages/
      DashboardPage.tsx               # Composes dashboard components
      HistoricalPage.tsx              # Composes historical components
      ComparisonPage.tsx              # Composes comparison components
      StatisticsPage.tsx              # Composes statistics components
      DeviceDetailPage.tsx            # Composes device components

    utils/
      csv.ts                          # CSV generation + download
      format.ts                       # Number/date formatting helpers
      time.ts                         # Time slot generation, gap detection
      constants.ts                    # System capacity, color palettes

    themes/
      echarts-light.ts                # ECharts light theme registration
      echarts-dark.ts                 # ECharts dark theme registration
      index.ts                        # Theme init (registers both at startup)
```

---

## 2. React 18 + Vite + TypeScript Setup

### 2.1 vite.config.ts

```typescript
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'path';

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: true,
    rollupOptions: {
      output: {
        manualChunks: {
          echarts: ['echarts'],
          react: ['react', 'react-dom'],
          router: ['react-router-dom'],
        },
      },
    },
  },
});
```

### 2.2 tsconfig.json

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "module": "ESNext",
    "moduleResolution": "bundler",
    "jsx": "react-jsx",
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noFallthroughCasesInSwitch": true,
    "allowImportingTsExtensions": true,
    "noEmit": true,
    "isolatedModules": true,
    "skipLibCheck": true,
    "paths": {
      "@/*": ["./src/*"]
    },
    "baseUrl": "."
  },
  "include": ["src"],
  "references": [{ "path": "./tsconfig.node.json" }]
}
```

### 2.3 package.json (dependencies)

```json
{
  "name": "gogrowatt-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc && vite build",
    "preview": "vite preview",
    "lint": "eslint src --ext .ts,.tsx",
    "test": "vitest",
    "test:ui": "vitest --ui",
    "test:coverage": "vitest run --coverage"
  },
  "dependencies": {
    "react": "^18.3.0",
    "react-dom": "^18.3.0",
    "react-router-dom": "^6.23.0",
    "echarts": "^5.5.0",
    "echarts-for-react": "^3.0.2",
    "zustand": "^4.5.0",
    "date-fns": "^3.6.0",
    "date-fns-tz": "^3.1.0"
  },
  "devDependencies": {
    "@types/react": "^18.3.0",
    "@types/react-dom": "^18.3.0",
    "@vitejs/plugin-react": "^4.3.0",
    "autoprefixer": "^10.4.0",
    "postcss": "^8.4.0",
    "tailwindcss": "^3.4.0",
    "typescript": "^5.4.0",
    "vite": "^5.4.0",
    "vitest": "^1.6.0",
    "@testing-library/react": "^15.0.0",
    "@testing-library/jest-dom": "^6.4.0",
    "@testing-library/user-event": "^14.5.0",
    "msw": "^2.3.0",
    "jsdom": "^24.0.0"
  }
}
```

### 2.4 Entry Point: src/main.tsx

```tsx
import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { App } from './App';
import { initThemes } from './themes';
import './index.css'; // Tailwind directives

// Register ECharts themes before first render
initThemes();

// Restore dark mode from localStorage
const savedDark = localStorage.getItem('growatt-dark-mode') === 'true';
if (savedDark) {
  document.documentElement.classList.add('dark');
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
);
```

---

## 3. TypeScript Types (api/types.ts)

All interfaces match the REST API JSON field names from doc 03 exactly. Unit-suffixed field names (`pac_w`, `vpv1_v`, etc.) are used throughout, consistent with the database schema (doc 01) and the consistency resolution (doc 00, issues #1 and #12).

```typescript
// src/api/types.ts

// ============================================================
// Response Envelope
// ============================================================

export interface ApiEnvelope<T> {
  data: T;
  pagination?: Pagination;
  meta: Meta;
}

export interface Pagination {
  page: number;
  per_page: number;
  total: number;
  total_pages: number;
}

export interface Meta {
  timestamp: string;   // ISO 8601
  request_id: string;  // e.g., "req_abc123"
}

export interface ApiErrorBody {
  error: {
    code: string;      // e.g., "VALIDATION_ERROR", "NOT_FOUND", "RATE_LIMITED"
    message: string;
    details?: unknown;
  };
  meta: Meta;
}

// ============================================================
// Plant
// ============================================================

/** Matches GET /api/v1/plants response items */
export interface PlantListItem {
  id: string;
  name: string;
  country: string;
  city: string;
  latitude: number;
  longitude: number;
  peak_power_kw: number;
  status: string;              // "online" | "offline" | "standby"
  current_power_w: number;
  today_energy_kwh: number;
  total_energy_kwh: number;
  created_at: string;          // ISO 8601
  device_count: number;
}

/** Matches GET /api/v1/plants/{id} response */
export interface PlantDetail {
  id: string;
  name: string;
  country: string;
  city: string;
  latitude: number;
  longitude: number;
  peak_power_kw: number;
  status: string;
  current_power_w: number;
  today_energy_kwh: number;
  month_energy_kwh: number;
  year_energy_kwh: number;
  total_energy_kwh: number;
  peak_power_today_w: number;
  formula_coal_kg: number;
  formula_co2_kg: number;
  formula_trees: number;
  money_saved: number;
  money_unit: string;          // "USD", "EUR", etc.
  created_at: string;
  devices: DeviceListItem[];
}

// ============================================================
// Device
// ============================================================

/** Matches device items in plant detail and /plants/{id}/devices */
export interface DeviceListItem {
  serial_number: string;
  name: string;
  type: string;                // "inverter"
  model: string;               // "MIN 6000TL-XH"
  status: string;              // "online" | "offline"
  last_update: string;         // ISO 8601 with tz offset
}

/** Matches GET /api/v1/devices/{sn} response */
export interface DeviceDetail {
  serial_number: string;
  plant_id: string;
  name: string;
  type: string;
  model: string;
  status: string;
  last_update: string;
  current: DeviceCurrentReadings;
}

export interface DeviceCurrentReadings {
  pac_w: number;               // AC output power (watts)
  ppv_w: number;               // DC PV input power (watts)
  vpv1_v: number;              // PV string 1 voltage (volts)
  vpv2_v: number;              // PV string 2 voltage (volts)
  ipv1_a: number;              // PV string 1 current (amps)
  ipv2_a: number;              // PV string 2 current (amps)
  vac1_v: number;              // Grid AC voltage (volts)
  iac1_a: number;              // Grid AC current (amps)
  frequency_hz: number;        // Grid frequency (Hz)
  temperature_c: number;       // Inverter temperature (Celsius)
  today_energy_kwh: number;
  total_energy_kwh: number;
}

// ============================================================
// Power Time-Series
// ============================================================

/** Matches GET /api/v1/devices/{sn}/power data payload */
export interface PowerResponse {
  serial_number: string;
  from: string;
  to: string;
  interval: PowerInterval;
  timezone: string;
  fields: PowerField[];
  readings: PowerReading[];
}

export type PowerInterval = '5min' | '15min' | '1h' | '1d';

export type PowerField =
  | 'pac_w'
  | 'ppv_w'
  | 'vpv1_v'
  | 'vpv2_v'
  | 'ipv1_a'
  | 'ipv2_a'
  | 'vac1_v'
  | 'iac1_a';

/**
 * A single power reading.
 *
 * At interval=5min, fields are raw numbers (e.g., pac_w: 5120.5).
 * At interval=15min/1h/1d, fields are AggregatedFieldValue objects
 * (e.g., pac_w: { avg: 5120.5, min: 4900.0, max: 5300.0, samples: 12 }).
 *
 * The union type handles both shapes.
 */
export interface PowerReading {
  time: string;                // ISO 8601 with tz offset
  pac_w?: number | AggregatedFieldValue;
  ppv_w?: number | AggregatedFieldValue;
  vpv1_v?: number | AggregatedFieldValue;
  vpv2_v?: number | AggregatedFieldValue;
  ipv1_a?: number | AggregatedFieldValue;
  ipv2_a?: number | AggregatedFieldValue;
  vac1_v?: number | AggregatedFieldValue;
  iac1_a?: number | AggregatedFieldValue;
}

export interface AggregatedFieldValue {
  avg: number;
  min: number;
  max: number;
  samples: number;
}

// ============================================================
// Latest Reading
// ============================================================

/** Matches GET /api/v1/devices/{sn}/power/latest data payload */
export interface LatestReading {
  serial_number: string;
  time: string;
  pac_w: number;
  ppv_w: number;
  vpv1_v: number;
  vpv2_v: number;
  ipv1_a: number;
  ipv2_a: number;
  vac1_v: number;
  iac1_a: number;
}

// ============================================================
// Energy
// ============================================================

/** Matches GET /api/v1/devices/{sn}/energy data payload */
export interface EnergyResponse {
  serial_number: string;
  from: string;
  to: string;
  unit: 'day' | 'month';
  timezone: string;
  totals: EnergyDataPoint[];
  summary: EnergySummary;
}

export interface EnergyDataPoint {
  date: string;                // "2026-02-15" (day) or "2026-02" (month)
  energy_kwh: number;
}

export interface EnergySummary {
  total_kwh: number;
  average_kwh: number;
  max_kwh?: number;
  max_date?: string;
  min_kwh?: number;
  min_date?: string;
  days_with_data?: number;     // present when unit=day
  months_with_data?: number;   // present when unit=month
}

// ============================================================
// Statistics
// ============================================================

/** Matches GET /api/v1/devices/{sn}/stats data payload */
export interface MultiDayStats {
  serial_number: string;
  from: string;
  to: string;
  field: PowerField;
  timezone: string;
  days_analyzed: number;
  total_production_kwh: number;
  daily_average_kwh: number;
  peak_hour: number;           // 0-23
  peak_power_avg_w: number;
  by_hour: AggregatedHourStats[];
}

export interface AggregatedHourStats {
  hour: number;                // 0-23
  min_w: number;
  max_w: number;
  avg_w: number;
  median_w: number;
  stddev_w: number;
  sample_days: number;
}

// ============================================================
// Health
// ============================================================

export interface HealthResponse {
  status: 'healthy' | 'degraded';
  version: string;
  uptime_seconds: number;
  database: {
    connected: boolean;
    latency_ms: number;
  };
  data_freshness: {
    latest_reading: string;
    age_seconds: number;
    is_stale: boolean;
    stale_threshold_seconds: number;
  } | null;
}

// ============================================================
// Utility types for component props
// ============================================================

/** Type guard: is this field value raw or aggregated? */
export function isAggregated(
  value: number | AggregatedFieldValue | undefined,
): value is AggregatedFieldValue {
  return typeof value === 'object' && value !== null && 'avg' in value;
}

/** Extract the display value from a possibly-aggregated field */
export function fieldValue(
  value: number | AggregatedFieldValue | undefined,
): number {
  if (value === undefined) return 0;
  if (isAggregated(value)) return value.avg;
  return value;
}
```

---

## 4. API Client Module (api/client.ts)

The client wraps the browser `fetch` API, unwraps the standard `{ data, pagination?, meta }` envelope from doc 03, and provides typed endpoint functions for every route.

```typescript
// src/api/client.ts

import type {
  ApiEnvelope,
  ApiErrorBody,
  Pagination,
  PlantListItem,
  PlantDetail,
  DeviceListItem,
  DeviceDetail,
  PowerResponse,
  PowerInterval,
  PowerField,
  LatestReading,
  EnergyResponse,
  MultiDayStats,
  HealthResponse,
} from './types';

// ============================================================
// Configuration
// ============================================================

const BASE_URL = import.meta.env.VITE_API_BASE_URL ?? '/api/v1';

// ============================================================
// Custom Error Class
// ============================================================

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
    public readonly details?: unknown,
    public readonly requestId?: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

// ============================================================
// Core Fetch Helpers
// ============================================================

interface FetchOptions {
  signal?: AbortSignal;
}

/**
 * Low-level fetch that returns the full envelope.
 * All callers should use the typed wrappers below.
 */
async function rawFetch<T>(path: string, opts?: FetchOptions): Promise<ApiEnvelope<T>> {
  const url = `${BASE_URL}${path}`;
  const res = await fetch(url, {
    headers: { Accept: 'application/json' },
    signal: opts?.signal,
  });

  if (!res.ok) {
    let code = 'UNKNOWN_ERROR';
    let message = `HTTP ${res.status}`;
    let details: unknown;
    let requestId: string | undefined;

    try {
      const body: ApiErrorBody = await res.json();
      code = body.error.code;
      message = body.error.message;
      details = body.error.details;
      requestId = body.meta.request_id;
    } catch {
      // Response body was not JSON; use defaults
    }

    throw new ApiError(res.status, code, message, details, requestId);
  }

  return res.json() as Promise<ApiEnvelope<T>>;
}

/** Unwrap envelope, return just the data payload. */
async function apiFetch<T>(path: string, opts?: FetchOptions): Promise<T> {
  const envelope = await rawFetch<T>(path, opts);
  return envelope.data;
}

/** Unwrap envelope, return data + pagination. */
async function apiFetchPaginated<T>(
  path: string,
  opts?: FetchOptions,
): Promise<{ data: T; pagination: Pagination | undefined }> {
  const envelope = await rawFetch<T>(path, opts);
  return { data: envelope.data, pagination: envelope.pagination };
}

// ============================================================
// Query String Builder
// ============================================================

function buildQuery(params: Record<string, string | undefined>): string {
  const qs = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== '') {
      qs.set(key, value);
    }
  }
  const str = qs.toString();
  return str ? `?${str}` : '';
}

// ============================================================
// Typed Endpoint Functions
// ============================================================

// --- Health ---

export function fetchHealth(opts?: FetchOptions): Promise<HealthResponse> {
  return apiFetch<HealthResponse>('/health', opts);
}

// --- Plants ---

export interface ListPlantsParams {
  page?: number;
  per_page?: number;
}

export function fetchPlants(params?: ListPlantsParams, opts?: FetchOptions) {
  const qs = buildQuery({
    page: params?.page?.toString(),
    per_page: params?.per_page?.toString(),
  });
  return apiFetchPaginated<PlantListItem[]>(`/plants${qs}`, opts);
}

export function fetchPlant(id: string, opts?: FetchOptions): Promise<PlantDetail> {
  return apiFetch<PlantDetail>(`/plants/${id}`, opts);
}

export function fetchPlantDevices(
  plantId: string,
  params?: ListPlantsParams,
  opts?: FetchOptions,
) {
  const qs = buildQuery({
    page: params?.page?.toString(),
    per_page: params?.per_page?.toString(),
  });
  return apiFetchPaginated<DeviceListItem[]>(
    `/plants/${plantId}/devices${qs}`,
    opts,
  );
}

// --- Devices ---

export function fetchDevice(sn: string, opts?: FetchOptions): Promise<DeviceDetail> {
  return apiFetch<DeviceDetail>(`/devices/${sn}`, opts);
}

// --- Power ---

export interface PowerQueryParams {
  from: string;               // YYYY-MM-DD or RFC 3339
  to: string;
  interval?: PowerInterval;
  fields?: PowerField[];      // becomes comma-separated
  tz?: string;                // IANA timezone
  page?: number;
  per_page?: number;
}

export function fetchDevicePower(
  sn: string,
  params: PowerQueryParams,
  opts?: FetchOptions,
) {
  const qs = buildQuery({
    from: params.from,
    to: params.to,
    interval: params.interval,
    fields: params.fields?.join(','),
    tz: params.tz,
    page: params.page?.toString(),
    per_page: params.per_page?.toString(),
  });
  return apiFetchPaginated<PowerResponse>(`/devices/${sn}/power${qs}`, opts);
}

export function fetchPlantPower(
  plantId: string,
  params: PowerQueryParams,
  opts?: FetchOptions,
) {
  const qs = buildQuery({
    from: params.from,
    to: params.to,
    interval: params.interval,
    fields: params.fields?.join(','),
    tz: params.tz,
    page: params.page?.toString(),
    per_page: params.per_page?.toString(),
  });
  return apiFetchPaginated<PowerResponse>(`/plants/${plantId}/power${qs}`, opts);
}

export function fetchDeviceLatest(
  sn: string,
  opts?: FetchOptions,
): Promise<LatestReading> {
  return apiFetch<LatestReading>(`/devices/${sn}/power/latest`, opts);
}

// --- Energy ---

export interface EnergyQueryParams {
  from: string;               // YYYY-MM-DD
  to: string;                 // YYYY-MM-DD
  unit?: 'day' | 'month';
  tz?: string;
}

export function fetchDeviceEnergy(
  sn: string,
  params: EnergyQueryParams,
  opts?: FetchOptions,
): Promise<EnergyResponse> {
  const qs = buildQuery({
    from: params.from,
    to: params.to,
    unit: params.unit,
    tz: params.tz,
  });
  return apiFetch<EnergyResponse>(`/devices/${sn}/energy${qs}`, opts);
}

export function fetchPlantEnergy(
  plantId: string,
  params: EnergyQueryParams,
  opts?: FetchOptions,
): Promise<EnergyResponse> {
  const qs = buildQuery({
    from: params.from,
    to: params.to,
    unit: params.unit,
    tz: params.tz,
  });
  return apiFetch<EnergyResponse>(`/plants/${plantId}/energy${qs}`, opts);
}

// --- Stats ---

export interface StatsQueryParams {
  from: string;               // YYYY-MM-DD
  to: string;                 // YYYY-MM-DD
  field?: PowerField;
  tz?: string;
}

export function fetchDeviceStats(
  sn: string,
  params: StatsQueryParams,
  opts?: FetchOptions,
): Promise<MultiDayStats> {
  const qs = buildQuery({
    from: params.from,
    to: params.to,
    field: params.field,
    tz: params.tz,
  });
  return apiFetch<MultiDayStats>(`/devices/${sn}/stats${qs}`, opts);
}

export function fetchPlantStats(
  plantId: string,
  params: StatsQueryParams,
  opts?: FetchOptions,
): Promise<MultiDayStats> {
  const qs = buildQuery({
    from: params.from,
    to: params.to,
    field: params.field,
    tz: params.tz,
  });
  return apiFetch<MultiDayStats>(`/plants/${plantId}/stats${qs}`, opts);
}
```

---

## 5. Zustand Store Definitions

### 5.1 Store Architecture

The store is split into slices using Zustand's slice pattern. Each slice manages a distinct domain. The slices are combined into a single store via `create()`.

### 5.2 Type Definitions (store/index.ts)

```typescript
// src/store/index.ts

import { create } from 'zustand';
import { subscribeWithSelector } from 'zustand/middleware';
import { createPlantSlice, type PlantSlice } from './slices/plantSlice';
import { createLiveSlice, type LiveSlice } from './slices/liveSlice';
import { createPowerSlice, type PowerSlice } from './slices/powerSlice';
import { createEnergySlice, type EnergySlice } from './slices/energySlice';
import { createStatsSlice, type StatsSlice } from './slices/statsSlice';
import { createUISlice, type UISlice } from './slices/uiSlice';

export type GrowattStore =
  & PlantSlice
  & LiveSlice
  & PowerSlice
  & EnergySlice
  & StatsSlice
  & UISlice;

export const useStore = create<GrowattStore>()(
  subscribeWithSelector((...args) => ({
    ...createPlantSlice(...args),
    ...createLiveSlice(...args),
    ...createPowerSlice(...args),
    ...createEnergySlice(...args),
    ...createStatsSlice(...args),
    ...createUISlice(...args),
  })),
);
```

### 5.3 Plant Slice

```typescript
// src/store/slices/plantSlice.ts

import type { StateCreator } from 'zustand';
import type { GrowattStore } from '../index';
import type { PlantListItem, PlantDetail, DeviceListItem } from '@/api/types';
import { fetchPlants, fetchPlant } from '@/api/client';

export interface PlantSlice {
  // State
  plants: PlantListItem[];
  plantsLoading: boolean;
  plantsError: string | null;
  selectedPlantId: string | null;
  selectedPlant: PlantDetail | null;
  selectedDeviceSn: string | null;

  // Actions
  loadPlants: () => Promise<void>;
  selectPlant: (id: string) => Promise<void>;
  selectDevice: (sn: string) => void;
}

export const createPlantSlice: StateCreator<GrowattStore, [], [], PlantSlice> = (
  set,
  get,
) => ({
  plants: [],
  plantsLoading: false,
  plantsError: null,
  selectedPlantId: null,
  selectedPlant: null,
  selectedDeviceSn: null,

  loadPlants: async () => {
    set({ plantsLoading: true, plantsError: null });
    try {
      const { data } = await fetchPlants({ per_page: 100 });
      set({ plants: data, plantsLoading: false });

      // Auto-select first plant if none selected
      if (!get().selectedPlantId && data.length > 0) {
        await get().selectPlant(data[0].id);
      }
    } catch (err) {
      set({
        plantsLoading: false,
        plantsError: err instanceof Error ? err.message : 'Failed to load plants',
      });
    }
  },

  selectPlant: async (id: string) => {
    set({ selectedPlantId: id, selectedPlant: null });
    try {
      const plant = await fetchPlant(id);
      set({ selectedPlant: plant });

      // Auto-select first device
      if (plant.devices.length > 0) {
        set({ selectedDeviceSn: plant.devices[0].serial_number });
      }
    } catch (err) {
      set({
        plantsError: err instanceof Error ? err.message : 'Failed to load plant',
      });
    }
  },

  selectDevice: (sn: string) => {
    set({ selectedDeviceSn: sn });
  },
});
```

### 5.4 Live Slice

```typescript
// src/store/slices/liveSlice.ts

import type { StateCreator } from 'zustand';
import type { GrowattStore } from '../index';
import type { LatestReading } from '@/api/types';
import { fetchDeviceLatest } from '@/api/client';

export interface LiveSlice {
  // State
  latestReading: LatestReading | null;
  liveLoading: boolean;
  liveError: string | null;
  autoRefresh: boolean;
  refreshIntervalMs: number;
  lastFetchTime: number | null;     // Date.now() of last successful fetch

  // Actions
  fetchLatest: () => Promise<void>;
  setRefreshInterval: (ms: number) => void;
  toggleAutoRefresh: () => void;
}

export const createLiveSlice: StateCreator<GrowattStore, [], [], LiveSlice> = (
  set,
  get,
) => ({
  latestReading: null,
  liveLoading: false,
  liveError: null,
  autoRefresh: true,
  refreshIntervalMs: 30_000,        // 30 seconds default
  lastFetchTime: null,

  fetchLatest: async () => {
    const sn = get().selectedDeviceSn;
    if (!sn) return;

    set({ liveLoading: true, liveError: null });
    try {
      const reading = await fetchDeviceLatest(sn);
      set({
        latestReading: reading,
        liveLoading: false,
        lastFetchTime: Date.now(),
      });
    } catch (err) {
      set({
        liveLoading: false,
        liveError: err instanceof Error ? err.message : 'Failed to fetch latest',
      });
    }
  },

  setRefreshInterval: (ms: number) => {
    set({ refreshIntervalMs: ms });
  },

  toggleAutoRefresh: () => {
    set((state) => ({ autoRefresh: !state.autoRefresh }));
  },
});
```

### 5.5 Power Slice

```typescript
// src/store/slices/powerSlice.ts

import type { StateCreator } from 'zustand';
import type { GrowattStore } from '../index';
import type { PowerResponse, PowerInterval, PowerField } from '@/api/types';
import { fetchDevicePower } from '@/api/client';
import { format, isToday, parseISO } from 'date-fns';

/** Cache key format: "{sn}:{from}:{to}:{interval}:{fields}" */
function cacheKey(
  sn: string,
  from: string,
  to: string,
  interval: PowerInterval,
  fields: PowerField[],
): string {
  return `${sn}:${from}:${to}:${interval}:${fields.sort().join(',')}`;
}

export interface PowerSlice {
  // State
  powerCache: Map<string, PowerResponse>;
  powerLoading: boolean;
  powerError: string | null;

  // Actions
  fetchPower: (params: {
    from: string;
    to: string;
    interval: PowerInterval;
    fields?: PowerField[];
    tz?: string;
  }) => Promise<PowerResponse | null>;
  clearPowerCache: () => void;
}

export const createPowerSlice: StateCreator<GrowattStore, [], [], PowerSlice> = (
  set,
  get,
) => ({
  powerCache: new Map(),
  powerLoading: false,
  powerError: null,

  fetchPower: async (params) => {
    const sn = get().selectedDeviceSn;
    if (!sn) return null;

    const fields = params.fields ?? ['pac_w'];
    const key = cacheKey(sn, params.from, params.to, params.interval, fields);

    // Check cache -- only use cache for completed (non-today) ranges
    const endIncludesToday = isToday(parseISO(params.to));
    const cached = get().powerCache.get(key);
    if (cached && !endIncludesToday) {
      return cached;
    }

    set({ powerLoading: true, powerError: null });
    try {
      const { data } = await fetchDevicePower(sn, {
        from: params.from,
        to: params.to,
        interval: params.interval,
        fields,
        tz: params.tz,
        per_page: 2000,
      });

      // Store in cache
      set((state) => {
        const newCache = new Map(state.powerCache);
        newCache.set(key, data);
        return { powerCache: newCache, powerLoading: false };
      });

      return data;
    } catch (err) {
      set({
        powerLoading: false,
        powerError: err instanceof Error ? err.message : 'Failed to fetch power data',
      });
      return null;
    }
  },

  clearPowerCache: () => {
    set({ powerCache: new Map() });
  },
});
```

### 5.6 Energy Slice

```typescript
// src/store/slices/energySlice.ts

import type { StateCreator } from 'zustand';
import type { GrowattStore } from '../index';
import type { EnergyResponse } from '@/api/types';
import { fetchDeviceEnergy } from '@/api/client';

export interface EnergySlice {
  energyData: EnergyResponse | null;
  energyLoading: boolean;
  energyError: string | null;

  fetchEnergy: (params: {
    from: string;
    to: string;
    unit?: 'day' | 'month';
    tz?: string;
  }) => Promise<void>;
}

export const createEnergySlice: StateCreator<GrowattStore, [], [], EnergySlice> = (
  set,
  get,
) => ({
  energyData: null,
  energyLoading: false,
  energyError: null,

  fetchEnergy: async (params) => {
    const sn = get().selectedDeviceSn;
    if (!sn) return;

    set({ energyLoading: true, energyError: null });
    try {
      const data = await fetchDeviceEnergy(sn, params);
      set({ energyData: data, energyLoading: false });
    } catch (err) {
      set({
        energyLoading: false,
        energyError: err instanceof Error ? err.message : 'Failed to fetch energy',
      });
    }
  },
});
```

### 5.7 Stats Slice

```typescript
// src/store/slices/statsSlice.ts

import type { StateCreator } from 'zustand';
import type { GrowattStore } from '../index';
import type { MultiDayStats, PowerField } from '@/api/types';
import { fetchDeviceStats } from '@/api/client';

export interface StatsSlice {
  statsData: MultiDayStats | null;
  statsLoading: boolean;
  statsError: string | null;

  fetchStats: (params: {
    from: string;
    to: string;
    field?: PowerField;
    tz?: string;
  }) => Promise<void>;
}

export const createStatsSlice: StateCreator<GrowattStore, [], [], StatsSlice> = (
  set,
  get,
) => ({
  statsData: null,
  statsLoading: false,
  statsError: null,

  fetchStats: async (params) => {
    const sn = get().selectedDeviceSn;
    if (!sn) return;

    set({ statsLoading: true, statsError: null });
    try {
      const data = await fetchDeviceStats(sn, params);
      set({ statsData: data, statsLoading: false });
    } catch (err) {
      set({
        statsLoading: false,
        statsError: err instanceof Error ? err.message : 'Failed to fetch stats',
      });
    }
  },
});
```

### 5.8 UI Slice

```typescript
// src/store/slices/uiSlice.ts

import type { StateCreator } from 'zustand';
import type { GrowattStore } from '../index';

export interface Toast {
  id: string;
  type: 'info' | 'success' | 'warning' | 'error';
  message: string;
  dismissAfterMs?: number;
}

export interface UISlice {
  darkMode: boolean;
  sidebarOpen: boolean;
  toasts: Toast[];

  toggleDarkMode: () => void;
  setSidebarOpen: (open: boolean) => void;
  addToast: (toast: Omit<Toast, 'id'>) => void;
  removeToast: (id: string) => void;
}

let toastCounter = 0;

export const createUISlice: StateCreator<GrowattStore, [], [], UISlice> = (set) => ({
  darkMode: localStorage.getItem('growatt-dark-mode') === 'true',
  sidebarOpen: true,
  toasts: [],

  toggleDarkMode: () => {
    set((state) => {
      const next = !state.darkMode;
      document.documentElement.classList.toggle('dark', next);
      localStorage.setItem('growatt-dark-mode', String(next));
      return { darkMode: next };
    });
  },

  setSidebarOpen: (open: boolean) => {
    set({ sidebarOpen: open });
  },

  addToast: (toast) => {
    const id = `toast-${++toastCounter}`;
    set((state) => ({
      toasts: [...state.toasts, { ...toast, id }],
    }));

    // Auto-dismiss
    if (toast.dismissAfterMs) {
      setTimeout(() => {
        set((state) => ({
          toasts: state.toasts.filter((t) => t.id !== id),
        }));
      }, toast.dismissAfterMs);
    }
  },

  removeToast: (id: string) => {
    set((state) => ({
      toasts: state.toasts.filter((t) => t.id !== id),
    }));
  },
});
```
