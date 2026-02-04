package growatt

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ListDevices returns all devices for a plant
func (c *Client) ListDevices(ctx context.Context, plantID string) ([]Device, error) {
	params := url.Values{}
	params.Set("plant_id", plantID)

	body, err := c.get(ctx, "device/list", params)
	if err != nil {
		return nil, err
	}

	data, err := parseResponse[DeviceListData](body)
	if err != nil {
		return nil, err
	}

	return data.Devices, nil
}

// GetMINInverterDetails returns details for a MIN/TLX inverter
func (c *Client) GetMINInverterDetails(ctx context.Context, serial string) (*MINInverterData, error) {
	params := url.Values{}
	params.Set("tlx_sn", serial)

	body, err := c.get(ctx, "device/tlx/tlx_data_info", params)
	if err != nil {
		return nil, err
	}

	return parseResponse[MINInverterData](body)
}

// MINHistoryRequest is the request body for MIN inverter historical data
type MINHistoryRequest struct {
	DeviceSN   string
	StartDate  string
	EndDate    string
	TimezoneID string
	Page       int
	PerPage    int
}

// ToFormData converts the request to URL-encoded form data
func (r MINHistoryRequest) ToFormData() url.Values {
	data := url.Values{}
	data.Set("tlx_sn", r.DeviceSN)
	data.Set("start_date", r.StartDate)
	data.Set("end_date", r.EndDate)
	data.Set("timezone_id", r.TimezoneID)
	data.Set("page", fmt.Sprintf("%d", r.Page))
	data.Set("perpage", fmt.Sprintf("%d", r.PerPage))
	return data
}

// MINHistoryDataPoint represents a single data point in MIN history
type MINHistoryDataPoint struct {
	Time  string    `json:"time"`
	Pac   FlexFloat `json:"pac"`   // AC Power (W)
	Ppv   FlexFloat `json:"ppv"`   // PV Power (W)
	Vpv1  FlexFloat `json:"vpv1"`  // PV1 Voltage
	Vpv2  FlexFloat `json:"vpv2"`  // PV2 Voltage
	Ipv1  FlexFloat `json:"ipv1"`  // PV1 Current
	Ipv2  FlexFloat `json:"ipv2"`  // PV2 Current
	Vac1  FlexFloat `json:"vac1"`  // AC Voltage
	Iac1  FlexFloat `json:"iac1"`  // AC Current
}

// MINHistoryResponse is the response from MIN historical data endpoint
type MINHistoryResponse struct {
	Count int                   `json:"count"`
	Datas []MINHistoryDataPoint `json:"datas"`
}

// GetMINInverterHistory returns historical data for a MIN/TLX inverter
// Note: Maximum date range is 7 days
func (c *Client) GetMINInverterHistory(ctx context.Context, serial string, date time.Time, timezone string) (*PowerData, error) {
	if timezone == "" {
		timezone = "US/Central" // Default timezone
	}

	dateStr := date.Format("2006-01-02")

	reqBody := MINHistoryRequest{
		DeviceSN:   serial,
		StartDate:  dateStr,
		EndDate:    dateStr,
		TimezoneID: timezone,
		Page:       1,
		PerPage:    100, // API max is 100
	}

	body, err := c.postForm(ctx, "device/tlx/tlx_data", reqBody.ToFormData())
	if err != nil {
		return nil, err
	}

	histResp, err := parseResponse[MINHistoryResponse](body)
	if err != nil {
		return nil, err
	}

	// Convert to PowerData format
	powers := make([]PowerDataPoint, 0, len(histResp.Datas))
	for _, d := range histResp.Datas {
		timeStr := normalizeTime(d.Time)
		// Use Pac (AC power) as the power value
		powers = append(powers, PowerDataPoint{
			Time:  timeStr,
			Power: d.Pac.Float64(),
		})
	}

	// Sort by time
	sort.Slice(powers, func(i, j int) bool {
		return powers[i].Time < powers[j].Time
	})

	return &PowerData{
		PlantID: FlexString(serial),
		Date:    dateStr,
		Powers:  powers,
	}, nil
}

// GetMINInverterHistoryRange fetches historical data for a date range
// Note: API has 7-day maximum per request, this method handles pagination
func (c *Client) GetMINInverterHistoryRange(ctx context.Context, serial string, from, to time.Time, timezone string) ([]PowerData, error) {
	var results []PowerData

	current := from
	for !current.After(to) {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		data, err := c.GetMINInverterHistory(ctx, serial, current, timezone)
		if err != nil {
			return results, fmt.Errorf("fetching MIN history for %s: %w", current.Format("2006-01-02"), err)
		}

		results = append(results, *data)
		current = current.AddDate(0, 0, 1)
	}

	return results, nil
}

// postForm performs a POST request with form-encoded body
func (c *Client) postForm(ctx context.Context, endpoint string, data url.Values) ([]byte, error) {
	c.enforceRateLimit()

	fullURL := c.baseURL + endpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("token", c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return respBody, nil
}
