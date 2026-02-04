package growatt

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// FlexFloat handles JSON numbers that may be strings or floats
type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(data []byte) error {
	// Try as number first
	var num float64
	if err := json.Unmarshal(data, &num); err == nil {
		*f = FlexFloat(num)
		return nil
	}

	// Try as string
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}

	if str == "" {
		*f = 0
		return nil
	}

	num, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return err
	}
	*f = FlexFloat(num)
	return nil
}

func (f FlexFloat) Float64() float64 {
	return float64(f)
}

// FlexString handles JSON values that may be strings or numbers
type FlexString string

func (s *FlexString) UnmarshalJSON(data []byte) error {
	// Try as string first
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = FlexString(str)
		return nil
	}

	// Try as number (int or float)
	var num json.Number
	if err := json.Unmarshal(data, &num); err == nil {
		*s = FlexString(num.String())
		return nil
	}

	// Try as raw number
	var f float64
	if err := json.Unmarshal(data, &f); err == nil {
		*s = FlexString(strconv.FormatFloat(f, 'f', -1, 64))
		return nil
	}

	return nil
}

func (s FlexString) String() string {
	return string(s)
}

// TimeUnit represents the time unit for energy queries
type TimeUnit string

const (
	TimeUnitDay   TimeUnit = "day"
	TimeUnitMonth TimeUnit = "month"
)

// Response is the generic API response wrapper
type Response[T any] struct {
	ErrorCode int    `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
	Data      T      `json:"data"`
}

// Plant represents a power station
type Plant struct {
	PlantID       FlexString `json:"plant_id"`
	PlantName     string     `json:"plant_name"`
	PlantType     int        `json:"plant_type"`
	Country       string     `json:"country"`
	City          string     `json:"city"`
	Latitude      FlexFloat  `json:"latitude"`
	Longitude     FlexFloat  `json:"longitude"`
	PeakPower     FlexFloat  `json:"peak_power"`
	CurrentPower  FlexFloat  `json:"current_power"`
	TodayEnergy   FlexFloat  `json:"today_energy"`
	TotalEnergy   FlexFloat  `json:"total_energy"`
	CreateDate    string     `json:"create_date"`
	Status        int        `json:"status"`
	FormulaCoal   FlexFloat  `json:"formula_coal"`
	FormulaCO2    FlexFloat  `json:"formula_co2"`
	FormulaMoney  FlexFloat  `json:"formula_money"`
	FormulaTree   FlexFloat  `json:"formula_tree"`
	MoneyUnit     string     `json:"money_unit"`
	MoneyUnitText string     `json:"money_unit_text"`
}

// PlantListData is the response data for plant list
type PlantListData struct {
	Count  int     `json:"count"`
	Plants []Plant `json:"plants"`
}

// PlantData represents energy overview data
type PlantData struct {
	PlantID        FlexString `json:"plant_id"`
	TodayEnergy    FlexFloat  `json:"today_energy"`
	TotalEnergy    FlexFloat  `json:"total_energy"`
	CurrentPower   FlexFloat  `json:"current_power"`
	PeakPowerToday FlexFloat  `json:"peak_power_today"`
	MonthEnergy    FlexFloat  `json:"month_energy"`
	YearEnergy     FlexFloat  `json:"year_energy"`
}

// PowerDataPoint represents a single 5-minute power reading
type PowerDataPoint struct {
	Time  string  `json:"time"`
	Power float64 `json:"power"`
}

// PowerData represents power data for a single day
type PowerData struct {
	PlantID FlexString       `json:"plant_id"`
	Date    string           `json:"date"`
	Powers  []PowerDataPoint `json:"powers"`
}

// PowerDataRaw is the raw API response format for power data
type PowerDataRaw struct {
	PlantID FlexString `json:"plant_id"`
	Count   int        `json:"count"`
	Powers  FlexPowers `json:"powers"`
}

// FlexPowers handles powers data that may be a map or an array
type FlexPowers map[string]float64

func (p *FlexPowers) UnmarshalJSON(data []byte) error {
	// Try as map first (original expected format)
	var m map[string]float64
	if err := json.Unmarshal(data, &m); err == nil {
		result := make(map[string]float64)
		for timeStr, power := range m {
			result[normalizeTime(timeStr)] = power
		}
		*p = FlexPowers(result)
		return nil
	}

	// Try as array of objects with time/power fields
	var arr []struct {
		Time  string    `json:"time"`
		Power FlexFloat `json:"power"`
	}
	if err := json.Unmarshal(data, &arr); err == nil {
		result := make(map[string]float64)
		for _, item := range arr {
			result[normalizeTime(item.Time)] = item.Power.Float64()
		}
		*p = FlexPowers(result)
		return nil
	}

	// Try as array of arrays [[time, power], ...]
	var arr2 [][]json.RawMessage
	if err := json.Unmarshal(data, &arr2); err == nil {
		result := make(map[string]float64)
		for _, item := range arr2 {
			if len(item) >= 2 {
				var timeStr string
				var power float64
				json.Unmarshal(item[0], &timeStr)
				json.Unmarshal(item[1], &power)
				if timeStr != "" {
					result[normalizeTime(timeStr)] = power
				}
			}
		}
		*p = FlexPowers(result)
		return nil
	}

	// Empty or null
	*p = make(map[string]float64)
	return nil
}

// normalizeTime extracts HH:MM from various time formats
func normalizeTime(t string) string {
	// Handle "YYYY-MM-DD HH:MM" or "YYYY-MM-DD HH:MM:SS"
	if strings.Contains(t, " ") {
		parts := strings.Split(t, " ")
		if len(parts) >= 2 {
			t = parts[1]
		}
	}
	// Truncate seconds if present (HH:MM:SS -> HH:MM)
	if len(t) > 5 && t[2] == ':' && t[5] == ':' {
		t = t[:5]
	}
	return t
}

// EnergyDataPoint represents energy data for a time period
type EnergyDataPoint struct {
	Date   string  `json:"date"`
	Energy float64 `json:"energy"`
}

// EnergyData represents historical energy data
type EnergyData struct {
	PlantID FlexString        `json:"plant_id"`
	Datas   []EnergyDataPoint `json:"datas"`
}

// EnergyDataRaw is the raw API response format for energy data
type EnergyDataRaw struct {
	PlantID FlexString         `json:"plant_id"`
	Count   int                `json:"count"`
	Datas   map[string]float64 `json:"datas"`
}

// Device represents a device in a plant
type Device struct {
	DeviceSN   FlexString `json:"device_sn"`
	DeviceType int        `json:"device_type"`
	DeviceName string     `json:"device_name"`
	Status     int        `json:"status"`
	Model      string     `json:"model"`
	LastUpdate string     `json:"last_update"`
}

// DeviceListData is the response data for device list
type DeviceListData struct {
	Count   int      `json:"count"`
	Devices []Device `json:"devices"`
}

// MINInverterData represents data for MIN/TLX inverters
type MINInverterData struct {
	Serial      string    `json:"tlx_sn"`
	Status      int       `json:"status"`
	Pac         FlexFloat `json:"pac"`
	Etoday      FlexFloat `json:"etoday"`
	Etotal      FlexFloat `json:"etotal"`
	Vpv1        FlexFloat `json:"vpv1"`
	Vpv2        FlexFloat `json:"vpv2"`
	Ipv1        FlexFloat `json:"ipv1"`
	Ipv2        FlexFloat `json:"ipv2"`
	Vac1        FlexFloat `json:"vac1"`
	Iac1        FlexFloat `json:"iac1"`
	Fac         FlexFloat `json:"fac"`
	Temperature FlexFloat `json:"temperature"`
}

// ParsedPowerData is power data with parsed time
type ParsedPowerData struct {
	Date    time.Time
	Time    string
	Power   float64
	Hour    int
	Minute  int
}
