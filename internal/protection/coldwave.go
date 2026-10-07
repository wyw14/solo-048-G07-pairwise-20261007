package protection

import (
	"fmt"
	"math"
	"sync"
	"time"

	"heatpulse/internal/telemetry"
)

// RiskLevel 寒潮风险等级。
type RiskLevel string

const (
	RiskUnknown  RiskLevel = "unknown"  // 数据过期或不足，无法评估
	RiskLow      RiskLevel = "low"      // 时间窗内无冻结风险
	RiskElevated RiskLevel = "elevated" // 时间窗内可能逼近临界
	RiskHigh     RiskLevel = "high"     // 时间窗内预计触发防冻
	RiskSevere   RiskLevel = "severe"   // 当前已处于临界之下
)

// ActionStatus 预防动作的就绪/执行状态。
type ActionStatus string

const (
	ActionUnavailable ActionStatus = "unavailable" // 风险未知，动作禁止
	ActionStandby     ActionStatus = "standby"     // 建议待命，尚未达到执行条件
	ActionRequired    ActionStatus = "required"    // 建议立即执行
	ActionBlocked     ActionStatus = "blocked"     // 需要执行但关联资源被占用
	ActionEngaged     ActionStatus = "engaged"     // 已与防冻/阀门/锁存联动生效
)

// WindChillConfig 风寒配置：站点海拔处的风速（m/s）、相对湿度（0-1）与暴露系数。
type WindChillConfig struct {
	StationID string    `json:"station_id"`
	WindSpeed float64   `json:"wind_speed_ms"`
	Humidity  float64   `json:"humidity"`
	Exposure  float64   `json:"exposure"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Thresholds 寒潮判定阈值，全部以摄氏度/小时/兆帕表达。
type Thresholds struct {
	FreezeCelsius       float64 `json:"freeze_celsius"`        // 防冻触发温度（风寒后）
	HighWindowHours     float64 `json:"high_window_hours"`     // 该时间窗内预计穿越 -> high
	ElevatedWindowHours float64 `json:"elevated_window_hours"` // 该时间窗内预计穿越 -> elevated
	DataMaxAge          string  `json:"data_max_age"`          // 遥测最大有效期
	PressureMinReturn   float64 `json:"pressure_min_return"`   // 回水压力低于此值放大风险
	TrendSamples        int     `json:"trend_samples"`         // 参与线性回归的样本数
}

// DefaultThresholds 默认阈值。
func DefaultThresholds() Thresholds {
	return Thresholds{
		FreezeCelsius:       2.0,
		HighWindowHours:     6,
		ElevatedWindowHours: 24,
		DataMaxAge:          "30m",
		PressureMinReturn:   0.35,
		TrendSamples:        4,
	}
}

// ColdwaveAssessment 单次寒潮风险评估结果。
type ColdwaveAssessment struct {
	StationID        string       `json:"station_id"`
	RiskLevel        RiskLevel    `json:"risk_level"`
	EstimatedTrigger *time.Time   `json:"estimated_trigger_at,omitempty"`
	HoursToTrigger   *float64     `json:"hours_to_trigger,omitempty"`
	ApparentCelsius  *float64     `json:"apparent_celsius,omitempty"`
	AirCelsius       *float64     `json:"air_celsius,omitempty"`
	TrendCelsiusHour *float64     `json:"trend_celsius_per_hour,omitempty"`
	ReturnPressure   *float64     `json:"return_pressure,omitempty"`
	PressureWeak     bool         `json:"pressure_weak"`
	WindowHours      float64      `json:"window_hours"`
	FreezeLatched    bool         `json:"freeze_latched"`
	DataState        string       `json:"data_state"` // fresh | stale
	StaleReasons     []string     `json:"stale_reasons,omitempty"`
	EvaluatedAt      time.Time    `json:"evaluated_at"`
	Actions          []ActionPlan `json:"actions"`
}

// ActionPlan 预防动作及其与现有保护系统的关联状态。
type ActionPlan struct {
	Code        string       `json:"code"` // antifreeze_circulation | frost_valve_admission | freeze_latch
	Description string       `json:"description"`
	Status      ActionStatus `json:"status"`
	Detail      string       `json:"detail,omitempty"`
	Target      string       `json:"target,omitempty"` // 阀门 ID 等关联目标
	Linked      *ActionLinks `json:"linked,omitempty"`
}

// ActionLinks 动作与现有防冻循环、阀门操作、锁存器的关联实况。
type ActionLinks struct {
	AntifreezeRunning bool   `json:"antifreeze_running"`
	ValveOwner        string `json:"valve_owner,omitempty"`
	FreezeRiskLatch   bool   `json:"freeze_risk_latch"`
	PressureTripLatch bool   `json:"pressure_trip_latch"`
}

// ColdwaveRequest 评估请求：未来时间窗与可选的阀门目标。
type ColdwaveRequest struct {
	WindowHours float64 `json:"window_hours"`
	FrostValve  string  `json:"frost_valve"` // 高风险时建议开启的防冻阀
}

// stalenessError 标记哪一路输入过期。
type stalenessError struct{ reason string }

func (e *stalenessError) Error() string { return e.reason }

// Predictor 寒潮风险预测器：风寒配置 + 遥测 + 未来时间窗。
type Predictor struct {
	series     *telemetry.Series
	mu         sync.RWMutex
	configs    map[string]WindChillConfig
	thresholds Thresholds
	maxAge     time.Duration
}

func NewPredictor(series *telemetry.Series, thresholds Thresholds) (*Predictor, error) {
	maxAge, err := time.ParseDuration(thresholds.DataMaxAge)
	if err != nil {
		return nil, fmt.Errorf("invalid data max age %q: %w", thresholds.DataMaxAge, err)
	}
	if thresholds.FreezeCelsius >= 100 || thresholds.ElevatedWindowHours <= 0 || thresholds.TrendSamples < 2 {
		return nil, fmt.Errorf("invalid coldwave thresholds")
	}
	return &Predictor{
		series:     series,
		configs:    make(map[string]WindChillConfig),
		thresholds: thresholds,
		maxAge:     maxAge,
	}, nil
}

// SetConfig 写入站点风寒配置。
func (p *Predictor) SetConfig(config WindChillConfig) (WindChillConfig, error) {
	if config.StationID == "" {
		return WindChillConfig{}, fmt.Errorf("station id is required")
	}
	if config.WindSpeed < 0 || config.Humidity < 0 || config.Humidity > 1 {
		return WindChillConfig{}, fmt.Errorf("wind speed must be >=0 and humidity within [0,1]")
	}
	if config.Exposure <= 0 {
		config.Exposure = 1
	}
	config.UpdatedAt = time.Now().UTC()
	p.mu.Lock()
	p.configs[config.StationID] = config
	p.mu.Unlock()
	return config, nil
}

// Config 读取站点风寒配置；未配置时返回暴露系数为 1 的默认配置。
func (p *Predictor) Config(stationID string) WindChillConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	config, ok := p.configs[stationID]
	if !ok {
		return WindChillConfig{StationID: stationID, Exposure: 1}
	}
	return config
}

// Assess 基于当前遥测与未来时间窗输出风险评估。任何关键输入过期或缺失时
// 风险等级只能是 unknown，预计触发时间留空。
func (p *Predictor) Assess(stationID string, request ColdwaveRequest, now time.Time) ColdwaveAssessment {
	if request.WindowHours <= 0 {
		request.WindowHours = p.thresholds.HighWindowHours
	}
	assessment := ColdwaveAssessment{
		StationID:   stationID,
		RiskLevel:   RiskUnknown,
		WindowHours: request.WindowHours,
		DataState:   "stale",
		EvaluatedAt: now.UTC(),
	}

	config := p.Config(stationID)
	air, trend, airErr := p.freshTemperature(stationID, now)
	if airErr != nil {
		assessment.StaleReasons = append(assessment.StaleReasons, airErr.Error())
	}
	ret, retErr := p.freshReturnPressure(stationID, now)
	if retErr != nil {
		assessment.StaleReasons = append(assessment.StaleReasons, retErr.Error())
	}

	if airErr != nil || retErr != nil {
		assessment.Actions = PlanActions(stationID, RiskUnknown, ActionLinks{}, request.FrostValve)
		return assessment
	}

	apparent := WindChillCelsius(air, config)
	assessment.AirCelsius = &air
	assessment.ApparentCelsius = &apparent
	assessment.TrendCelsiusHour = &trend
	assessment.ReturnPressure = &ret
	assessment.PressureWeak = ret < p.thresholds.PressureMinReturn
	assessment.DataState = "fresh"

	// 回水压力偏低意味着循环减弱、冻结来得更快，按比例提前穿越时刻。
	effectiveTrend := trend
	if assessment.PressureWeak {
		if effectiveTrend < 0 {
			effectiveTrend *= 1.25
		} else {
			effectiveTrend -= 0.25
		}
	}

	if apparent <= p.thresholds.FreezeCelsius {
		assessment.RiskLevel = RiskSevere
		zero := 0.0
		assessment.HoursToTrigger = &zero
		trigger := now.UTC()
		assessment.EstimatedTrigger = &trigger
	} else if effectiveTrend < -1e-9 {
		hours := (apparent - p.thresholds.FreezeCelsius) / -effectiveTrend
		assessment.HoursToTrigger = &hours
		trigger := now.UTC().Add(time.Duration(hours * float64(time.Hour)))
		assessment.EstimatedTrigger = &trigger
		switch {
		case hours <= request.WindowHours:
			assessment.RiskLevel = RiskHigh
		case hours <= p.thresholds.ElevatedWindowHours:
			assessment.RiskLevel = RiskElevated
		default:
			assessment.RiskLevel = RiskLow
		}
	} else {
		assessment.RiskLevel = RiskLow
	}
	return assessment
}

// freshTemperature 读取最新温度并对近期样本做线性回归，样本过期则报错。
func (p *Predictor) freshTemperature(stationID string, now time.Time) (float64, float64, error) {
	latest, ok := p.series.Latest(stationID, telemetry.KindTemperature)
	if !ok {
		return 0, 0, &stalenessError{"temperature telemetry missing"}
	}
	if now.Sub(latest.At) > p.maxAge {
		return 0, 0, &stalenessError{fmt.Sprintf("temperature telemetry stale since %s", latest.At.Format(time.RFC3339))}
	}
	samples := p.series.Recent(stationID, telemetry.KindTemperature, p.thresholds.TrendSamples)
	trend := linearTrendCelsiusPerHour(samples)
	if math.IsNaN(trend) || math.IsInf(trend, 0) {
		return 0, 0, &stalenessError{"temperature trend unusable"}
	}
	return latest.Value, trend, nil
}

// freshReturnPressure 读取最新回水压力；缺失或过期均视为关键输入不可用，
// 评估只能给出 unknown。
func (p *Predictor) freshReturnPressure(stationID string, now time.Time) (float64, error) {
	latest, ok := p.series.Latest(stationID, telemetry.KindReturnPressure)
	if !ok {
		return 0, &stalenessError{"return pressure telemetry missing"}
	}
	if now.Sub(latest.At) > p.maxAge {
		return 0, &stalenessError{fmt.Sprintf("return pressure telemetry stale since %s", latest.At.Format(time.RFC3339))}
	}
	return latest.Value, nil
}

// WindChillCelsius 计算风寒后的表观温度（摄氏度）。
// 风速项采用简化 Steadman 风寒，湿度项在低温下以小权重计入蒸发/结露修正，
// exposure 为站点暴露系数（1 = 标准暴露，越大越不利）。
func WindChillCelsius(airCelsius float64, config WindChillConfig) float64 {
	wind := config.WindSpeed
	if wind < 0 {
		wind = 0
	}
	humidity := config.Humidity
	if humidity < 0 {
		humidity = 0
	} else if humidity > 1 {
		humidity = 1
	}
	exposure := config.Exposure
	if exposure <= 0 {
		exposure = 1
	}
	windTerm := 0.0
	if wind > 1.3 { // 风寒公式仅在有明显风速时生效
		windTerm = 11.37 + 0.3965*airCelsius - 11.43*math.Pow(wind, 0.16) + 0.427*airCelsius*math.Pow(wind, 0.16) - airCelsius
	}
	damp := 0.0
	if airCelsius < 5 && humidity > 0.5 { // 仅低温高湿加剧散热
		damp = (humidity - 0.5) * (5 - airCelsius) * 0.2
	}
	return airCelsius + (windTerm-damp)*exposure
}

// linearTrendCelsiusPerHour 对样本做最小二乘线性回归，返回 °C/小时斜率。
func linearTrendCelsiusPerHour(samples []telemetry.Sample) float64 {
	n := float64(len(samples))
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumXX float64
	base := samples[0].At
	for _, sample := range samples {
		x := sample.At.Sub(base).Hours()
		y := sample.Value
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	denom := n*sumXX - sumX*sumX
	if math.Abs(denom) < 1e-12 {
		return 0 // 样本时间戳重合，无法回归
	}
	return (n*sumXY - sumX*sumY) / denom
}

// PlanActions 根据风险等级与关联保护系统实况生成预防动作计划（不执行）。
func PlanActions(stationID string, level RiskLevel, links ActionLinks, frostValve string) []ActionPlan {
	frostOwner := "frost:" + stationID
	circulation := ActionPlan{
		Code:        "antifreeze_circulation",
		Description: "启动站点防冻循环，保持管网流动防止冻结",
		Linked:      cloneLinks(links),
	}
	valve := ActionPlan{
		Code:        "frost_valve_admission",
		Description: "开启防冻阀引入回水，提升末端温度",
		Target:      frostValve,
		Linked:      cloneLinks(links),
	}
	latch := ActionPlan{
		Code:        "freeze_latch",
		Description: "置位冻结风险锁存器，联锁保护直至人工复位",
		Linked:      cloneLinks(links),
	}

	switch level {
	case RiskUnknown:
		for _, plan := range []*ActionPlan{&circulation, &valve, &latch} {
			plan.Status = ActionUnavailable
			plan.Detail = "数据过期，禁止动作"
		}
		return []ActionPlan{circulation, valve, latch}
	case RiskLow:
		circulation.Status, valve.Status, latch.Status = ActionStandby, ActionStandby, ActionStandby
	case RiskElevated:
		circulation.Status, valve.Status, latch.Status = ActionStandby, ActionStandby, ActionStandby
		circulation.Detail = "时间窗内存在风险，循环保持待命"
	case RiskHigh, RiskSevere:
		// 防冻循环
		if links.AntifreezeRunning {
			circulation.Status = ActionEngaged
			circulation.Detail = "防冻循环运行中"
		} else {
			circulation.Status = ActionRequired
		}
		// 防冻阀：未指定则跳过；已由本站防冻操作持有视为生效；被他人占用则阻塞
		switch {
		case frostValve == "":
			valve.Status = ActionStandby
			valve.Detail = "未指定防冻阀，跳过阀门动作"
		case links.ValveOwner == frostOwner:
			valve.Status = ActionEngaged
			valve.Detail = "防冻阀已由防冻保护开启"
		case links.ValveOwner != "":
			valve.Status = ActionBlocked
			valve.Detail = "防冻阀被 " + links.ValveOwner + " 占用"
		default:
			valve.Status = ActionRequired
		}
		// 冻结风险锁存器
		if links.FreezeRiskLatch {
			latch.Status = ActionEngaged
			latch.Detail = "FreezeRisk 锁存已置位"
		} else {
			latch.Status = ActionRequired
		}
	}
	return []ActionPlan{circulation, valve, latch}
}

// cloneLinks 复制关联实况，避免各动作共享指针。
func cloneLinks(links ActionLinks) *ActionLinks {
	copy := links
	return &copy
}
