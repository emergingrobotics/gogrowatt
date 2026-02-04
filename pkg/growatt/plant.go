package growatt

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ListPlants returns all plants associated with the account
func (c *Client) ListPlants(ctx context.Context) ([]Plant, error) {
	body, err := c.get(ctx, "plant/list", nil)
	if err != nil {
		return nil, err
	}

	data, err := parseResponse[PlantListData](body)
	if err != nil {
		return nil, err
	}

	return data.Plants, nil
}

// GetPlantDetails returns details for a specific plant
func (c *Client) GetPlantDetails(ctx context.Context, plantID string) (*Plant, error) {
	params := url.Values{}
	params.Set("plant_id", plantID)

	body, err := c.get(ctx, "plant/details", params)
	if err != nil {
		return nil, err
	}

	return parseResponse[Plant](body)
}

// GetPlantData returns energy overview for a plant
func (c *Client) GetPlantData(ctx context.Context, plantID string) (*PlantData, error) {
	params := url.Values{}
	params.Set("plant_id", plantID)

	body, err := c.get(ctx, "plant/data", params)
	if err != nil {
		return nil, err
	}

	return parseResponse[PlantData](body)
}

// GetPlantPower returns 5-minute interval power data for a specific date
func (c *Client) GetPlantPower(ctx context.Context, plantID string, date time.Time) (*PowerData, error) {
	params := url.Values{}
	params.Set("plant_id", plantID)
	params.Set("date", date.Format("2006-01-02"))

	body, err := c.get(ctx, "plant/power", params)
	if err != nil {
		return nil, err
	}

	// Parse the raw response format
	raw, err := parseResponse[PowerDataRaw](body)
	if err != nil {
		return nil, err
	}

	// Convert to sorted slice
	powers := make([]PowerDataPoint, 0, len(raw.Powers))
	for timeStr, power := range map[string]float64(raw.Powers) {
		powers = append(powers, PowerDataPoint{
			Time:  timeStr,
			Power: power,
		})
	}

	// Sort by time
	sort.Slice(powers, func(i, j int) bool {
		return powers[i].Time < powers[j].Time
	})

	return &PowerData{
		PlantID: FlexString(raw.PlantID),
		Date:    date.Format("2006-01-02"),
		Powers:  powers,
	}, nil
}

// GetPlantPowerRange fetches power data for a date range
func (c *Client) GetPlantPowerRange(ctx context.Context, plantID string, from, to time.Time) ([]PowerData, error) {
	var results []PowerData

	current := from
	for !current.After(to) {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		data, err := c.GetPlantPower(ctx, plantID, current)
		if err != nil {
			return results, fmt.Errorf("fetching power for %s: %w", current.Format("2006-01-02"), err)
		}

		results = append(results, *data)
		current = current.AddDate(0, 0, 1)
	}

	return results, nil
}

// GetPlantEnergy returns historical energy data
func (c *Client) GetPlantEnergy(ctx context.Context, plantID, startDate, endDate string, timeUnit TimeUnit) (*EnergyData, error) {
	params := url.Values{}
	params.Set("plant_id", plantID)
	params.Set("start_date", startDate)
	params.Set("end_date", endDate)
	params.Set("time_unit", string(timeUnit))

	body, err := c.get(ctx, "plant/energy", params)
	if err != nil {
		return nil, err
	}

	// Parse the raw response format
	raw, err := parseResponse[EnergyDataRaw](body)
	if err != nil {
		return nil, err
	}

	// Convert map to sorted slice
	datas := make([]EnergyDataPoint, 0, len(raw.Datas))
	for dateStr, energy := range raw.Datas {
		datas = append(datas, EnergyDataPoint{
			Date:   dateStr,
			Energy: energy,
		})
	}

	// Sort by date
	sort.Slice(datas, func(i, j int) bool {
		return datas[i].Date < datas[j].Date
	})

	return &EnergyData{
		PlantID: FlexString(raw.PlantID),
		Datas:   datas,
	}, nil
}

// ParsePowerData converts raw power data to parsed format with hour/minute
func ParsePowerData(data *PowerData) ([]ParsedPowerData, error) {
	date, err := time.Parse("2006-01-02", data.Date)
	if err != nil {
		return nil, fmt.Errorf("parsing date %s: %w", data.Date, err)
	}

	result := make([]ParsedPowerData, 0, len(data.Powers))
	for _, p := range data.Powers {
		timeStr := p.Time

		// Handle full datetime format "YYYY-MM-DD HH:MM"
		if strings.Contains(timeStr, " ") {
			parts := strings.Split(timeStr, " ")
			if len(parts) == 2 {
				timeStr = parts[1] // Extract just the time part
			}
		}

		parts := strings.Split(timeStr, ":")
		if len(parts) < 2 {
			continue
		}

		hour, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}

		minute, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}

		result = append(result, ParsedPowerData{
			Date:   date,
			Time:   timeStr, // Store just the time part
			Power:  p.Power,
			Hour:   hour,
			Minute: minute,
		})
	}

	return result, nil
}
