package protection

import (
	"context"
	"math"
	"testing"
	"time"

	"heatpulse/internal/exchanger"
	"heatpulse/internal/pump"
	"heatpulse/internal/store"
	"heatpulse/internal/telemetry"
	"heatpulse/internal/valve"
)

type harness struct {
	series     *telemetry.Series
	predictor  *Predictor
	actions    *ActionService
	antifreeze *Antifreeze
	frost      *FrostProtection
	latches    *LatchStore
	incidents  *Service
	operations *valve.Operations
}

func newHarness(t *testing.T, thresholds Thresholds) *harness {
	t.Helper()
	series := telemetry.NewSeries(128)
	predictor, err := NewPredictor(series, thresholds)
	if err != nil {
		t.Fatalf("new predictor: %v", err)
	}
	pumps := pump.NewRunner()
	circulation := exchanger.NewCirculationRunner(pumps)
	antifreeze := NewAntifreeze(circulation)
	operations := valve.NewOperations()
	frost := NewFrostProtection(operations)
	latches := NewLatchStore(store.NewStationSnapshotStore(t.TempDir()))
	incidents := NewService()
	actions := NewActionService(predictor, antifreeze, frost, latches, incidents)
	return &harness{
		series: series, predictor: predictor, actions: actions, antifreeze: antifreeze,
		frost: frost, latches: latches, incidents: incidents, operations: operations,
	}
}

func (h *harness) close(t *testing.T, stations ...string) {
	t.Helper()
	for _, id := range stations {
		if h.antifreeze.Running(id) {
			if _, err := h.antifreeze.Stop(id); err != nil {
				t.Fatalf("stop antifreeze: %v", err)
			}
		}
	}
}

func addTemp(t *testing.T, h *harness, station string, points []float64, now time.Time) {
	t.Helper()
	for i, value := range points {
		at := now.Add(-time.Duration(len(points)-1-i) * time.Hour)
		if err := h.series.Add(telemetry.Sample{
			StationID: station, Kind: telemetry.KindTemperature,
			Watermark: uint64(i + 1), Value: value, At: at,
		}); err != nil {
			t.Fatalf("add temp: %v", err)
		}
	}
}

func addPressure(t *testing.T, h *harness, station string, value float64, at time.Time) {
	t.Helper()
	if err := h.series.Add(telemetry.Sample{
		StationID: station, Kind: telemetry.KindReturnPressure,
		Watermark: 1, Value: value, At: at,
	}); err != nil {
		t.Fatalf("add pressure: %v", err)
	}
}

func TestWindChillLowersApparentTemperature(t *testing.T) {
	calm := WindChillCelsius(0, WindChillConfig{Exposure: 1})
	windy := WindChillCelsius(0, WindChillConfig{WindSpeed: 10, Humidity: 0.5, Exposure: 1})
	if math.Abs(calm) > 1e-9 {
		t.Fatalf("calm apparent = %.2f, want 0", calm)
	}
	if windy >= calm {
		t.Fatalf("windy apparent %.2f should be below calm %.2f", windy, calm)
	}
}

func TestAssessUnknownWithoutTelemetry(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	assessment := h.predictor.Assess("ghost", ColdwaveRequest{WindowHours: 6}, time.Now())
	if assessment.RiskLevel != RiskUnknown {
		t.Fatalf("risk = %s, want unknown", assessment.RiskLevel)
	}
	if assessment.EstimatedTrigger != nil {
		t.Fatalf("trigger time must be unknown, got %v", assessment.EstimatedTrigger)
	}
	if len(assessment.StaleReasons) == 0 {
		t.Fatalf("expected stale reasons")
	}
	for _, action := range assessment.Actions {
		if action.Status != ActionUnavailable {
			t.Fatalf("action %s status = %s, want unavailable", action.Code, action.Status)
		}
	}
}

func TestAssessUnknownWhenTelemetryStale(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	now := time.Now()
	staleAt := now.Add(-time.Hour)
	addTemp(t, h, "s1", []float64{8, 6, 4, 3}, staleAt)
	addPressure(t, h, "s1", 0.46, staleAt)

	assessment := h.predictor.Assess("s1", ColdwaveRequest{WindowHours: 6}, now)
	if assessment.RiskLevel != RiskUnknown {
		t.Fatalf("risk = %s, want unknown on stale data", assessment.RiskLevel)
	}
	if assessment.DataState != "stale" {
		t.Fatalf("data state = %s, want stale", assessment.DataState)
	}
}

func TestAssessUnknownWhenReturnPressureStale(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	now := time.Now()
	addTemp(t, h, "s1", []float64{8, 6, 4, 3}, now) // 新鲜温度
	addPressure(t, h, "s1", 0.46, now.Add(-time.Hour))

	assessment := h.predictor.Assess("s1", ColdwaveRequest{WindowHours: 6}, now)
	if assessment.RiskLevel != RiskUnknown {
		t.Fatalf("risk = %s, want unknown when return pressure stale", assessment.RiskLevel)
	}
}

func TestAssessHighWithEstimatedTrigger(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	now := time.Now()
	if _, err := h.predictor.SetConfig(WindChillConfig{StationID: "s1", Exposure: 1}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	// 3°C 且每小时降 2°C，距 2°C 阈值约 0.5 小时。
	addTemp(t, h, "s1", []float64{9, 7, 5, 3}, now)
	addPressure(t, h, "s1", 0.46, now)

	assessment := h.predictor.Assess("s1", ColdwaveRequest{WindowHours: 6}, now)
	if assessment.RiskLevel != RiskHigh {
		t.Fatalf("risk = %s, want high", assessment.RiskLevel)
	}
	if assessment.HoursToTrigger == nil || math.Abs(*assessment.HoursToTrigger-0.5) > 0.01 {
		got := -1.0
		if assessment.HoursToTrigger != nil {
			got = *assessment.HoursToTrigger
		}
		t.Fatalf("hours to trigger = %.3f, want ~0.5", got)
	}
	if assessment.EstimatedTrigger == nil {
		t.Fatalf("expected estimated trigger time")
	}
}

func TestAssessSevereAtOrBelowThreshold(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	now := time.Now()
	addTemp(t, h, "s1", []float64{4, 3, 2, 1}, now)
	addPressure(t, h, "s1", 0.46, now)

	assessment := h.predictor.Assess("s1", ColdwaveRequest{WindowHours: 6}, now)
	if assessment.RiskLevel != RiskSevere {
		t.Fatalf("risk = %s, want severe", assessment.RiskLevel)
	}
	if assessment.HoursToTrigger == nil || *assessment.HoursToTrigger != 0 {
		t.Fatalf("hours to trigger = %v, want 0", assessment.HoursToTrigger)
	}
}

func TestEngageRefusedOnUnknownWithoutSideEffects(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	_, err := h.actions.Engage(context.Background(), "s1", ColdwaveRequest{WindowHours: 6, FrostValve: "v1"}, time.Now())
	if err == nil {
		t.Fatalf("engage on unknown must fail")
	}
	if h.antifreeze.Running("s1") {
		t.Fatalf("antifreeze must not start on unknown")
	}
	if got := h.latches.Get("s1"); got.FreezeRisk {
		t.Fatalf("latch must not be set on unknown")
	}
	if h.frost.AdmissionStation("v1") != "" {
		t.Fatalf("valve must not be opened on unknown")
	}
}

func TestEngageLinksAllProtectionSystems(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	defer h.close(t, "s1")
	now := time.Now()
	addTemp(t, h, "s1", []float64{9, 7, 5, 3}, now)
	addPressure(t, h, "s1", 0.46, now)

	result, err := h.actions.Engage(context.Background(), "s1", ColdwaveRequest{WindowHours: 6, FrostValve: "v1"}, now)
	if err != nil {
		t.Fatalf("engage: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	// 防冻循环真实运行
	if !h.antifreeze.Running("s1") {
		t.Fatalf("antifreeze circulation not running")
	}
	// 防冻阀已由防冻保护开启
	if h.frost.AdmissionStation("v1") != "s1" {
		t.Fatalf("frost valve admission station = %q, want s1", h.frost.AdmissionStation("v1"))
	}
	// 冻结锁存已置位
	if got := h.latches.Get("s1"); !got.FreezeRisk {
		t.Fatalf("freeze risk latch not set: %+v", got)
	}
	// 严重/高风险事件已上报
	found := false
	for _, incident := range h.incidents.List() {
		if incident.Kind == "coldwave_freeze" && incident.StationID == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("coldwave incident not raised")
	}

	// 联动后再评估：三项动作均为 engaged
	reassessment := h.actions.Evaluate("s1", ColdwaveRequest{WindowHours: 6, FrostValve: "v1"}, now)
	status := map[string]ActionStatus{}
	for _, action := range reassessment.Actions {
		status[action.Code] = action.Status
	}
	for code, want := range map[string]ActionStatus{
		"antifreeze_circulation": ActionEngaged,
		"frost_valve_admission":  ActionEngaged,
		"freeze_latch":           ActionEngaged,
	} {
		if status[code] != want {
			t.Fatalf("action %s = %s, want %s", code, status[code], want)
		}
	}
}

func TestEngageBlockedByManualValveOwner(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	defer h.close(t, "s1")
	now := time.Now()
	addTemp(t, h, "s1", []float64{9, 7, 5, 3}, now)
	addPressure(t, h, "s1", 0.46, now)

	// 手动操作进行中，占用阀门且不释放。
	if _, err := h.operations.Start("v1", "manual:s2", 50); err != nil {
		t.Fatalf("seed manual owner: %v", err)
	}

	result, err := h.actions.Engage(context.Background(), "s1", ColdwaveRequest{WindowHours: 6, FrostValve: "v1"}, now)
	if err != nil {
		t.Fatalf("engage: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected blocked valve error")
	}
	if h.frost.AdmissionStation("v1") != "" {
		t.Fatalf("blocked valve must not be opened by frost protection")
	}
	// 循环与锁存不受阀门阻塞影响
	if !h.antifreeze.Running("s1") {
		t.Fatalf("antifreeze should still run when valve blocked")
	}

	plan := h.actions.Evaluate("s1", ColdwaveRequest{WindowHours: 6, FrostValve: "v1"}, now)
	for _, action := range plan.Actions {
		if action.Code == "frost_valve_admission" && action.Status != ActionBlocked {
			t.Fatalf("valve action = %s, want blocked", action.Status)
		}
	}
}

func TestWeakReturnPressureSetsPressureTrip(t *testing.T) {
	h := newHarness(t, DefaultThresholds())
	defer h.close(t, "s1")
	now := time.Now()
	addTemp(t, h, "s1", []float64{9, 7, 5, 3}, now)
	addPressure(t, h, "s1", 0.20, now) // 低于 0.35

	assessment := h.actions.Evaluate("s1", ColdwaveRequest{WindowHours: 6}, now)
	if !assessment.PressureWeak {
		t.Fatalf("expected pressure weak flag")
	}
	if _, err := h.actions.Engage(context.Background(), "s1", ColdwaveRequest{WindowHours: 6}, now); err != nil {
		t.Fatalf("engage: %v", err)
	}
	if got := h.latches.Get("s1"); !got.FreezeRisk || !got.PressureTrip {
		t.Fatalf("latches = %+v, want freeze + pressure trip", got)
	}
}
