# MCP Server Implementation Design: gogrowatt-mcp

Detailed implementation blueprint for the gogrowatt MCP server. This document
provides complete Go code, JSON-RPC wire examples, type definitions, and test
strategies sufficient to implement the server without ambiguity.

---

## Table of Contents

1. [Architecture Principles](#1-architecture-principles)
2. [Package Structure](#2-package-structure)
3. [Configuration](#3-configuration)
4. [Transport Layer: JSON-RPC 2.0 over stdio](#4-transport-layer)
5. [MCP Protocol Handler](#5-mcp-protocol-handler)
6. [REST API Client (`internal/apiclient`)](#6-rest-api-client)
7. [Tool Definitions and Handlers](#7-tool-definitions-and-handlers)
8. [Resource Definitions and Handlers](#8-resource-definitions-and-handlers)
9. [Date Parsing (`internal/mcp/dates.go`)](#9-date-parsing)
10. [LLM-Optimized Response Formatting](#10-response-formatting)
11. [Error Handling](#11-error-handling)
12. [Logging](#12-logging)
13. [Unit Test Strategy](#13-unit-test-strategy)
14. [Integration Test Strategy](#14-integration-test-strategy)
15. [Client Configuration (Claude Desktop / Claude Code)](#15-client-configuration)

---

## 1. Architecture Principles

**Critical constraint (consistency fix #6):** The MCP server reads ALL data
through the gogrowatt REST API (`/api/v1/...`). It NEVER communicates with the
Growatt cloud API directly. Only the data fetcher service talks to Growatt.

```
AI Agent (Claude)
    |  stdin/stdout (JSON-RPC 2.0)
    v
gogrowatt-mcp process
    |  HTTP GET requests
    v
gogrowatt-api (REST API on localhost:8080)
    |  SQL queries
    v
PostgreSQL (TimescaleDB)
```

**Field naming (consistency fix #1, #5, #12):** All field names use the
canonical unit-suffixed names from the database schema: `ts`, `device_sn`,
`pac_w`, `ppv_w`, `vpv1_v`, `vpv2_v`, `ipv1_a`, `ipv2_a`, `vac1_v`, `iac1_a`,
`frequency_hz`, `temperature_c`, `today_energy_kwh`, `total_energy_kwh`.

**Transport rule:** stdout is exclusively for JSON-RPC messages. All diagnostic
output (logs, errors, debug) goes to stderr.

---

## 2. Package Structure

```
cmd/gogrowatt-mcp/
  main.go                  -- entry point, config loading, wiring

internal/mcp/
  server.go                -- MCP Server struct, protocol dispatch loop
  transport.go             -- JSON-RPC 2.0 reader/writer over stdio
  tools.go                 -- tool registry, definitions, dispatch
  tool_handlers.go         -- individual tool handler implementations
  resources.go             -- resource registry, definitions, dispatch
  resource_handlers.go     -- individual resource handler implementations
  dates.go                 -- flexible date parsing
  format.go                -- LLM-optimized text formatting functions
  errors.go                -- MCP error codes and helpers

internal/apiclient/
  client.go                -- HTTP client for gogrowatt REST API
  types.go                 -- Go structs matching REST API JSON responses
```

---

## 3. Configuration

### `cmd/gogrowatt-mcp/main.go`

```go
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/gogrowatt/internal/apiclient"
	"github.com/gogrowatt/internal/mcp"
)

// Config holds all configuration read from environment variables.
type Config struct {
	APIBaseURL    string // GOGROWATT_API_URL
	DefaultPlant  string // GROWATT_PLANT_ID
	DefaultDevice string // GROWATT_DEVICE_SN
	Timezone      string // GROWATT_TIMEZONE
}

func loadConfig() Config {
	cfg := Config{
		APIBaseURL:    envOr("GOGROWATT_API_URL", "http://localhost:8080"),
		DefaultPlant:  os.Getenv("GROWATT_PLANT_ID"),
		DefaultDevice: os.Getenv("GROWATT_DEVICE_SN"),
		Timezone:      envOr("GROWATT_TIMEZONE", "US/Central"),
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	// All logging goes to stderr -- stdout is the JSON-RPC transport.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg := loadConfig()
	logger.Info("starting gogrowatt-mcp",
		"api_url", cfg.APIBaseURL,
		"default_device", cfg.DefaultDevice,
		"timezone", cfg.Timezone,
	)

	client := apiclient.New(cfg.APIBaseURL)

	srv := mcp.NewServer(mcp.ServerConfig{
		Client:        client,
		DefaultPlant:  cfg.DefaultPlant,
		DefaultDevice: cfg.DefaultDevice,
		Timezone:      cfg.Timezone,
		Logger:        logger,
	})

	if err := srv.Run(os.Stdin, os.Stdout); err != nil {
		logger.Error("server exited with error", "err", err)
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}
```

### Environment Variables

| Variable            | Required | Default                 | Description                         |
|---------------------|----------|-------------------------|-------------------------------------|
| `GOGROWATT_API_URL` | No       | `http://localhost:8080`  | Base URL of the gogrowatt REST API  |
| `GROWATT_PLANT_ID`  | No       | (auto-detect)           | Default plant ID for convenience    |
| `GROWATT_DEVICE_SN` | No       | (auto-detect)           | Default device serial number        |
| `GROWATT_TIMEZONE`  | No       | `US/Central`            | Timezone for date resolution        |

The MCP server does NOT need Growatt cloud credentials. Those belong only to the
data fetcher.

---

## 4. Transport Layer

### `internal/mcp/transport.go`

The transport reads newline-delimited JSON-RPC 2.0 messages from stdin and writes
responses to stdout. Each message is a single JSON object terminated by `\n`.

```go
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// --- JSON-RPC 2.0 Types ---

type JSONRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"` // nil for notifications
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  interface{}      `json:"result,omitempty"`
	Error   *JSONRPCError    `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Standard JSON-RPC error codes
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// --- Transport ---

type Transport struct {
	reader  *bufio.Reader
	writer  io.Writer
	writeMu sync.Mutex
}

func NewTransport(r io.Reader, w io.Writer) *Transport {
	return &Transport{
		reader: bufio.NewReader(r),
		writer: w,
	}
}

// ReadMessage reads one JSON-RPC request from stdin.
// Returns io.EOF when stdin is closed.
func (t *Transport) ReadMessage() (*JSONRPCRequest, error) {
	line, err := t.reader.ReadBytes('\n')
	if err != nil {
		return nil, err // io.EOF or read error
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w", err)
	}

	if req.JSONRPC != "2.0" {
		return nil, fmt.Errorf("unsupported jsonrpc version: %q", req.JSONRPC)
	}

	return &req, nil
}

// WriteResponse writes one JSON-RPC response to stdout.
func (t *Transport) WriteResponse(resp *JSONRPCResponse) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}

	data = append(data, '\n')
	_, err = t.writer.Write(data)
	return err
}

// SendResult is a convenience for successful responses.
func (t *Transport) SendResult(id *json.RawMessage, result interface{}) error {
	return t.WriteResponse(&JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

// SendError is a convenience for error responses.
func (t *Transport) SendError(id *json.RawMessage, code int, message string) error {
	return t.WriteResponse(&JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: message},
	})
}
```

### Wire Format Examples

**Request (one line on stdin):**
```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claude-desktop","version":"1.0"}}}
```

**Response (one line on stdout):**
```json
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{},"resources":{}},"serverInfo":{"name":"gogrowatt-mcp","version":"0.1.0"}}}
```

**Notification (no id, no response expected):**
```json
{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}
```

---

## 5. MCP Protocol Handler

### `internal/mcp/server.go`

```go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/gogrowatt/internal/apiclient"
)

const (
	ServerName    = "gogrowatt-mcp"
	ServerVersion = "0.1.0"
	ProtoVersion  = "2024-11-05"
)

type ServerConfig struct {
	Client        *apiclient.Client
	DefaultPlant  string
	DefaultDevice string
	Timezone      string
	Logger        *slog.Logger
}

type Server struct {
	cfg       ServerConfig
	client    *apiclient.Client
	transport *Transport
	tools     *ToolRegistry
	resources *ResourceRegistry
	log       *slog.Logger
}

func NewServer(cfg ServerConfig) *Server {
	s := &Server{
		cfg:    cfg,
		client: cfg.Client,
		log:    cfg.Logger,
	}
	s.tools = NewToolRegistry(s)
	s.resources = NewResourceRegistry(s)
	return s
}

// Run starts the main message loop. Blocks until stdin is closed or a
// fatal error occurs.
func (s *Server) Run(stdin io.Reader, stdout io.Writer) error {
	s.transport = NewTransport(stdin, stdout)

	for {
		req, err := s.transport.ReadMessage()
		if err != nil {
			if err == io.EOF {
				s.log.Info("stdin closed, shutting down")
				return nil
			}
			s.log.Error("failed to read message", "err", err)
			// Send parse error if we can
			s.transport.SendError(nil, CodeParseError, err.Error())
			continue
		}

		s.log.Debug("received request", "method", req.Method, "id", string(ptrOrNull(req.ID)))
		s.handleRequest(req)
	}
}

func ptrOrNull(p *json.RawMessage) []byte {
	if p == nil {
		return []byte("null")
	}
	return *p
}

func (s *Server) handleRequest(req *JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "notifications/initialized":
		// Notification; no response needed. Client confirms ready.
		s.log.Info("client initialized")
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(req)
	case "resources/list":
		s.handleResourcesList(req)
	case "resources/templates/list":
		s.handleResourceTemplatesList(req)
	case "resources/read":
		s.handleResourcesRead(req)
	default:
		s.transport.SendError(req.ID, CodeMethodNotFound,
			fmt.Sprintf("unknown method: %s", req.Method))
	}
}

// --- initialize ---

type InitializeParams struct {
	ProtocolVersion string      `json:"protocolVersion"`
	Capabilities    interface{} `json:"capabilities"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

type Capabilities struct {
	Tools     *struct{} `json:"tools,omitempty"`
	Resources *struct{} `json:"resources,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleInitialize(req *JSONRPCRequest) {
	var params InitializeParams
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}

	s.log.Info("initialize",
		"client", params.ClientInfo.Name,
		"client_version", params.ClientInfo.Version,
		"protocol", params.ProtocolVersion,
	)

	result := InitializeResult{
		ProtocolVersion: ProtoVersion,
		Capabilities: Capabilities{
			Tools:     &struct{}{},
			Resources: &struct{}{},
		},
		ServerInfo: ServerInfo{
			Name:    ServerName,
			Version: ServerVersion,
		},
	}

	s.transport.SendResult(req.ID, result)
}

// --- tools/list ---

func (s *Server) handleToolsList(req *JSONRPCRequest) {
	s.transport.SendResult(req.ID, map[string]interface{}{
		"tools": s.tools.Definitions(),
	})
}

// --- tools/call ---

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is the MCP tool call result format.
type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) handleToolsCall(req *JSONRPCRequest) {
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.transport.SendError(req.ID, CodeInvalidParams,
			"invalid tools/call params: "+err.Error())
		return
	}

	s.log.Info("tools/call", "tool", params.Name)

	ctx := context.Background()
	result := s.tools.Call(ctx, params.Name, params.Arguments)
	s.transport.SendResult(req.ID, result)
}

// --- resources/list ---

func (s *Server) handleResourcesList(req *JSONRPCRequest) {
	s.transport.SendResult(req.ID, map[string]interface{}{
		"resources": s.resources.StaticResources(),
	})
}

// --- resources/templates/list ---

func (s *Server) handleResourceTemplatesList(req *JSONRPCRequest) {
	s.transport.SendResult(req.ID, map[string]interface{}{
		"resourceTemplates": s.resources.Templates(),
	})
}

// --- resources/read ---

type ResourceReadParams struct {
	URI string `json:"uri"`
}

func (s *Server) handleResourcesRead(req *JSONRPCRequest) {
	var params ResourceReadParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.transport.SendError(req.ID, CodeInvalidParams,
			"invalid resources/read params: "+err.Error())
		return
	}

	s.log.Info("resources/read", "uri", params.URI)

	ctx := context.Background()
	result, err := s.resources.Read(ctx, params.URI)
	if err != nil {
		s.transport.SendError(req.ID, CodeInvalidParams, err.Error())
		return
	}
	s.transport.SendResult(req.ID, result)
}
```

### Initialize Handshake Wire Example

```
--> {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claude-desktop","version":"1.0"}}}
<-- {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{},"resources":{}},"serverInfo":{"name":"gogrowatt-mcp","version":"0.1.0"}}}
--> {"jsonrpc":"2.0","method":"notifications/initialized","params":{}}
```

---

## 6. REST API Client

### `internal/apiclient/types.go`

All types match the REST API JSON responses from doc 03. Field names use the
consistency-fixed unit-suffixed names.

```go
package apiclient

// Envelope is the standard REST API response wrapper.
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
	Code    string `json:"code"`
	Message string `json:"message"`
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

// --- Domain Types ---

type Plant struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Country        string  `json:"country"`
	City           string  `json:"city"`
	Latitude       float64 `json:"latitude"`
	Longitude      float64 `json:"longitude"`
	PeakPowerKW    float64 `json:"peak_power_kw"`
	Status         string  `json:"status"`
	CurrentPowerW  float64 `json:"current_power_w"`
	TodayEnergyKWh float64 `json:"today_energy_kwh"`
	TotalEnergyKWh float64 `json:"total_energy_kwh"`
	DeviceCount    int     `json:"device_count"`
}

type Device struct {
	SerialNumber string `json:"serial_number"`
	PlantID      string `json:"plant_id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Model        string `json:"model"`
	Status       string `json:"status"`
	LastUpdate   string `json:"last_update"`
}

type DeviceDetail struct {
	SerialNumber string        `json:"serial_number"`
	PlantID      string        `json:"plant_id"`
	Name         string        `json:"name"`
	Type         string        `json:"type"`
	Model        string        `json:"model"`
	Status       string        `json:"status"`
	LastUpdate   string        `json:"last_update"`
	Current      DeviceCurrent `json:"current"`
}

type DeviceCurrent struct {
	PacW           float64 `json:"pac_w"`
	PpvW           float64 `json:"ppv_w"`
	Vpv1V          float64 `json:"vpv1_v"`
	Vpv2V          float64 `json:"vpv2_v"`
	Ipv1A          float64 `json:"ipv1_a"`
	Ipv2A          float64 `json:"ipv2_a"`
	Vac1V          float64 `json:"vac1_v"`
	Iac1A          float64 `json:"iac1_a"`
	FrequencyHz    float64 `json:"frequency_hz"`
	TemperatureC   float64 `json:"temperature_c"`
	TodayEnergyKWh float64 `json:"today_energy_kwh"`
	TotalEnergyKWh float64 `json:"total_energy_kwh"`
}

// LatestReading from GET /devices/{sn}/power/latest
type LatestReading struct {
	SerialNumber string  `json:"serial_number"`
	Time         string  `json:"time"`
	PacW         float64 `json:"pac_w"`
	PpvW         float64 `json:"ppv_w"`
	Vpv1V        float64 `json:"vpv1_v"`
	Vpv2V        float64 `json:"vpv2_v"`
	Ipv1A        float64 `json:"ipv1_a"`
	Ipv2A        float64 `json:"ipv2_a"`
	Vac1V        float64 `json:"vac1_v"`
	Iac1A        float64 `json:"iac1_a"`
}

// PowerResponse from GET /devices/{sn}/power
type PowerResponse struct {
	SerialNumber string    `json:"serial_number"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	Interval     string    `json:"interval"`
	Timezone     string    `json:"timezone"`
	Fields       []string  `json:"fields"`
	Readings     []Reading `json:"readings"`
}

// Reading can hold raw 5min data or aggregated data.
// For 5min: PacW is a float64.
// For 1h/1d: PacW is an AggBucket.
// We use json.RawMessage and parse contextually.
type Reading struct {
	Time string          `json:"time"`
	PacW json.RawMessage `json:"pac_w,omitempty"`
}

// ReadingSimple is for 5min interval (raw float values).
type ReadingSimple struct {
	Time string  `json:"time"`
	PacW float64 `json:"pac_w"`
}

// ReadingAggregated is for 1h/1d intervals (avg/min/max/samples).
type ReadingAggregated struct {
	Time string    `json:"time"`
	PacW AggBucket `json:"pac_w"`
}

type AggBucket struct {
	Avg     float64 `json:"avg"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Samples int     `json:"samples"`
}

// EnergyResponse from GET /devices/{sn}/energy
type EnergyResponse struct {
	SerialNumber string        `json:"serial_number"`
	From         string        `json:"from"`
	To           string        `json:"to"`
	Unit         string        `json:"unit"`
	Timezone     string        `json:"timezone"`
	Totals       []EnergyTotal `json:"totals"`
	Summary      EnergySummary `json:"summary"`
}

type EnergyTotal struct {
	Date      string  `json:"date"`
	EnergyKWh float64 `json:"energy_kwh"`
}

type EnergySummary struct {
	TotalKWh     float64 `json:"total_kwh"`
	AverageKWh   float64 `json:"average_kwh"`
	MaxKWh       float64 `json:"max_kwh"`
	MaxDate      string  `json:"max_date"`
	MinKWh       float64 `json:"min_kwh"`
	MinDate      string  `json:"min_date"`
	DaysWithData int     `json:"days_with_data"`
}

// StatsResponse from GET /devices/{sn}/stats
type StatsResponse struct {
	SerialNumber       string       `json:"serial_number"`
	From               string       `json:"from"`
	To                 string       `json:"to"`
	Field              string       `json:"field"`
	Timezone           string       `json:"timezone"`
	DaysAnalyzed       int          `json:"days_analyzed"`
	TotalProductionKWh float64      `json:"total_production_kwh"`
	DailyAverageKWh    float64      `json:"daily_average_kwh"`
	PeakHour           int          `json:"peak_hour"`
	PeakPowerAvgW      float64      `json:"peak_power_avg_w"`
	ByHour             []HourlyBin  `json:"by_hour"`
}

type HourlyBin struct {
	Hour       int     `json:"hour"`
	MinW       float64 `json:"min_w"`
	MaxW       float64 `json:"max_w"`
	AvgW       float64 `json:"avg_w"`
	MedianW    float64 `json:"median_w"`
	StddevW    float64 `json:"stddev_w"`
	SampleDays int     `json:"sample_days"`
}

// --- Query Parameter Structs ---

type PowerParams struct {
	From     string
	To       string
	Interval string
	Fields   string
	Tz       string
}

type EnergyParams struct {
	From string
	To   string
	Unit string
	Tz   string
}

type StatsParams struct {
	From  string
	To    string
	Field string
	Tz    string
}
```

### `internal/apiclient/client.go`

```go
package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ListPlants fetches GET /api/v1/plants.
func (c *Client) ListPlants(ctx context.Context) ([]Plant, error) {
	var env Envelope[[]Plant]
	if err := c.getJSON(ctx, "/api/v1/plants", nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// GetPlantDevices fetches GET /api/v1/plants/{id}/devices.
func (c *Client) GetPlantDevices(ctx context.Context, plantID string) ([]Device, error) {
	var env Envelope[[]Device]
	path := fmt.Sprintf("/api/v1/plants/%s/devices", plantID)
	if err := c.getJSON(ctx, path, nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// GetDevice fetches GET /api/v1/devices/{sn}.
func (c *Client) GetDevice(ctx context.Context, sn string) (*DeviceDetail, error) {
	var env Envelope[DeviceDetail]
	path := fmt.Sprintf("/api/v1/devices/%s", sn)
	if err := c.getJSON(ctx, path, nil, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// GetDevicePowerLatest fetches GET /api/v1/devices/{sn}/power/latest.
func (c *Client) GetDevicePowerLatest(ctx context.Context, sn string) (*LatestReading, error) {
	var env Envelope[LatestReading]
	path := fmt.Sprintf("/api/v1/devices/%s/power/latest", sn)
	if err := c.getJSON(ctx, path, nil, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// GetDevicePower fetches GET /api/v1/devices/{sn}/power with query params.
func (c *Client) GetDevicePower(ctx context.Context, sn string, params PowerParams) (*PowerResponse, error) {
	q := url.Values{}
	q.Set("from", params.From)
	q.Set("to", params.To)
	if params.Interval != "" {
		q.Set("interval", params.Interval)
	}
	if params.Fields != "" {
		q.Set("fields", params.Fields)
	}
	if params.Tz != "" {
		q.Set("tz", params.Tz)
	}

	var env Envelope[PowerResponse]
	path := fmt.Sprintf("/api/v1/devices/%s/power", sn)
	if err := c.getJSON(ctx, path, q, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// GetDeviceEnergy fetches GET /api/v1/devices/{sn}/energy.
func (c *Client) GetDeviceEnergy(ctx context.Context, sn string, params EnergyParams) (*EnergyResponse, error) {
	q := url.Values{}
	q.Set("from", params.From)
	q.Set("to", params.To)
	if params.Unit != "" {
		q.Set("unit", params.Unit)
	}
	if params.Tz != "" {
		q.Set("tz", params.Tz)
	}

	var env Envelope[EnergyResponse]
	path := fmt.Sprintf("/api/v1/devices/%s/energy", sn)
	if err := c.getJSON(ctx, path, q, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// GetDeviceStats fetches GET /api/v1/devices/{sn}/stats.
func (c *Client) GetDeviceStats(ctx context.Context, sn string, params StatsParams) (*StatsResponse, error) {
	q := url.Values{}
	q.Set("from", params.From)
	q.Set("to", params.To)
	if params.Field != "" {
		q.Set("field", params.Field)
	}
	if params.Tz != "" {
		q.Set("tz", params.Tz)
	}

	var env Envelope[StatsResponse]
	path := fmt.Sprintf("/api/v1/devices/%s/stats", sn)
	if err := c.getJSON(ctx, path, q, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// --- Internal HTTP helpers ---

// APIErrorResponse represents an error from the REST API.
type APIErrorResponse struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIErrorResponse) Error() string {
	return fmt.Sprintf("REST API %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out interface{}) error {
	u := c.baseURL + path
	if query != nil && len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request to REST API failed (is gogrowatt-api running at %s?): %w",
			c.baseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to parse error envelope
		var errEnv ErrorEnvelope
		if json.Unmarshal(body, &errEnv) == nil && errEnv.Error.Code != "" {
			return &APIErrorResponse{
				StatusCode: resp.StatusCode,
				Code:       errEnv.Error.Code,
				Message:    errEnv.Error.Message,
			}
		}
		return &APIErrorResponse{
			StatusCode: resp.StatusCode,
			Code:       "UNKNOWN",
			Message:    string(body),
		}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response JSON: %w", err)
	}
	return nil
}
```

---

## 7. Tool Definitions and Handlers

### `internal/mcp/tools.go` -- Registry and Definitions

```go
package mcp

import (
	"context"
	"encoding/json"
)

// ToolDefinition is the MCP tool schema returned by tools/list.
type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]PropertySchema `json:"properties"`
	Required   []string                  `json:"required"`
}

type PropertySchema struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Items       *Items   `json:"items,omitempty"`
}

type Items struct {
	Type string `json:"type"`
}

type ToolHandler func(ctx context.Context, args json.RawMessage) ToolResult

type ToolRegistry struct {
	server   *Server
	defs     []ToolDefinition
	handlers map[string]ToolHandler
}

func NewToolRegistry(s *Server) *ToolRegistry {
	r := &ToolRegistry{
		server:   s,
		handlers: make(map[string]ToolHandler),
	}
	r.register()
	return r
}

func (r *ToolRegistry) Definitions() []ToolDefinition {
	return r.defs
}

func (r *ToolRegistry) Call(ctx context.Context, name string, args json.RawMessage) ToolResult {
	h, ok := r.handlers[name]
	if !ok {
		return errorResult("Unknown tool: " + name + ". Use tools/list to see available tools.")
	}
	return h(ctx, args)
}

func (r *ToolRegistry) add(def ToolDefinition, handler ToolHandler) {
	r.defs = append(r.defs, def)
	r.handlers[def.Name] = handler
}

func textResult(text string) ToolResult {
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: text}},
	}
}

func errorResult(text string) ToolResult {
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: "Error: " + text}},
		IsError: true,
	}
}

func (r *ToolRegistry) register() {
	// 1. list_plants
	r.add(ToolDefinition{
		Name:        "list_plants",
		Description: "List all solar plants (power stations). Returns plant IDs, names, locations, system capacity, and current status. Use this first to discover available plant_id values needed by other tools.",
		InputSchema: InputSchema{
			Type:       "object",
			Properties: map[string]PropertySchema{},
			Required:   []string{},
		},
	}, r.server.toolListPlants)

	// 2. list_devices
	r.add(ToolDefinition{
		Name:        "list_devices",
		Description: "List all devices (inverters, meters) for a specific plant. Returns device serial numbers, types, models, and online/offline status. Use the device serial number (sn) from this response as the 'sn' parameter in power/energy/stats tools.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"plant_id": {Type: "string", Description: "Plant ID from list_plants. Example: '12345'"},
			},
			Required: []string{"plant_id"},
		},
	}, r.server.toolListDevices)

	// 3. get_device_details
	r.add(ToolDefinition{
		Name:        "get_device_details",
		Description: "Get live readings from a specific inverter device. Returns current AC power output, today's energy, PV string voltages and currents (useful for diagnosing string imbalance), AC voltage/current/frequency, and inverter temperature. The 'sn' is the device serial number from list_devices.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"sn": {Type: "string", Description: "Device serial number. Example: 'TLXABC12345'"},
			},
			Required: []string{"sn"},
		},
	}, r.server.toolGetDeviceDetails)

	// 4. get_current_power
	r.add(ToolDefinition{
		Name:        "get_current_power",
		Description: "Get the current instantaneous power output (watts) and latest reading for a device. If sn is omitted and GROWATT_DEVICE_SN is configured, it is used as the default. Best for quick 'how much power right now?' queries.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"sn": {Type: "string", Description: "Device serial number. Optional if GROWATT_DEVICE_SN is set."},
			},
			Required: []string{},
		},
	}, r.server.toolGetCurrentPower)

	// 5. get_power_history
	r.add(ToolDefinition{
		Name:        "get_power_history",
		Description: "Get historical power production data (watts) as a time series. Data is natively collected at 5-minute intervals. Use 'interval' to aggregate: '5min' for raw data (best for single-day analysis), '15min' for smoothed curves, '1h' for daily profiles (returns min/max/avg per hour), '1d' for multi-week trends. Date parameters accept: 'today', 'yesterday', 'YYYY-MM-DD', or relative like 'last-week'. For ranges over 7 days, prefer '1d' interval to keep response size manageable.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"sn":       {Type: "string", Description: "Device serial number from list_devices."},
				"from":     {Type: "string", Description: "Start date. Accepts 'today', 'yesterday', 'last-week', or 'YYYY-MM-DD'. Default: 'today'."},
				"to":       {Type: "string", Description: "End date (inclusive). Same formats as 'from'. Default: same as 'from'."},
				"interval": {Type: "string", Enum: []string{"5min", "15min", "1h", "1d"}, Description: "Aggregation interval. Default: '5min' for single day, '1h' for multi-day."},
			},
			Required: []string{"sn"},
		},
	}, r.server.toolGetPowerHistory)

	// 6. get_energy_summary
	r.add(ToolDefinition{
		Name:        "get_energy_summary",
		Description: "Get energy production totals (kWh) aggregated by day or month. Unlike get_power_history which returns instantaneous power (watts), this returns actual metered energy totals. Use 'day' for daily totals (up to ~90 days), 'month' for monthly totals (up to years of data). This is the most accurate source for 'how much energy was produced' questions.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"sn":   {Type: "string", Description: "Device serial number from list_devices."},
				"from": {Type: "string", Description: "Start date. 'YYYY-MM-DD' for daily, 'YYYY-MM' for monthly. Also accepts 'today', 'yesterday', 'last-week', 'last-month'."},
				"to":   {Type: "string", Description: "End date (inclusive). Same formats as 'from'."},
				"unit": {Type: "string", Enum: []string{"day", "month"}, Description: "Aggregation period. Default: 'day'."},
			},
			Required: []string{"sn", "from", "to"},
		},
	}, r.server.toolGetEnergySummary)

	// 7. get_production_stats
	r.add(ToolDefinition{
		Name:        "get_production_stats",
		Description: "Get statistical analysis of power production over a date range. Returns per-hour statistics (min, max, average, median, standard deviation) computed across all days in the range. Useful for understanding typical daily production profiles, identifying anomalies, and answering questions like 'what is the average power at 2pm?' or 'how variable is morning production?'. Limit to 30 days max for reasonable response size.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"sn":   {Type: "string", Description: "Device serial number."},
				"from": {Type: "string", Description: "Start date. Accepts 'today', 'yesterday', 'last-week', 'YYYY-MM-DD'."},
				"to":   {Type: "string", Description: "End date (inclusive). Same formats as 'from'."},
			},
			Required: []string{"sn", "from", "to"},
		},
	}, r.server.toolGetProductionStats)

	// 8. compare_days
	r.add(ToolDefinition{
		Name:        "compare_days",
		Description: "Compare power production profiles across multiple specific days. Returns data for each requested day aligned on the same time axis, making it easy to spot patterns, weather impacts, or seasonal shifts. Limit to 7 days max. Use '1h' interval for readable comparisons.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"sn":       {Type: "string", Description: "Device serial number."},
				"dates":    {Type: "array", Items: &Items{Type: "string"}, Description: "List of dates to compare. Accepts 'today', 'yesterday', 'YYYY-MM-DD'. Example: ['2026-02-14', '2026-02-15']."},
				"interval": {Type: "string", Enum: []string{"5min", "15min", "1h"}, Description: "Time resolution for comparison. Default: '1h'."},
			},
			Required: []string{"sn", "dates"},
		},
	}, r.server.toolCompareDays)
}
```

### `internal/mcp/tool_handlers.go` -- All 8 Tool Implementations

```go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gogrowatt/internal/apiclient"
)

// --- Tool 1: list_plants ---

func (s *Server) toolListPlants(ctx context.Context, args json.RawMessage) ToolResult {
	plants, err := s.client.ListPlants(ctx)
	if err != nil {
		return s.apiErrorResult("list plants", err)
	}
	return textResult(FormatPlantList(plants))
}

// --- Tool 2: list_devices ---

type listDevicesArgs struct {
	PlantID string `json:"plant_id"`
}

func (s *Server) toolListDevices(ctx context.Context, args json.RawMessage) ToolResult {
	var a listDevicesArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errorResult("Invalid arguments: " + err.Error())
	}
	if a.PlantID == "" {
		return errorResult("Missing required parameter 'plant_id'. Use list_plants to find plant IDs.")
	}

	devices, err := s.client.GetPlantDevices(ctx, a.PlantID)
	if err != nil {
		return s.apiErrorResult("list devices", err)
	}
	return textResult(FormatDeviceList(a.PlantID, devices))
}

// --- Tool 3: get_device_details ---

type deviceSNArgs struct {
	SN string `json:"sn"`
}

func (s *Server) toolGetDeviceDetails(ctx context.Context, args json.RawMessage) ToolResult {
	var a deviceSNArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errorResult("Invalid arguments: " + err.Error())
	}
	if a.SN == "" {
		return errorResult("Missing required parameter 'sn'. Use list_devices to find serial numbers.")
	}

	device, err := s.client.GetDevice(ctx, a.SN)
	if err != nil {
		return s.apiErrorResult("get device details", err)
	}
	return textResult(FormatDeviceDetails(device))
}

// --- Tool 4: get_current_power ---

func (s *Server) toolGetCurrentPower(ctx context.Context, args json.RawMessage) ToolResult {
	var a deviceSNArgs
	if args != nil {
		json.Unmarshal(args, &a)
	}

	sn := a.SN
	if sn == "" {
		sn = s.cfg.DefaultDevice
	}
	if sn == "" {
		return errorResult("No device serial number provided and GROWATT_DEVICE_SN is not configured. Provide 'sn' or use list_devices to find one.")
	}

	reading, err := s.client.GetDevicePowerLatest(ctx, sn)
	if err != nil {
		return s.apiErrorResult("get current power", err)
	}
	return textResult(FormatCurrentPower(reading))
}

// --- Tool 5: get_power_history ---

type powerHistoryArgs struct {
	SN       string `json:"sn"`
	From     string `json:"from"`
	To       string `json:"to"`
	Interval string `json:"interval"`
}

func (s *Server) toolGetPowerHistory(ctx context.Context, args json.RawMessage) ToolResult {
	var a powerHistoryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errorResult("Invalid arguments: " + err.Error())
	}
	if a.SN == "" {
		return errorResult("Missing required parameter 'sn'.")
	}

	// Default from to "today"
	if a.From == "" {
		a.From = "today"
	}

	fromDate, err := ParseFlexDate(a.From)
	if err != nil {
		return errorResult(err.Error())
	}
	fromStr := FormatDate(fromDate)

	// Default to = from (single day)
	toStr := fromStr
	if a.To != "" {
		toDate, err := ParseFlexDate(a.To)
		if err != nil {
			return errorResult(err.Error())
		}
		toStr = FormatDate(toDate)
	}

	// Auto-select interval if not specified
	interval := a.Interval
	if interval == "" {
		if fromStr == toStr {
			interval = "5min"
		} else {
			interval = "1h"
		}
	}

	resp, err := s.client.GetDevicePower(ctx, a.SN, apiclient.PowerParams{
		From:     fromStr,
		To:       toStr,
		Interval: interval,
		Fields:   "pac_w",
		Tz:       s.cfg.Timezone,
	})
	if err != nil {
		return s.apiErrorResult("get power history", err)
	}
	return textResult(FormatPowerHistory(resp, interval))
}

// --- Tool 6: get_energy_summary ---

type energySummaryArgs struct {
	SN   string `json:"sn"`
	From string `json:"from"`
	To   string `json:"to"`
	Unit string `json:"unit"`
}

func (s *Server) toolGetEnergySummary(ctx context.Context, args json.RawMessage) ToolResult {
	var a energySummaryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errorResult("Invalid arguments: " + err.Error())
	}
	if a.SN == "" || a.From == "" || a.To == "" {
		return errorResult("Missing required parameters. Need 'sn', 'from', and 'to'.")
	}

	fromDate, err := ParseFlexDate(a.From)
	if err != nil {
		return errorResult(err.Error())
	}
	toDate, err := ParseFlexDate(a.To)
	if err != nil {
		return errorResult(err.Error())
	}

	unit := a.Unit
	if unit == "" {
		unit = "day"
	}

	resp, err := s.client.GetDeviceEnergy(ctx, a.SN, apiclient.EnergyParams{
		From: FormatDate(fromDate),
		To:   FormatDate(toDate),
		Unit: unit,
		Tz:   s.cfg.Timezone,
	})
	if err != nil {
		return s.apiErrorResult("get energy summary", err)
	}
	return textResult(FormatEnergySummary(resp))
}

// --- Tool 7: get_production_stats ---

type productionStatsArgs struct {
	SN   string `json:"sn"`
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) toolGetProductionStats(ctx context.Context, args json.RawMessage) ToolResult {
	var a productionStatsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errorResult("Invalid arguments: " + err.Error())
	}
	if a.SN == "" || a.From == "" || a.To == "" {
		return errorResult("Missing required parameters. Need 'sn', 'from', and 'to'.")
	}

	fromDate, err := ParseFlexDate(a.From)
	if err != nil {
		return errorResult(err.Error())
	}
	toDate, err := ParseFlexDate(a.To)
	if err != nil {
		return errorResult(err.Error())
	}

	resp, err := s.client.GetDeviceStats(ctx, a.SN, apiclient.StatsParams{
		From:  FormatDate(fromDate),
		To:    FormatDate(toDate),
		Field: "pac_w",
		Tz:    s.cfg.Timezone,
	})
	if err != nil {
		return s.apiErrorResult("get production stats", err)
	}
	return textResult(FormatProductionStats(resp))
}

// --- Tool 8: compare_days ---

type compareDaysArgs struct {
	SN       string   `json:"sn"`
	Dates    []string `json:"dates"`
	Interval string   `json:"interval"`
}

func (s *Server) toolCompareDays(ctx context.Context, args json.RawMessage) ToolResult {
	var a compareDaysArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errorResult("Invalid arguments: " + err.Error())
	}
	if a.SN == "" || len(a.Dates) == 0 {
		return errorResult("Missing required parameters. Need 'sn' and 'dates' array.")
	}
	if len(a.Dates) > 7 {
		return errorResult("Too many dates. Maximum 7 days for comparison.")
	}

	interval := a.Interval
	if interval == "" {
		interval = "1h"
	}

	// Resolve all dates
	resolvedDates := make([]string, len(a.Dates))
	for i, d := range a.Dates {
		t, err := ParseFlexDate(d)
		if err != nil {
			return errorResult(fmt.Sprintf("Invalid date at index %d: %s", i, err.Error()))
		}
		resolvedDates[i] = FormatDate(t)
	}

	// Fetch power data for each date (one REST API call per date)
	dayData := make([]*apiclient.PowerResponse, len(resolvedDates))
	for i, dateStr := range resolvedDates {
		resp, err := s.client.GetDevicePower(ctx, a.SN, apiclient.PowerParams{
			From:     dateStr,
			To:       dateStr,
			Interval: interval,
			Fields:   "pac_w",
			Tz:       s.cfg.Timezone,
		})
		if err != nil {
			return s.apiErrorResult(fmt.Sprintf("fetch data for %s", dateStr), err)
		}
		dayData[i] = resp
	}

	return textResult(FormatCompareDays(a.SN, resolvedDates, dayData, interval))
}
```

---

## 8. Resource Definitions and Handlers

### `internal/mcp/resources.go`

```go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ResourceDefinition for resources/list (static, non-templated).
type ResourceDefinition struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// ResourceTemplate for resources/templates/list (URI templates with params).
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// ResourceContent is returned inside resources/read results.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

// ResourceReadResult is the resources/read response.
type ResourceReadResult struct {
	Contents []ResourceContent `json:"contents"`
}

type ResourceRegistry struct {
	server *Server
}

func NewResourceRegistry(s *Server) *ResourceRegistry {
	return &ResourceRegistry{server: s}
}

// StaticResources returns non-templated resources.
func (r *ResourceRegistry) StaticResources() []ResourceDefinition {
	return []ResourceDefinition{
		{
			URI:         "solar://plants",
			Name:        "Solar Plants",
			Description: "List of all solar plants with current power output and today's energy. Refreshed on each read via GET /api/v1/plants.",
			MimeType:    "application/json",
		},
	}
}

// Templates returns parameterized resource templates.
func (r *ResourceRegistry) Templates() []ResourceTemplate {
	return []ResourceTemplate{
		{
			URITemplate: "solar://devices/{sn}/current",
			Name:        "Device Live Readings",
			Description: "Current inverter readings: AC power, PV string voltages/currents, AC voltage/current/frequency, and temperature.",
			MimeType:    "application/json",
		},
		{
			URITemplate: "solar://devices/{sn}/power/{date}",
			Name:        "Daily Power Profile",
			Description: "Complete power production profile for a specific date at 5-minute resolution. The {date} accepts 'today', 'yesterday', or 'YYYY-MM-DD'.",
			MimeType:    "application/json",
		},
	}
}

// URI matching patterns
var (
	rePlants       = regexp.MustCompile(`^solar://plants$`)
	reDevCurrent   = regexp.MustCompile(`^solar://devices/([^/]+)/current$`)
	reDevPowerDate = regexp.MustCompile(`^solar://devices/([^/]+)/power/([^/]+)$`)
)

// Read dispatches a resource read by URI.
func (r *ResourceRegistry) Read(ctx context.Context, uri string) (*ResourceReadResult, error) {
	if rePlants.MatchString(uri) {
		return r.readPlants(ctx, uri)
	}
	if m := reDevCurrent.FindStringSubmatch(uri); m != nil {
		return r.readDeviceCurrent(ctx, uri, m[1])
	}
	if m := reDevPowerDate.FindStringSubmatch(uri); m != nil {
		return r.readDevicePower(ctx, uri, m[1], m[2])
	}
	return nil, fmt.Errorf("unknown resource URI: %s", uri)
}

func jsonText(v interface{}) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
```

### `internal/mcp/resource_handlers.go`

```go
package mcp

import (
	"context"
	"fmt"

	"github.com/gogrowatt/internal/apiclient"
)

// --- Resource: solar://plants ---

type plantsResourceData struct {
	Plants []plantResourceItem `json:"plants"`
}

type plantResourceItem struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Location       locData `json:"location"`
	PeakPowerKW    float64 `json:"peak_power_kw"`
	CurrentPowerW  float64 `json:"current_power_w"`
	TodayEnergyKWh float64 `json:"today_energy_kwh"`
	TotalEnergyKWh float64 `json:"total_energy_kwh"`
	Status         string  `json:"status"`
}

type locData struct {
	City    string  `json:"city"`
	Country string  `json:"country"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

func (r *ResourceRegistry) readPlants(ctx context.Context, uri string) (*ResourceReadResult, error) {
	plants, err := r.server.client.ListPlants(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch plants: %w", err)
	}

	items := make([]plantResourceItem, len(plants))
	for i, p := range plants {
		items[i] = plantResourceItem{
			ID:   p.ID,
			Name: p.Name,
			Location: locData{
				City: p.City, Country: p.Country,
				Lat: p.Latitude, Lon: p.Longitude,
			},
			PeakPowerKW:    p.PeakPowerKW,
			CurrentPowerW:  p.CurrentPowerW,
			TodayEnergyKWh: p.TodayEnergyKWh,
			TotalEnergyKWh: p.TotalEnergyKWh,
			Status:         p.Status,
		}
	}

	text, err := jsonText(plantsResourceData{Plants: items})
	if err != nil {
		return nil, err
	}
	return &ResourceReadResult{
		Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: text}},
	}, nil
}

// --- Resource: solar://devices/{sn}/current ---

type deviceCurrentResourceData struct {
	SerialNumber   string           `json:"serial_number"`
	PacW           float64          `json:"pac_w"`
	PpvW           float64          `json:"ppv_w"`
	TodayEnergyKWh float64         `json:"today_energy_kwh"`
	TotalEnergyKWh float64         `json:"total_energy_kwh"`
	PVStrings      []pvStringData   `json:"pv_strings"`
	AC             acData           `json:"ac"`
	TemperatureC   float64          `json:"temperature_c"`
	Status         string           `json:"status"`
}

type pvStringData struct {
	ID     int     `json:"id"`
	VpvV   float64 `json:"vpv_v"`
	IpvA   float64 `json:"ipv_a"`
	PowerW float64 `json:"power_w"`
}

type acData struct {
	Vac1V       float64 `json:"vac1_v"`
	Iac1A       float64 `json:"iac1_a"`
	FrequencyHz float64 `json:"frequency_hz"`
}

func (r *ResourceRegistry) readDeviceCurrent(ctx context.Context, uri, sn string) (*ResourceReadResult, error) {
	dev, err := r.server.client.GetDevice(ctx, sn)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch device %s: %w", sn, err)
	}

	c := dev.Current
	data := deviceCurrentResourceData{
		SerialNumber:   dev.SerialNumber,
		PacW:           c.PacW,
		PpvW:           c.PpvW,
		TodayEnergyKWh: c.TodayEnergyKWh,
		TotalEnergyKWh: c.TotalEnergyKWh,
		PVStrings: []pvStringData{
			{ID: 1, VpvV: c.Vpv1V, IpvA: c.Ipv1A, PowerW: c.Vpv1V * c.Ipv1A},
			{ID: 2, VpvV: c.Vpv2V, IpvA: c.Ipv2A, PowerW: c.Vpv2V * c.Ipv2A},
		},
		AC: acData{
			Vac1V: c.Vac1V, Iac1A: c.Iac1A, FrequencyHz: c.FrequencyHz,
		},
		TemperatureC: c.TemperatureC,
		Status:       dev.Status,
	}

	text, err := jsonText(data)
	if err != nil {
		return nil, err
	}
	return &ResourceReadResult{
		Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: text}},
	}, nil
}

// --- Resource: solar://devices/{sn}/power/{date} ---

type powerResourceData struct {
	SerialNumber string                `json:"serial_number"`
	Date         string                `json:"date"`
	Interval     string                `json:"interval"`
	Points       []powerResourcePoint  `json:"points"`
	Summary      powerResourceSummary  `json:"summary"`
}

type powerResourcePoint struct {
	Time string  `json:"time"`
	PacW float64 `json:"pac_w"`
}

type powerResourceSummary struct {
	PeakPowerW      float64 `json:"peak_power_w"`
	PeakTime        string  `json:"peak_time"`
	TotalEnergyKWh  float64 `json:"total_energy_kwh"`
	ProductionHours float64 `json:"production_hours"`
}

func (r *ResourceRegistry) readDevicePower(ctx context.Context, uri, sn, dateInput string) (*ResourceReadResult, error) {
	dateTime, err := ParseFlexDate(dateInput)
	if err != nil {
		return nil, fmt.Errorf("invalid date %q: %w", dateInput, err)
	}
	dateStr := FormatDate(dateTime)

	resp, err := r.server.client.GetDevicePower(ctx, sn, apiclient.PowerParams{
		From:     dateStr,
		To:       dateStr,
		Interval: "5min",
		Fields:   "pac_w",
		Tz:       r.server.cfg.Timezone,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch power for %s on %s: %w", sn, dateStr, err)
	}

	// Parse the readings as simple floats (5min interval)
	points, peak, peakTime, totalKWh, prodHours := parsePowerReadingsForResource(resp)

	data := powerResourceData{
		SerialNumber: sn,
		Date:         dateStr,
		Interval:     "5min",
		Points:       points,
		Summary: powerResourceSummary{
			PeakPowerW:      peak,
			PeakTime:        peakTime,
			TotalEnergyKWh:  totalKWh,
			ProductionHours: prodHours,
		},
	}

	text, err := jsonText(data)
	if err != nil {
		return nil, err
	}
	return &ResourceReadResult{
		Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: text}},
	}, nil
}

// parsePowerReadingsForResource extracts simple pac_w values from raw 5min
// readings and computes summary statistics.
func parsePowerReadingsForResource(resp *apiclient.PowerResponse) (
	points []powerResourcePoint, peak float64, peakTime string, totalKWh float64, prodHours float64,
) {
	var firstProdTime, lastProdTime string

	for _, r := range resp.Readings {
		// For 5min interval, pac_w is a plain float in the JSON
		var pacW float64
		if r.PacW != nil {
			json.Unmarshal(r.PacW, &pacW)
		}

		points = append(points, powerResourcePoint{Time: r.Time, PacW: pacW})

		if pacW > peak {
			peak = pacW
			peakTime = r.Time
		}

		// Estimate energy: each 5min reading represents 5/60 hours
		totalKWh += (pacW / 1000.0) * (5.0 / 60.0)

		if pacW > 0 {
			if firstProdTime == "" {
				firstProdTime = r.Time
			}
			lastProdTime = r.Time
		}
	}

	// Estimate production hours from first to last non-zero reading
	if firstProdTime != "" && lastProdTime != "" {
		// Count non-zero intervals * 5 minutes
		var nonZeroCount int
		for _, p := range points {
			if p.PacW > 0 {
				nonZeroCount++
			}
		}
		prodHours = float64(nonZeroCount) * 5.0 / 60.0
	}

	return
}
```

### Wire Examples for Resources

**resources/list response:**
```json
{"jsonrpc":"2.0","id":3,"result":{"resources":[{"uri":"solar://plants","name":"Solar Plants","description":"List of all solar plants...","mimeType":"application/json"}]}}
```

**resources/templates/list response:**
```json
{"jsonrpc":"2.0","id":4,"result":{"resourceTemplates":[{"uriTemplate":"solar://devices/{sn}/current","name":"Device Live Readings","description":"Current inverter readings...","mimeType":"application/json"},{"uriTemplate":"solar://devices/{sn}/power/{date}","name":"Daily Power Profile","description":"Complete power production profile...","mimeType":"application/json"}]}}
```

**resources/read request and response:**
```
--> {"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"solar://devices/TLXABC12345/current"}}
<-- {"jsonrpc":"2.0","id":5,"result":{"contents":[{"uri":"solar://devices/TLXABC12345/current","mimeType":"application/json","text":"{\"serial_number\":\"TLXABC12345\",\"pac_w\":5120.5,...}"}]}}
```

---

## 9. Date Parsing

### `internal/mcp/dates.go`

```go
package mcp

import (
	"fmt"
	"strings"
	"time"
)

// ParseFlexDate resolves human-friendly date expressions to a time.Time.
//
// Supported formats:
//   - "today"      -> current date
//   - "yesterday"  -> current date - 1 day
//   - "last-week"  -> 7 days ago
//   - "last-month" -> 30 days ago
//   - "YYYY-MM-DD" -> exact date
//   - "YYYY-MM"    -> first of month (for monthly energy queries)
func ParseFlexDate(input string) (time.Time, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	normalized := strings.ToLower(strings.TrimSpace(input))

	switch normalized {
	case "today":
		return today, nil
	case "yesterday":
		return today.AddDate(0, 0, -1), nil
	case "last-week":
		return today.AddDate(0, 0, -7), nil
	case "last-month":
		return today.AddDate(0, -1, 0), nil
	}

	// Try YYYY-MM-DD
	if t, err := time.Parse("2006-01-02", normalized); err == nil {
		return t, nil
	}

	// Try YYYY-MM (first of month)
	if t, err := time.Parse("2006-01", normalized); err == nil {
		return t, nil
	}

	return time.Time{}, fmt.Errorf(
		"Invalid date '%s'. Use 'today', 'yesterday', 'last-week', 'last-month', or 'YYYY-MM-DD'.",
		input,
	)
}

// FormatDate converts a time.Time to YYYY-MM-DD for REST API query parameters.
func FormatDate(t time.Time) string {
	return t.Format("2006-01-02")
}
```

---

## 10. Response Formatting

### `internal/mcp/format.go`

All format functions produce structured plain text optimized for LLM consumption:
aligned tables, unit-suffixed field names, and summary lines.

```go
package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/gogrowatt/internal/apiclient"
)

// commaFloat formats a float with commas for thousands: 5389.9 -> "5,389.9"
func commaFloat(f float64, decimals int) string {
	format := fmt.Sprintf("%%.%df", decimals)
	s := fmt.Sprintf(format, f)

	parts := strings.Split(s, ".")
	intPart := parts[0]
	negative := false
	if strings.HasPrefix(intPart, "-") {
		negative = true
		intPart = intPart[1:]
	}

	// Insert commas
	n := len(intPart)
	if n <= 3 {
		if negative {
			intPart = "-" + intPart
		}
		if len(parts) > 1 {
			return intPart + "." + parts[1]
		}
		return intPart
	}

	var result []byte
	for i, c := range intPart {
		if i > 0 && (n-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	out := string(result)
	if negative {
		out = "-" + out
	}
	if len(parts) > 1 {
		out += "." + parts[1]
	}
	return out
}

// --- Tool 1: list_plants ---

func FormatPlantList(plants []apiclient.Plant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Solar Plants (%d found):\n", len(plants))

	for i, p := range plants {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, p.Name)
		fmt.Fprintf(&b, "   Plant ID:       %s\n", p.ID)
		fmt.Fprintf(&b, "   Location:       %s, %s (%.2f, %.2f)\n",
			p.City, p.Country, p.Latitude, p.Longitude)
		fmt.Fprintf(&b, "   System size:    %.1f kW\n", p.PeakPowerKW)
		fmt.Fprintf(&b, "   Current power:  %s W\n", commaFloat(p.CurrentPowerW, 1))
		fmt.Fprintf(&b, "   Today's energy: %.1f kWh\n", p.TodayEnergyKWh)
		fmt.Fprintf(&b, "   Total energy:   %s kWh\n", commaFloat(p.TotalEnergyKWh, 1))
		fmt.Fprintf(&b, "   Status:         %s\n", capitalize(p.Status))
	}
	return b.String()
}

// --- Tool 2: list_devices ---

func FormatDeviceList(plantID string, devices []apiclient.Device) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Devices for plant %s (%d found):\n", plantID, len(devices))

	for i, d := range devices {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, d.Name)
		fmt.Fprintf(&b, "   Serial:      %s\n", d.SerialNumber)
		fmt.Fprintf(&b, "   Type:        %s\n", d.Type)
		fmt.Fprintf(&b, "   Model:       %s\n", d.Model)
		fmt.Fprintf(&b, "   Status:      %s\n", capitalize(d.Status))
		fmt.Fprintf(&b, "   Last update: %s\n", d.LastUpdate)
	}
	return b.String()
}

// --- Tool 3: get_device_details ---

func FormatDeviceDetails(d *apiclient.DeviceDetail) string {
	c := d.Current
	str1Power := c.Vpv1V * c.Ipv1A
	str2Power := c.Vpv2V * c.Ipv2A

	var b strings.Builder
	fmt.Fprintf(&b, "Device %s - Live Readings\n", d.SerialNumber)
	b.WriteString("\nPower Output:\n")
	fmt.Fprintf(&b, "  AC Power (pac_w):       %s W\n", commaFloat(c.PacW, 1))
	fmt.Fprintf(&b, "  PV Power (ppv_w):       %s W\n", commaFloat(c.PpvW, 1))
	fmt.Fprintf(&b, "  Today Energy:           %.1f kWh\n", c.TodayEnergyKWh)
	fmt.Fprintf(&b, "  Total Energy:           %s kWh\n", commaFloat(c.TotalEnergyKWh, 1))

	b.WriteString("\nPV Strings:\n")
	fmt.Fprintf(&b, "  String 1 (vpv1_v / ipv1_a):  %.1f V @ %.2f A  (%s W)\n",
		c.Vpv1V, c.Ipv1A, commaFloat(str1Power, 0))
	fmt.Fprintf(&b, "  String 2 (vpv2_v / ipv2_a):  %.1f V @ %.2f A  (%s W)\n",
		c.Vpv2V, c.Ipv2A, commaFloat(str2Power, 0))

	b.WriteString("\nAC Output:\n")
	fmt.Fprintf(&b, "  Voltage (vac1_v):  %.1f V\n", c.Vac1V)
	fmt.Fprintf(&b, "  Current (iac1_a):  %.2f A\n", c.Iac1A)
	fmt.Fprintf(&b, "  Frequency:         %.2f Hz\n", c.FrequencyHz)

	b.WriteString("\nInverter:\n")
	fmt.Fprintf(&b, "  Temperature: %.1f C\n", c.TemperatureC)
	fmt.Fprintf(&b, "  Status:      %s\n", capitalize(d.Status))

	return b.String()
}

// --- Tool 4: get_current_power ---

func FormatCurrentPower(r *apiclient.LatestReading) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Current Solar Production (%s):\n", r.SerialNumber)
	fmt.Fprintf(&b, "\n  Power now (pac_w):  %s W\n", commaFloat(r.PacW, 1))
	fmt.Fprintf(&b, "  PV power (ppv_w):   %s W\n", commaFloat(r.PpvW, 1))
	fmt.Fprintf(&b, "  String 1:           %.1f V @ %.2f A\n", r.Vpv1V, r.Ipv1A)
	fmt.Fprintf(&b, "  String 2:           %.1f V @ %.2f A\n", r.Vpv2V, r.Ipv2A)
	fmt.Fprintf(&b, "\n  Time: %s\n", r.Time)
	return b.String()
}

// --- Tool 5: get_power_history ---

func FormatPowerHistory(resp *apiclient.PowerResponse, interval string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Power History - %s\n", resp.SerialNumber)

	if resp.From == resp.To || strings.HasPrefix(resp.To, strings.Split(resp.From, "T")[0]) {
		fmt.Fprintf(&b, "Date: %s | Interval: %s | Points: %d\n",
			extractDate(resp.From), interval, len(resp.Readings))
	} else {
		fmt.Fprintf(&b, "Range: %s to %s | Interval: %s\n",
			extractDate(resp.From), extractDate(resp.To), interval)
	}

	if interval == "5min" || interval == "15min" {
		// Simple table: time | pac_w
		b.WriteString("\nTime                           pac_w\n")
		b.WriteString("-----------------------------+--------\n")

		var peak float64
		var peakTime string
		var totalKWh float64
		intervalMinutes := 5.0
		if interval == "15min" {
			intervalMinutes = 15.0
		}

		for _, r := range resp.Readings {
			var pacW float64
			if r.PacW != nil {
				json.Unmarshal(r.PacW, &pacW)
			}
			fmt.Fprintf(&b, "%-29s %8s\n", r.Time, commaFloat(pacW, 1))
			if pacW > peak {
				peak = pacW
				peakTime = extractTime(r.Time)
			}
			totalKWh += (pacW / 1000.0) * (intervalMinutes / 60.0)
		}

		fmt.Fprintf(&b, "\nSummary: Peak %s W at %s | Day total ~%.1f kWh\n",
			commaFloat(peak, 1), peakTime, totalKWh)
	} else {
		// Aggregated table: time | Avg | Min | Max | Samples
		b.WriteString("\nTime                           Avg(W)  Min(W)  Max(W)  Samples\n")
		b.WriteString("-----------------------------+--------+-------+-------+-------\n")

		for _, r := range resp.Readings {
			var bucket apiclient.AggBucket
			if r.PacW != nil {
				json.Unmarshal(r.PacW, &bucket)
			}
			fmt.Fprintf(&b, "%-29s %7.1f %7.1f %7.1f %7d\n",
				r.Time, bucket.Avg, bucket.Min, bucket.Max, bucket.Samples)
		}
	}

	return b.String()
}

// --- Tool 6: get_energy_summary ---

func FormatEnergySummary(resp *apiclient.EnergyResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Energy Summary - %s\n", resp.SerialNumber)
	fmt.Fprintf(&b, "Period: %s to %s | Unit: %s\n\n", resp.From, resp.To, resp.Unit)

	if resp.Unit == "day" {
		b.WriteString("Date          Energy (kWh)\n")
		b.WriteString("-----------+-----------\n")
	} else {
		b.WriteString("Month        Energy (kWh)\n")
		b.WriteString("-----------+-----------\n")
	}

	for _, t := range resp.Totals {
		fmt.Fprintf(&b, "%-13s %8.1f\n", t.Date, t.EnergyKWh)
	}

	s := resp.Summary
	fmt.Fprintf(&b, "\nTotal:   %.1f kWh over %d days\n", s.TotalKWh, s.DaysWithData)
	fmt.Fprintf(&b, "Average: %.1f kWh/day\n", s.AverageKWh)
	if s.MaxDate != "" {
		fmt.Fprintf(&b, "Best day:  %s (%.1f kWh)\n", s.MaxDate, s.MaxKWh)
	}
	if s.MinDate != "" {
		fmt.Fprintf(&b, "Worst day: %s (%.1f kWh)\n", s.MinDate, s.MinKWh)
	}

	return b.String()
}

// --- Tool 7: get_production_stats ---

func FormatProductionStats(resp *apiclient.StatsResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Production Statistics - %s\n", resp.SerialNumber)
	fmt.Fprintf(&b, "Period: %s to %s (%d days analyzed)\n\n", resp.From, resp.To, resp.DaysAnalyzed)

	b.WriteString("Summary:\n")
	fmt.Fprintf(&b, "  Peak hour (avg):           %d:00\n", resp.PeakHour)
	fmt.Fprintf(&b, "  Peak power (avg_w):        %s W\n", commaFloat(resp.PeakPowerAvgW, 0))
	fmt.Fprintf(&b, "  Daily average production:  %.1f kWh\n", resp.DailyAverageKWh)
	fmt.Fprintf(&b, "  Total production:          %.1f kWh\n\n", resp.TotalProductionKWh)

	b.WriteString("Hourly Breakdown:\n")
	b.WriteString("Hour  min_w   max_w   avg_w    median_w  stddev_w  Days\n")
	b.WriteString("----+-------+-------+--------+---------+--------+----\n")

	for _, h := range resp.ByHour {
		fmt.Fprintf(&b, "%4d %7.1f %7.1f %8.1f %9.1f %8.1f %4d\n",
			h.Hour, h.MinW, h.MaxW, h.AvgW, h.MedianW, h.StddevW, h.SampleDays)
	}

	b.WriteString("\nNotes:\n")
	b.WriteString("- High stddev_w at a given hour indicates weather variability\n")

	return b.String()
}

// --- Tool 8: compare_days ---

func FormatCompareDays(sn string, dates []string, dayData []*apiclient.PowerResponse, interval string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Day Comparison - %s\n", sn)
	fmt.Fprintf(&b, "Dates: %s | Interval: %s\n\n", strings.Join(dates, ", "), interval)

	// Collect all unique hours/times and build a map per date
	type timeKey = string
	type dayValues = map[timeKey]float64

	allTimes := make([]string, 0)
	timeSet := make(map[string]bool)
	perDay := make([]dayValues, len(dates))

	for i, resp := range dayData {
		perDay[i] = make(dayValues)
		for _, r := range resp.Readings {
			// Extract just the time portion for alignment
			tKey := extractTime(r.Time)
			if !timeSet[tKey] {
				timeSet[tKey] = true
				allTimes = append(allTimes, tKey)
			}
			var val float64
			if r.PacW != nil {
				// Try float first (5min), then bucket (1h)
				if err := json.Unmarshal(r.PacW, &val); err != nil {
					var bucket apiclient.AggBucket
					json.Unmarshal(r.PacW, &bucket)
					val = bucket.Avg
				}
			}
			perDay[i][tKey] = val
		}
	}

	// Header
	b.WriteString("Hour  ")
	for _, d := range dates {
		label := monthDay(d)
		fmt.Fprintf(&b, "  %8s", label+"(W)")
	}
	if len(dates) == 2 {
		b.WriteString("    Delta")
	}
	b.WriteString("\n")
	b.WriteString("------+")
	for range dates {
		b.WriteString("---------+")
	}
	if len(dates) == 2 {
		b.WriteString("------")
	}
	b.WriteString("\n")

	// Data rows
	dayTotals := make([]float64, len(dates))
	for _, t := range allTimes {
		fmt.Fprintf(&b, "%-6s", t)
		vals := make([]float64, len(dates))
		hasAll := true
		for i := range dates {
			v, ok := perDay[i][t]
			vals[i] = v
			if ok {
				fmt.Fprintf(&b, " %8.1f ", v)
				dayTotals[i] += v
			} else {
				fmt.Fprintf(&b, "       -- ")
				hasAll = false
			}
		}
		if len(dates) == 2 && hasAll && vals[0] > 0 {
			delta := ((vals[1] - vals[0]) / vals[0]) * 100
			fmt.Fprintf(&b, " %+.1f%%", delta)
		} else if len(dates) == 2 {
			b.WriteString("    --")
		}
		b.WriteString("\n")
	}

	// Day totals
	b.WriteString("\nDay totals: ")
	for i, d := range dates {
		intervalMinutes := 60.0 // default 1h
		if interval == "5min" {
			intervalMinutes = 5.0
		} else if interval == "15min" {
			intervalMinutes = 15.0
		}
		kwh := dayTotals[i] / 1000.0 * (intervalMinutes / 60.0)
		if i > 0 {
			b.WriteString(" | ")
		}
		fmt.Fprintf(&b, "%s: ~%.1f kWh", monthDay(d), kwh)
	}
	b.WriteString("\n")

	return b.String()
}

// --- Helpers ---

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func extractDate(rfc string) string {
	if len(rfc) >= 10 {
		return rfc[:10]
	}
	return rfc
}

func extractTime(rfc string) string {
	// "2026-02-15T12:55:00-06:00" -> "12:55"
	if idx := strings.Index(rfc, "T"); idx >= 0 {
		timePart := rfc[idx+1:]
		if len(timePart) >= 5 {
			return timePart[:5]
		}
	}
	return rfc
}

func monthDay(dateStr string) string {
	// "2026-02-15" -> "Feb-15"
	months := []string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	if len(dateStr) >= 10 {
		var m, d int
		fmt.Sscanf(dateStr[5:10], "%d-%d", &m, &d)
		if m >= 1 && m <= 12 {
			return fmt.Sprintf("%s-%02d", months[m], d)
		}
	}
	return dateStr
}

// round is unused but available for formatting needs.
func round(f float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(f*pow) / pow
}
```

---

## 11. Error Handling

### `internal/mcp/errors.go`

```go
package mcp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gogrowatt/internal/apiclient"
)

// apiErrorResult converts a REST API error into a user-friendly MCP tool
// error result. It maps HTTP status codes to helpful messages with
// recovery suggestions.
func (s *Server) apiErrorResult(action string, err error) ToolResult {
	var apiErr *apiclient.APIErrorResponse
	if errors.As(err, &apiErr) {
		msg := mapAPIError(action, apiErr)
		return errorResult(msg)
	}

	// Connection error (REST API unreachable)
	if strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "dial tcp") ||
		strings.Contains(err.Error(), "no such host") {
		return errorResult(fmt.Sprintf(
			"Cannot reach the REST API at %s. Is gogrowatt-api running? Original error: %s",
			s.cfg.APIBaseURL, err.Error()))
	}

	// Generic error
	return errorResult(fmt.Sprintf("Failed to %s: %s", action, err.Error()))
}

func mapAPIError(action string, apiErr *apiclient.APIErrorResponse) string {
	switch apiErr.StatusCode {
	case 400:
		return fmt.Sprintf("Invalid request to %s: %s. Check date format (YYYY-MM-DD) and parameters.",
			action, apiErr.Message)
	case 404:
		switch apiErr.Code {
		case "NOT_FOUND":
			return fmt.Sprintf("Not found: %s. Use list_plants or list_devices to see available IDs.",
				apiErr.Message)
		case "NO_DATA":
			return fmt.Sprintf("No data for this date range. %s. The fetcher may not have collected data yet.",
				apiErr.Message)
		default:
			return fmt.Sprintf("Not found when trying to %s: %s", action, apiErr.Message)
		}
	case 429:
		return "Rate limited by the REST API. Wait a moment and retry."
	case 502:
		return "Database error on the server side. The REST API could not query PostgreSQL."
	case 504:
		return "Query timed out. Try a shorter date range or coarser interval (e.g., '1d' instead of '5min')."
	default:
		return fmt.Sprintf("REST API error %d (%s): %s",
			apiErr.StatusCode, apiErr.Code, apiErr.Message)
	}
}
```

### Error Response Wire Examples

**Tool error (device not found):**
```json
{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"Error: Not found: Device 'NOEXIST999' not found. Use list_plants or list_devices to see available IDs."}],"isError":true}}
```

**Tool error (invalid date):**
```json
{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"Error: Invalid date 'not-a-date'. Use 'today', 'yesterday', 'last-week', 'last-month', or 'YYYY-MM-DD'."}],"isError":true}}
```

**Tool error (API unreachable):**
```json
{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"Error: Cannot reach the REST API at http://localhost:8080. Is gogrowatt-api running? Original error: dial tcp [::1]:8080: connection refused"}],"isError":true}}
```

**Protocol error (unknown method):**
```json
{"jsonrpc":"2.0","id":8,"error":{"code":-32601,"message":"unknown method: foo/bar"}}
```

---

## 12. Logging

All logging goes to stderr using Go's `log/slog` structured logger. stdout is
reserved exclusively for the JSON-RPC transport.

```go
// In main.go:
logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelInfo,
}))

// Usage throughout server:
s.log.Info("tools/call", "tool", params.Name)
s.log.Debug("received request", "method", req.Method)
s.log.Error("REST API error", "action", action, "err", err)
```

Log levels:
- **Debug**: Every JSON-RPC message received/sent, REST API request details
- **Info**: Startup config, initialize handshake, tool calls, resource reads
- **Error**: REST API failures, JSON parse errors, unexpected conditions

---

## 13. Unit Test Strategy

### `internal/mcp/dates_test.go`

```go
package mcp

import (
	"testing"
	"time"
)

func TestParseFlexDate(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	tests := []struct {
		input   string
		want    time.Time
		wantErr bool
	}{
		{input: "today", want: today},
		{input: "TODAY", want: today},
		{input: "  today  ", want: today},
		{input: "yesterday", want: today.AddDate(0, 0, -1)},
		{input: "last-week", want: today.AddDate(0, 0, -7)},
		{input: "last-month", want: today.AddDate(0, -1, 0)},
		{input: "2026-02-15", want: time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)},
		{input: "2026-02", want: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{input: "", wantErr: true},
		{input: "not-a-date", wantErr: true},
		{input: "02-15-2026", wantErr: true},
		{input: "next-week", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseFlexDate(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseFlexDate(%q) expected error, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFlexDate(%q) unexpected error: %v", tt.input, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("ParseFlexDate(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatDate(t *testing.T) {
	d := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	got := FormatDate(d)
	if got != "2026-02-15" {
		t.Errorf("FormatDate = %q, want %q", got, "2026-02-15")
	}
}
```

### `internal/mcp/format_test.go`

```go
package mcp

import (
	"strings"
	"testing"

	"github.com/gogrowatt/internal/apiclient"
)

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q, got:\n%s", substr, s)
	}
}

func TestFormatPlantList(t *testing.T) {
	plants := []apiclient.Plant{
		{
			ID: "12345", Name: "Home Solar", City: "Austin", Country: "US",
			Latitude: 30.27, Longitude: -97.74, PeakPowerKW: 8.4,
			CurrentPowerW: 5121, TodayEnergyKWh: 18.7, TotalEnergyKWh: 12450.3,
			Status: "online",
		},
	}
	out := FormatPlantList(plants)
	assertContains(t, out, "Solar Plants (1 found)")
	assertContains(t, out, "Home Solar")
	assertContains(t, out, "12345")
	assertContains(t, out, "5,121.0 W")
	assertContains(t, out, "Online")
}

func TestFormatDeviceDetails(t *testing.T) {
	d := &apiclient.DeviceDetail{
		SerialNumber: "TLXABC12345",
		Status:       "online",
		Current: apiclient.DeviceCurrent{
			PacW: 5120.5, PpvW: 5250.0,
			Vpv1V: 324.5, Vpv2V: 318.2,
			Ipv1A: 8.12, Ipv2A: 8.35,
			Vac1V: 243.1, Iac1A: 21.05,
			FrequencyHz: 60.01, TemperatureC: 42.3,
			TodayEnergyKWh: 18.7, TotalEnergyKWh: 12450.3,
		},
	}
	out := FormatDeviceDetails(d)
	assertContains(t, out, "TLXABC12345")
	assertContains(t, out, "pac_w")
	assertContains(t, out, "ppv_w")
	assertContains(t, out, "vpv1_v")
	assertContains(t, out, "ipv1_a")
	assertContains(t, out, "vac1_v")
	assertContains(t, out, "iac1_a")
	assertContains(t, out, "5,120.5")
	assertContains(t, out, "42.3 C")
}

func TestFormatCurrentPower(t *testing.T) {
	r := &apiclient.LatestReading{
		SerialNumber: "TLXABC12345",
		Time:         "2026-02-15T12:55:00-06:00",
		PacW: 5390, PpvW: 5450,
		Vpv1V: 335.2, Vpv2V: 330.1,
		Ipv1A: 8.25, Ipv2A: 8.40,
	}
	out := FormatCurrentPower(r)
	assertContains(t, out, "Current Solar Production")
	assertContains(t, out, "pac_w")
	assertContains(t, out, "5,390.0")
}

func TestCommaFloat(t *testing.T) {
	tests := []struct {
		input    float64
		decimals int
		want     string
	}{
		{5389.9, 1, "5,389.9"},
		{0.0, 1, "0.0"},
		{12450.3, 1, "12,450.3"},
		{999.0, 0, "999"},
		{1000.0, 0, "1,000"},
		{1234567.89, 2, "1,234,567.89"},
	}
	for _, tt := range tests {
		got := commaFloat(tt.input, tt.decimals)
		if got != tt.want {
			t.Errorf("commaFloat(%v, %d) = %q, want %q", tt.input, tt.decimals, got, tt.want)
		}
	}
}
```

### `internal/mcp/transport_test.go`

```go
package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTransportReadWrite(t *testing.T) {
	// Prepare a request on stdin
	reqObj := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]interface{}{},
	}
	reqBytes, _ := json.Marshal(reqObj)
	stdin := bytes.NewReader(append(reqBytes, '\n'))

	var stdout bytes.Buffer
	tr := NewTransport(stdin, &stdout)

	// Read
	req, err := tr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if req.Method != "initialize" {
		t.Errorf("Method = %q, want %q", req.Method, "initialize")
	}

	// Write
	err = tr.SendResult(req.ID, map[string]string{"status": "ok"})
	if err != nil {
		t.Fatalf("SendResult: %v", err)
	}

	// Verify output
	outStr := stdout.String()
	if !strings.Contains(outStr, `"result"`) {
		t.Errorf("output missing result: %s", outStr)
	}
	if !strings.HasSuffix(outStr, "\n") {
		t.Error("output should end with newline")
	}
}

func TestTransportNotification(t *testing.T) {
	// Notifications have no ID
	msg := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n"
	stdin := strings.NewReader(msg)
	tr := NewTransport(stdin, &bytes.Buffer{})

	req, err := tr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if req.ID != nil {
		t.Error("notification should have nil ID")
	}
	if req.Method != "notifications/initialized" {
		t.Errorf("Method = %q", req.Method)
	}
}
```

### `internal/mcp/tools_test.go`

```go
package mcp

import (
	"testing"
)

func TestToolRegistryDefinitions(t *testing.T) {
	// Create a server with nil client (just testing registry)
	s := &Server{
		cfg: ServerConfig{},
	}
	r := NewToolRegistry(s)

	defs := r.Definitions()
	if len(defs) != 8 {
		t.Errorf("expected 8 tool definitions, got %d", len(defs))
	}

	expectedNames := map[string]bool{
		"list_plants":          false,
		"list_devices":         false,
		"get_device_details":   false,
		"get_current_power":    false,
		"get_power_history":    false,
		"get_energy_summary":   false,
		"get_production_stats": false,
		"compare_days":         false,
	}

	for _, def := range defs {
		if _, ok := expectedNames[def.Name]; !ok {
			t.Errorf("unexpected tool: %s", def.Name)
		}
		expectedNames[def.Name] = true

		if def.Description == "" {
			t.Errorf("tool %s has empty description", def.Name)
		}
		if def.InputSchema.Type != "object" {
			t.Errorf("tool %s schema type = %q, want 'object'", def.Name, def.InputSchema.Type)
		}
	}

	for name, found := range expectedNames {
		if !found {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestToolRegistryUnknownTool(t *testing.T) {
	s := &Server{cfg: ServerConfig{}}
	r := NewToolRegistry(s)

	result := r.Call(nil, "nonexistent", nil)
	if !result.IsError {
		t.Error("expected error for unknown tool")
	}
}
```

---

## 14. Integration Test Strategy

### `internal/mcp/integration_test.go`

Integration tests start the MCP binary as a subprocess, communicate over
stdin/stdout with JSON-RPC, and verify end-to-end behavior against a running
REST API with test data.

```go
//go:build integration

package mcp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
)

var (
	mcpBinary string
	apiURL    string
)

func TestMain(m *testing.M) {
	// 1. Build gogrowatt-mcp binary
	// 2. Set apiURL from TEST_API_URL env var
	// 3. Run tests
	apiURL = os.Getenv("TEST_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	os.Exit(m.Run())
}

type mcpProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func startMCP(t *testing.T) *mcpProcess {
	t.Helper()
	cmd := exec.Command("./gogrowatt-mcp")
	cmd.Env = append(os.Environ(),
		"GOGROWATT_API_URL="+apiURL,
		"GROWATT_DEVICE_SN=TESTDEV001",
		"GROWATT_PLANT_ID=99999",
	)

	stdin, _ := cmd.StdinPipe()
	stdoutPipe, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start mcp: %v", err)
	}

	return &mcpProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdoutPipe),
	}
}

func (p *mcpProcess) send(t *testing.T, id int, method string, params interface{}) map[string]interface{} {
	t.Helper()
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	data, _ := json.Marshal(req)
	p.stdin.Write(append(data, '\n'))

	line, err := p.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp map[string]interface{}
	json.Unmarshal([]byte(line), &resp)
	return resp
}

func (p *mcpProcess) notify(t *testing.T, method string) {
	t.Helper()
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":{}}`, method)
	p.stdin.Write([]byte(msg + "\n"))
}

func (p *mcpProcess) close() {
	p.stdin.Close()
	p.cmd.Wait()
}

func extractText(resp map[string]interface{}) string {
	result, _ := resp["result"].(map[string]interface{})
	content, _ := result["content"].([]interface{})
	if len(content) > 0 {
		block, _ := content[0].(map[string]interface{})
		return block["text"].(string)
	}
	return ""
}

// --- Test Cases ---

func TestIntegration_InitializeHandshake(t *testing.T) {
	p := startMCP(t)
	defer p.close()

	resp := p.send(t, 1, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "test", "version": "1.0"},
	})

	result := resp["result"].(map[string]interface{})
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocol version mismatch")
	}
	info := result["serverInfo"].(map[string]interface{})
	if info["name"] != "gogrowatt-mcp" {
		t.Errorf("server name = %v", info["name"])
	}

	p.notify(t, "notifications/initialized")
}

func TestIntegration_ToolsList(t *testing.T) {
	p := startMCP(t)
	defer p.close()

	// Initialize first
	p.send(t, 1, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "test", "version": "1.0"},
	})
	p.notify(t, "notifications/initialized")

	resp := p.send(t, 2, "tools/list", map[string]interface{}{})
	result := resp["result"].(map[string]interface{})
	tools := result["tools"].([]interface{})
	if len(tools) != 8 {
		t.Errorf("expected 8 tools, got %d", len(tools))
	}
}

func TestIntegration_GetCurrentPower(t *testing.T) {
	p := startMCP(t)
	defer p.close()

	p.send(t, 1, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "test", "version": "1.0"},
	})
	p.notify(t, "notifications/initialized")

	resp := p.send(t, 10, "tools/call", map[string]interface{}{
		"name":      "get_current_power",
		"arguments": map[string]interface{}{},
	})

	text := extractText(resp)
	if text == "" {
		t.Fatal("empty response text")
	}
	// Should use default device from env
	if !contains(text, "Current Solar Production") && !contains(text, "Error") {
		t.Errorf("unexpected response: %s", text[:min(len(text), 200)])
	}
}

func TestIntegration_ErrorHandling(t *testing.T) {
	p := startMCP(t)
	defer p.close()

	p.send(t, 1, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "test", "version": "1.0"},
	})
	p.notify(t, "notifications/initialized")

	// Bad date
	resp := p.send(t, 20, "tools/call", map[string]interface{}{
		"name": "get_power_history",
		"arguments": map[string]interface{}{
			"sn":   "TESTDEV001",
			"from": "not-a-date",
		},
	})

	result := resp["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Error("expected isError=true for invalid date")
	}
	text := extractText(resp)
	if !contains(text, "Invalid date") {
		t.Errorf("expected helpful error message, got: %s", text)
	}
}

func TestIntegration_UnknownMethod(t *testing.T) {
	p := startMCP(t)
	defer p.close()

	resp := p.send(t, 1, "foo/bar", nil)
	if resp["error"] == nil {
		t.Error("expected JSON-RPC error for unknown method")
	}
	errObj := resp["error"].(map[string]interface{})
	if errObj["code"].(float64) != -32601 {
		t.Errorf("error code = %v, want -32601", errObj["code"])
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) > len(substr) && searchString(s, substr))
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

### Running Tests

```bash
# Unit tests
go test ./internal/mcp/... -v

# Integration tests (requires REST API running with test data)
go build -o gogrowatt-mcp ./cmd/gogrowatt-mcp/
TEST_API_URL=http://localhost:8080 go test ./internal/mcp/... -v -tags=integration -timeout 60s
```

---

## 15. Client Configuration

### Claude Desktop

File: `~/.config/claude/claude_desktop_config.json` (Linux) or
`~/Library/Application Support/Claude/claude_desktop_config.json` (macOS):

```json
{
  "mcpServers": {
    "solar": {
      "command": "/path/to/gogrowatt-mcp",
      "env": {
        "GOGROWATT_API_URL": "http://localhost:8080",
        "GROWATT_PLANT_ID": "12345",
        "GROWATT_DEVICE_SN": "TLXABC12345"
      }
    }
  }
}
```

### Claude Code

Project `.mcp.json`:

```json
{
  "mcpServers": {
    "solar": {
      "command": "/path/to/gogrowatt-mcp",
      "env": {
        "GOGROWATT_API_URL": "http://localhost:8080"
      }
    }
  }
}
```

CLI:

```bash
claude mcp add solar /path/to/gogrowatt-mcp \
  -e GOGROWATT_API_URL=http://localhost:8080 \
  -e GROWATT_DEVICE_SN=TLXABC12345
```

### Building

```bash
cd /gogrowatt
go build -o gogrowatt-mcp ./cmd/gogrowatt-mcp/
```

### Docker

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY . .
RUN go build -o /gogrowatt-mcp ./cmd/gogrowatt-mcp/

FROM alpine:3.19
COPY --from=builder /gogrowatt-mcp /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/gogrowatt-mcp"]
```

```bash
docker run --rm -i \
  -e GOGROWATT_API_URL=http://gogrowatt-api:8080 \
  gogrowatt-mcp
```

---

## Appendix: Complete Wire Protocol Session

This shows a full session from handshake through tool calls:

```
--> {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claude-desktop","version":"1.0"}}}
<-- {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{},"resources":{}},"serverInfo":{"name":"gogrowatt-mcp","version":"0.1.0"}}}

--> {"jsonrpc":"2.0","method":"notifications/initialized","params":{}}

--> {"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
<-- {"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"list_plants","description":"List all solar plants...","inputSchema":{"type":"object","properties":{},"required":[]}},{"name":"list_devices",...},{"name":"get_device_details",...},{"name":"get_current_power",...},{"name":"get_power_history",...},{"name":"get_energy_summary",...},{"name":"get_production_stats",...},{"name":"compare_days",...}]}}

--> {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_current_power","arguments":{}}}
<-- {"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"Current Solar Production (TLXABC12345):\n\n  Power now (pac_w):  5,390.0 W\n  PV power (ppv_w):   5,450.0 W\n  String 1:           335.2 V @ 8.25 A\n  String 2:           330.1 V @ 8.40 A\n\n  Time: 2026-02-15T12:55:00-06:00"}]}}

--> {"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_power_history","arguments":{"sn":"TLXABC12345","from":"yesterday","interval":"1h"}}}
<-- {"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"Power History - TLXABC12345\nDate: 2026-02-14 | Interval: 1h | Points: 12\n\nTime                           Avg(W)  Min(W)  Max(W)  Samples\n-----------------------------+--------+-------+-------+-------\n2026-02-14T07:00:00-06:00       56.2     0.0   120.9      7\n..."}]}}

--> {"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"solar://plants"}}
<-- {"jsonrpc":"2.0","id":5,"result":{"contents":[{"uri":"solar://plants","mimeType":"application/json","text":"{\"plants\":[{\"id\":\"12345\",\"name\":\"Home Solar\",...}]}"}]}}

--> {"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"solar://devices/TLXABC12345/power/today"}}
<-- {"jsonrpc":"2.0","id":6,"result":{"contents":[{"uri":"solar://devices/TLXABC12345/power/today","mimeType":"application/json","text":"{\"serial_number\":\"TLXABC12345\",\"date\":\"2026-02-15\",\"interval\":\"5min\",\"points\":[...],\"summary\":{\"peak_power_w\":5389.9,...}}"}]}}
```
