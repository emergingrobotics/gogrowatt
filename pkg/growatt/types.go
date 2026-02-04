package growatt

import "time"

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
	PlantID       string  `json:"plant_id"`
	PlantName     string  `json:"plant_name"`
	PlantType     int     `json:"plant_type"`
	Country       string  `json:"country"`
	City          string  `json:"city"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	PeakPower     float64 `json:"peak_power"`
	CurrentPower  float64 `json:"current_power"`
	TodayEnergy   float64 `json:"today_energy"`
	TotalEnergy   float64 `json:"total_energy"`
	CreateDate    string  `json:"create_date"`
	Status        int     `json:"status"`
	FormulaCoal   float64 `json:"formula_coal"`
	FormulaCO2    float64 `json:"formula_co2"`
	FormulaMoney  float64 `json:"formula_money"`
	FormulaTree   float64 `json:"formula_tree"`
	MoneyUnit     string  `json:"money_unit"`
	MoneyUnitText string  `json:"money_unit_text"`
}

// PlantListData is the response data for plant list
type PlantListData struct {
	Count  int     `json:"count"`
	Plants []Plant `json:"plants"`
}

// PlantData represents energy overview data
type PlantData struct {
	PlantID        string  `json:"plant_id"`
	TodayEnergy    float64 `json:"today_energy"`
	TotalEnergy    float64 `json:"total_energy"`
	CurrentPower   float64 `json:"current_power"`
	PeakPowerToday float64 `json:"peak_power_today"`
	MonthEnergy    float64 `json:"month_energy"`
	YearEnergy     float64 `json:"year_energy"`
}

// PowerDataPoint represents a single 5-minute power reading
type PowerDataPoint struct {
	Time  string  `json:"time"`
	Power float64 `json:"power"`
}

// PowerData represents power data for a single day
type PowerData struct {
	PlantID string           `json:"plant_id"`
	Date    string           `json:"date"`
	Powers  []PowerDataPoint `json:"powers"`
}

// PowerDataRaw is the raw API response format for power data
type PowerDataRaw struct {
	PlantID string             `json:"plant_id"`
	Count   int                `json:"count"`
	Powers  map[string]float64 `json:"powers"`
}

// EnergyDataPoint represents energy data for a time period
type EnergyDataPoint struct {
	Date   string  `json:"date"`
	Energy float64 `json:"energy"`
}

// EnergyData represents historical energy data
type EnergyData struct {
	PlantID string            `json:"plant_id"`
	Datas   []EnergyDataPoint `json:"datas"`
}

// EnergyDataRaw is the raw API response format for energy data
type EnergyDataRaw struct {
	PlantID string             `json:"plant_id"`
	Count   int                `json:"count"`
	Datas   map[string]float64 `json:"datas"`
}

// Device represents a device in a plant
type Device struct {
	DeviceSN   string `json:"device_sn"`
	DeviceType int    `json:"device_type"`
	DeviceName string `json:"device_name"`
	Status     int    `json:"status"`
	Model      string `json:"model"`
	LastUpdate string `json:"last_update"`
}

// DeviceListData is the response data for device list
type DeviceListData struct {
	Count   int      `json:"count"`
	Devices []Device `json:"devices"`
}

// MINInverterData represents data for MIN/TLX inverters
type MINInverterData struct {
	Serial      string  `json:"tlx_sn"`
	Status      int     `json:"status"`
	Pac         float64 `json:"pac"`
	Etoday      float64 `json:"etoday"`
	Etotal      float64 `json:"etotal"`
	Vpv1        float64 `json:"vpv1"`
	Vpv2        float64 `json:"vpv2"`
	Ipv1        float64 `json:"ipv1"`
	Ipv2        float64 `json:"ipv2"`
	Vac1        float64 `json:"vac1"`
	Iac1        float64 `json:"iac1"`
	Fac         float64 `json:"fac"`
	Temperature float64 `json:"temperature"`
}

// ParsedPowerData is power data with parsed time
type ParsedPowerData struct {
	Date    time.Time
	Time    string
	Power   float64
	Hour    int
	Minute  int
}
