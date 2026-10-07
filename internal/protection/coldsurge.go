package protection

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// RiskLevel is the cold-surge risk classification for one station.
type RiskLevel string

const (
	RiskUnknown  RiskLevel = "unknown"
	RiskSafe     RiskLevel = "safe"
	RiskWatch    RiskLevel = "watch"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// ActionKind identifies a preventive action bound to an existing protection
// mechanism (antifreeze circulation, frost valve operation, freeze latch).
type ActionKind string

const (
	ActionReviewLatches        ActionKind = "review_latches"
	ActionStartAntifreeze      ActionKind = "start_antifreeze_circulation"
	ActionSetFreezeRiskLatch   ActionKind = "set_freeze_risk_latch"
	ActionSetPressureTripLatch ActionKind = "set_pressure_trip_latch"
	ActionOpenFrostValve       ActionKind = "open_frost_valve"
)

// WindChillConfig expresses how wind exposure accelerates heat loss.
// CoolingPerMPS is the extra cooling rate (Celsius per hour) applied per m/s
// of wind at ExposureFactor 1.0 (0.5 sheltered, 1.5 fully exposed pipework).
type WindChillConfig struct {
	WindSpeedMPS   float64 `json:"wind_speed_mps"`
	ExposureFactor float64 `json:"exposure_factor"`
	CoolingPerMPS  float64 `json:"cooling_per_mps"`
}

// ColdSurgeConfig holds the thresholds and tuning for surge prediction.
type ColdSurgeConfig struct {
	// Temperature trip points in Celsius; a reading at/below CriticalCelsius
	// means freezing is imminent at the station.
	WatchCelsius    float64 `json:"watch_celsius"`
	WarningCelsius  float64 `json:"warning_celsius"`
	CriticalCelsius float64 `json:"critical_celsius"`

	// Return pressure band: at HealthyReturnPressure circulation is treated as
	// sound; at/below LowReturnPressure the cooling rate is scaled by
	// PressureCoolingFactor (weak/stagnant loop loses heat much faster).
	HealthyReturnPressure float64 `json:"healthy_return_pressure"`
	LowReturnPressure     float64 `json:"low_return_pressure"`
	PressureCoolingFactor float64 `json:"pressure_cooling_factor"`

	WindChill WindChillConfig `json:"wind_chill"`

	// FallbackCoolingRate (Celsius/hour) is used when the temperature trend
	// cannot be derived from recent telemetry (fewer than two fresh points).
	FallbackCoolingRate float64 `json:"fallback_cooling_rate"`
	// ForecastMinCelsius bounds the projection: medium temperature cannot fall
	// below the forecast ambient floor within the window.
	ForecastMinCelsius float64 `json:"forecast_min_celsius"`
	// MaxDataAge marks temperature/pressure samples older than this as stale.
	MaxDataAge time.Duration `json:"-"`
	// FrostValveID optionally preselects the valve used for frost admission;
	// when empty a valve_id must be supplied on the evaluation request.
	FrostValveID string `json:"frost_valve_id,omitempty"`
}

// DefaultColdSurgeConfig returns conservative district-heating defaults.
func DefaultColdSurgeConfig() ColdSurgeConfig {
	return ColdSurgeConfig{
		WatchCelsius:          8,
		WarningCelsius:        5,
		CriticalCelsius:       2,
		HealthyReturnPressure: 0.45,
		LowReturnPressure:     0.25,
		PressureCoolingFactor: 2.0,
		WindChill: WindChillConfig{
			WindSpeedMPS:   0,
			ExposureFactor: 1,
			CoolingPerMPS:  0.6,
		},
		FallbackCoolingRate: 2.0,
		ForecastMinCelsius:  -30,
		MaxDataAge:          15 * time.Minute,
	}
}

// Validate checks threshold ordering and tuning ranges.
func (c ColdSurgeConfig) Validate() error {
	if !(c.CriticalCelsius < c.WarningCelsius && c.WarningCelsius < c.WatchCelsius) {
		return fmt.Errorf("thresholds must be ordered critical < warning < watch")
	}
	if c.LowReturnPressure >= c.HealthyReturnPressure {
		return fmt.Errorf("low return pressure must be below healthy return pressure")
	}
	if c.PressureCoolingFactor < 1 {
		return fmt.Errorf("pressure cooling factor must be >= 1")
	}
	if c.WindChill.ExposureFactor < 0 || c.WindChill.ExposureFactor > 3 {
		return fmt.Errorf("wind exposure factor must be within [0,3]")
	}
	if c.WindChill.WindSpeedMPS < 0 || c.WindChill.CoolingPerMPS < 0 {
		return fmt.Errorf("wind speed and cooling factor must be non-negative")
	}
	if c.FallbackCoolingRate < 0 {
		return fmt.Errorf("fallback cooling rate must be non-negative")
	}
	if c.MaxDataAge <= 0 {
		return fmt.Errorf("max data age must be positive")
	}
	return nil
}

type coldSurgeConfigJSON struct {
	WatchCelsius          float64         `json:"watch_celsius"`
	WarningCelsius        float64         `json:"warning_celsius"`
	CriticalCelsius       float64         `json:"critical_celsius"`
	HealthyReturnPressure float64         `json:"healthy_return_pressure"`
	LowReturnPressure     float64         `json:"low_return_pressure"`
	PressureCoolingFactor float64         `json:"pressure_cooling_factor"`
	WindChill             WindChillConfig `json:"wind_chill"`
	FallbackCoolingRate   float64         `json:"fallback_cooling_rate"`
	ForecastMinCelsius    float64         `json:"forecast_min_celsius"`
	MaxDataAgeMinutes     float64         `json:"max_data_age_minutes"`
	FrostValveID          string          `json:"frost_valve_id,omitempty"`
}

// MarshalJSON renders durations as fractional minutes for API clients.
func (c ColdSurgeConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(coldSurgeConfigJSON{
		WatchCelsius:          c.WatchCelsius,
		WarningCelsius:        c.WarningCelsius,
		CriticalCelsius:       c.CriticalCelsius,
		HealthyReturnPressure: c.HealthyReturnPressure,
		LowReturnPressure:     c.LowReturnPressure,
		PressureCoolingFactor: c.PressureCoolingFactor,
		WindChill:             c.WindChill,
		FallbackCoolingRate:   c.FallbackCoolingRate,
		ForecastMinCelsius:    c.ForecastMinCelsius,
		MaxDataAgeMinutes:     c.MaxDataAge.Minutes(),
		FrostValveID:          c.FrostValveID,
	})
}

// UnmarshalJSON accepts fractional minutes and fills defaults for zero fields.
func (c *ColdSurgeConfig) UnmarshalJSON(raw []byte) error {
	var dto coldSurgeConfigJSON
	if err := json.Unmarshal(raw, &dto); err != nil {
		return err
	}
	*c = ColdSurgeConfig{
		WatchCelsius:          dto.WatchCelsius,
		WarningCelsius:        dto.WarningCelsius,
		CriticalCelsius:       dto.CriticalCelsius,
		HealthyReturnPressure: dto.HealthyReturnPressure,
		LowReturnPressure:     dto.LowReturnPressure,
		PressureCoolingFactor: dto.PressureCoolingFactor,
		WindChill:             dto.WindChill,
		FallbackCoolingRate:   dto.FallbackCoolingRate,
		ForecastMinCelsius:    dto.ForecastMinCelsius,
		MaxDataAge:            time.Duration(dto.MaxDataAgeMinutes * float64(time.Minute)),
		FrostValveID:          dto.FrostValveID,
	}
	return nil
}

// ColdSurgeReading carries the latest validated telemetry for a station.
type ColdSurgeReading struct {
	TemperatureCelsius  float64
	TemperatureAt       time.Time
	ReturnPressure      float64
	PressureAt          time.Time
	TrendCelsiusPerHour float64
	TrendKnown          bool
}

// PreventiveAction is a planned protection measure. The service layer decides
// whether (and how) each one is executed against the live protection systems.
type PreventiveAction struct {
	Kind        ActionKind `json:"kind"`
	Description string     `json:"description"`
	ValveID     string     `json:"valve_id,omitempty"`
	Urgent      bool       `json:"urgent"`
}

// Assessment is the prediction result for one station/window evaluation.
type Assessment struct {
	StationID                 string             `json:"station_id"`
	Level                     RiskLevel          `json:"level"`
	TemperatureCelsius        float64            `json:"temperature_celsius"`
	ReturnPressure            float64            `json:"return_pressure"`
	CoolingRateCelsiusPerHour float64            `json:"cooling_rate_celsius_per_hour"`
	ProjectedMinCelsius       *float64           `json:"projected_min_celsius,omitempty"`
	EstimatedTriggerAt        *time.Time         `json:"estimated_trigger_at,omitempty"`
	WindowMinutes             float64            `json:"window_minutes"`
	Actions                   []PreventiveAction `json:"actions"`
	Reasons                   []string           `json:"reasons,omitempty"`
	EvaluatedAt               time.Time          `json:"evaluated_at"`
}

// UnknownAssessment builds the only response allowed when inputs are stale or
// missing: the level stays unknown and no actions may be acted upon.
func UnknownAssessment(stationID string, now time.Time, reasons []string) Assessment {
	return Assessment{
		StationID:   stationID,
		Level:       RiskUnknown,
		Actions:     []PreventiveAction{},
		Reasons:     reasons,
		EvaluatedAt: now,
	}
}

// SurgePredictor contains the pure prediction logic; it has no side effects.
type SurgePredictor struct{}

// NewSurgePredictor constructs the predictor.
func NewSurgePredictor() *SurgePredictor { return &SurgePredictor{} }

// Assess derives the risk level, estimated trigger time and preventive action
// plan from current telemetry, wind-chill configuration and the future window.
func (p *SurgePredictor) Assess(stationID string, reading ColdSurgeReading, config ColdSurgeConfig, window time.Duration, now time.Time) Assessment {
	assessment := Assessment{
		StationID:          stationID,
		Level:              RiskSafe,
		TemperatureCelsius: reading.TemperatureCelsius,
		ReturnPressure:     reading.ReturnPressure,
		WindowMinutes:      window.Minutes(),
		Actions:            []PreventiveAction{},
		EvaluatedAt:        now,
	}

	// Cooling rate: observed downtrend (or forecast fallback when the trend is
	// unknown) plus the wind-chill contribution, scaled by the return-pressure
	// penalty when the loop is weak.
	base := config.FallbackCoolingRate
	if reading.TrendKnown {
		if cooling := -reading.TrendCelsiusPerHour; cooling > 0 {
			base = cooling
		} else {
			base = 0
		}
	}
	windRate := config.WindChill.CoolingPerMPS * config.WindChill.WindSpeedMPS * config.WindChill.ExposureFactor
	rate := (base + windRate) * p.pressureFactor(reading.ReturnPressure, config)
	assessment.CoolingRateCelsiusPerHour = round1(rate)

	reasons := make([]string, 0, 4)
	if reading.TrendKnown {
		reasons = append(reasons, fmt.Sprintf("observed trend %.2f C/h", reading.TrendCelsiusPerHour))
	} else {
		reasons = append(reasons, "temperature trend unknown, using forecast fallback rate")
	}
	if windRate > 0 {
		reasons = append(reasons, fmt.Sprintf("wind chill adds %.2f C/h (%.1f m/s, exposure %.2f)", windRate, config.WindChill.WindSpeedMPS, config.WindChill.ExposureFactor))
	}
	if reading.ReturnPressure <= config.LowReturnPressure {
		reasons = append(reasons, fmt.Sprintf("return pressure %.2f at/below low limit %.2f, circulation weak", reading.ReturnPressure, config.LowReturnPressure))
	}

	hours := window.Hours()
	if rate > 0 {
		projected := reading.TemperatureCelsius - rate*hours
		if projected < config.ForecastMinCelsius {
			projected = config.ForecastMinCelsius
		}
		projected = round1(projected)
		assessment.ProjectedMinCelsius = &projected

		// ETA for the critical threshold, even if it lands beyond the window.
		if reading.TemperatureCelsius > config.CriticalCelsius {
			etaHours := (reading.TemperatureCelsius - config.CriticalCelsius) / rate
			trigger := now.Add(time.Duration(etaHours * float64(time.Hour)))
			assessment.EstimatedTriggerAt = &trigger
		}
	}

	// Already at/below the critical band: trigger time is now.
	if reading.TemperatureCelsius <= config.CriticalCelsius {
		immediate := now
		assessment.EstimatedTriggerAt = &immediate
		assessment.Level = RiskCritical
		reasons = append(reasons, fmt.Sprintf("temperature %.2f at/below critical threshold %.2f", reading.TemperatureCelsius, config.CriticalCelsius))
	} else if assessment.ProjectedMinCelsius != nil && *assessment.ProjectedMinCelsius <= config.CriticalCelsius {
		assessment.Level = RiskCritical
		reasons = append(reasons, fmt.Sprintf("projected minimum %.2f C reaches critical threshold %.2f within %.0f min", *assessment.ProjectedMinCelsius, config.CriticalCelsius, window.Minutes()))
	} else if assessment.ProjectedMinCelsius != nil && *assessment.ProjectedMinCelsius <= config.WarningCelsius {
		assessment.Level = RiskHigh
		reasons = append(reasons, fmt.Sprintf("projected minimum %.2f C reaches warning threshold %.2f within %.0f min", *assessment.ProjectedMinCelsius, config.WarningCelsius, window.Minutes()))
	} else if assessment.ProjectedMinCelsius != nil && *assessment.ProjectedMinCelsius <= config.WatchCelsius {
		assessment.Level = RiskWatch
		reasons = append(reasons, fmt.Sprintf("projected minimum %.2f C reaches watch threshold %.2f within %.0f min", *assessment.ProjectedMinCelsius, config.WatchCelsius, window.Minutes()))
	} else {
		reasons = append(reasons, fmt.Sprintf("temperature %.2f C stays above watch threshold %.2f across the window", reading.TemperatureCelsius, config.WatchCelsius))
	}

	// Weak return pressure escalates a borderline prediction by one level:
	// a stagnant loop cannot rely on residual heat carry-over.
	if reading.ReturnPressure <= config.LowReturnPressure {
		switch assessment.Level {
		case RiskWatch:
			assessment.Level = RiskHigh
			reasons = append(reasons, "escalated watch to high due to low return pressure")
		case RiskHigh:
			assessment.Level = RiskCritical
			reasons = append(reasons, "escalated high to critical due to low return pressure")
		}
	}

	assessment.Actions = p.planActions(assessment.Level, reading, config)
	assessment.Reasons = reasons
	return assessment
}

func (p *SurgePredictor) pressureFactor(pressure float64, config ColdSurgeConfig) float64 {
	if pressure >= config.HealthyReturnPressure {
		return 1
	}
	if pressure <= config.LowReturnPressure {
		return config.PressureCoolingFactor
	}
	span := config.HealthyReturnPressure - config.LowReturnPressure
	if span <= 0 {
		return 1
	}
	fraction := (config.HealthyReturnPressure - pressure) / span
	return 1 + (config.PressureCoolingFactor-1)*fraction
}

func (p *SurgePredictor) planActions(level RiskLevel, reading ColdSurgeReading, config ColdSurgeConfig) []PreventiveAction {
	lowPressure := reading.ReturnPressure <= config.LowReturnPressure
	switch level {
	case RiskWatch:
		return []PreventiveAction{
			{Kind: ActionReviewLatches, Description: "review freeze-risk latch and station telemetry cadence"},
		}
	case RiskHigh:
		actions := []PreventiveAction{
			{Kind: ActionSetFreezeRiskLatch, Description: "arm freeze-risk latch", Urgent: true},
			{Kind: ActionStartAntifreeze, Description: "start antifreeze circulation loop", Urgent: true},
		}
		if lowPressure {
			actions = append(actions, PreventiveAction{
				Kind: ActionSetPressureTripLatch, Description: "arm pressure-trip latch, return pressure below low limit", Urgent: true,
			})
		}
		return actions
	case RiskCritical:
		valveID := config.FrostValveID
		actions := []PreventiveAction{
			{Kind: ActionSetFreezeRiskLatch, Description: "arm freeze-risk latch", Urgent: true},
			{Kind: ActionStartAntifreeze, Description: "start antifreeze circulation loop", Urgent: true},
			{Kind: ActionOpenFrostValve, Description: "open frost admission valve to warm the exposed branch", ValveID: valveID, Urgent: true},
		}
		if lowPressure {
			actions = append(actions, PreventiveAction{
				Kind: ActionSetPressureTripLatch, Description: "arm pressure-trip latch, return pressure below low limit", Urgent: true,
			})
		}
		return actions
	default:
		return []PreventiveAction{}
	}
}

func round1(value float64) float64 { return math.Round(value*10) / 10 }
