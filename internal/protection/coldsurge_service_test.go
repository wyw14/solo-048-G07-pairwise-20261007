package protection_test

import (
	"context"
	"testing"
	"time"

	"heatpulse/internal/exchanger"
	"heatpulse/internal/protection"
	"heatpulse/internal/pump"
	"heatpulse/internal/store"
	"heatpulse/internal/telemetry"
	"heatpulse/internal/valve"
)

type surgeHarness struct {
	series     *telemetry.Series
	antifreeze *protection.Antifreeze
	frost      *protection.FrostProtection
	latches    *protection.LatchStore
	operations *valve.Operations
	surge      *protection.ColdSurgeProtection
}

func newHarness(t *testing.T, config protection.ColdSurgeConfig) *surgeHarness {
	t.Helper()
	series := telemetry.NewSeries(128)
	pumpRunner := pump.NewRunner()
	circulation := exchanger.NewCirculationRunner(pumpRunner)
	antifreeze := protection.NewAntifreeze(circulation)
	t.Cleanup(antifreeze.Close)
	operations := valve.NewOperations()
	frost := protection.NewFrostProtection(operations)
	snapshots := store.NewStationSnapshotStore(t.TempDir())
	latches := protection.NewLatchStore(snapshots)
	surge, err := protection.NewColdSurgeProtection(series, antifreeze, frost, latches, config)
	if err != nil {
		t.Fatalf("new cold surge protection: %v", err)
	}
	return &surgeHarness{series: series, antifreeze: antifreeze, frost: frost, latches: latches, operations: operations, surge: surge}
}

func addTemperature(t *testing.T, h *surgeHarness, stationID string, watermark uint64, value float64, at time.Time) {
	t.Helper()
	if err := h.series.Add(telemetry.Sample{StationID: stationID, Kind: telemetry.KindTemperature, Watermark: watermark, Value: value, At: at}); err != nil {
		t.Fatalf("add temperature: %v", err)
	}
}

func addReturnPressure(t *testing.T, h *surgeHarness, stationID string, watermark uint64, value float64, at time.Time) {
	t.Helper()
	if err := h.series.Add(telemetry.Sample{StationID: stationID, Kind: telemetry.KindReturnPressure, Watermark: watermark, Value: value, At: at}); err != nil {
		t.Fatalf("add return pressure: %v", err)
	}
}

func outcomeByKind(result protection.EvaluationResult, kind protection.ActionKind) (protection.ActionOutcome, bool) {
	for _, outcome := range result.Outcomes {
		if outcome.Kind == kind {
			return outcome, true
		}
	}
	return protection.ActionOutcome{}, false
}

func TestEvaluateCriticalExecutesAllProtectionLinks(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	now := time.Now().UTC()
	addTemperature(t, h, "s1", 1, 1.5, now)
	addReturnPressure(t, h, "s1", 1, 0.46, now)

	result, err := h.surge.Evaluate(context.Background(), "s1", time.Hour, "fv-1", true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Assessment.Level != protection.RiskCritical {
		t.Fatalf("level = %s, want critical", result.Assessment.Level)
	}

	// Antifreeze circulation must be running.
	if !h.antifreeze.Running("s1") {
		t.Fatal("antifreeze circulation not started")
	}
	circulation, ok := outcomeByKind(result, protection.ActionStartAntifreeze)
	if !ok || !circulation.Executed || circulation.SessionID == "" {
		t.Fatalf("antifreeze outcome = %+v, want executed with session id", circulation)
	}

	// Freeze-risk latch must be armed and persisted with a positive revision.
	latches := h.latches.Get("s1")
	if !latches.FreezeRisk || latches.Revision == 0 {
		t.Fatalf("latches = %+v, want freeze-risk armed", latches)
	}
	latch, ok := outcomeByKind(result, protection.ActionSetFreezeRiskLatch)
	if !ok || !latch.Executed || latch.LatchRevision != latches.Revision {
		t.Fatalf("latch outcome = %+v, want executed at revision %d", latch, latches.Revision)
	}

	// Frost valve operation must be completed and released.
	valveAction, ok := outcomeByKind(result, protection.ActionOpenFrostValve)
	if !ok || !valveAction.Executed || valveAction.OperationID == "" {
		t.Fatalf("frost valve outcome = %+v, want executed operation", valveAction)
	}
	if !h.frost.Available("fv-1") {
		t.Fatal("frost valve must be released after completed operation")
	}
}

func TestEvaluateStaleTemperatureReturnsUnknownAndTriggersNothing(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	now := time.Now().UTC()
	stale := now.Add(-30 * time.Minute)
	addTemperature(t, h, "s1", 1, 1.5, stale)
	addReturnPressure(t, h, "s1", 1, 0.46, now)

	result, err := h.surge.Evaluate(context.Background(), "s1", time.Hour, "fv-1", true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Assessment.Level != protection.RiskUnknown {
		t.Fatalf("level = %s, want unknown", result.Assessment.Level)
	}
	if len(result.Assessment.Actions) != 0 || len(result.Outcomes) != 0 {
		t.Fatalf("unknown result must have no actions/outcomes, got %+v", result)
	}
	if h.antifreeze.Running("s1") {
		t.Fatal("antifreeze must not start from stale telemetry")
	}
	if h.latches.Get("s1").FreezeRisk {
		t.Fatal("freeze-risk latch must not arm from stale telemetry")
	}
	if h.frost.Available("fv-1") != true {
		t.Fatal("frost valve must remain untouched")
	}
	if result.Assessment.EstimatedTriggerAt != nil {
		t.Fatal("unknown assessment must not predict a trigger time")
	}
}

func TestEvaluateMissingReturnPressureReturnsUnknown(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	addTemperature(t, h, "s1", 1, 1.5, time.Now().UTC())

	result, err := h.surge.Evaluate(context.Background(), "s1", time.Hour, "fv-1", true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Assessment.Level != protection.RiskUnknown {
		t.Fatalf("level = %s, want unknown", result.Assessment.Level)
	}
	if len(result.Outcomes) != 0 {
		t.Fatalf("missing pressure must not produce outcomes, got %+v", result.Outcomes)
	}
}

func TestEvaluatePlanOnlyDoesNotAct(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	now := time.Now().UTC()
	addTemperature(t, h, "s1", 1, 1.5, now)
	addReturnPressure(t, h, "s1", 1, 0.46, now)

	result, err := h.surge.Evaluate(context.Background(), "s1", time.Hour, "fv-1", false)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Executed {
		t.Fatal("plan-only result must report executed=false")
	}
	for _, outcome := range result.Outcomes {
		if outcome.Executed {
			t.Fatalf("planned action %s must not be executed", outcome.Kind)
		}
	}
	if h.antifreeze.Running("s1") {
		t.Fatal("antifreeze must not start in plan-only mode")
	}
	if h.latches.Get("s1").Revision != 0 {
		t.Fatal("latches must not change in plan-only mode")
	}
}

func TestEvaluateLowReturnPressureArmsPressureTrip(t *testing.T) {
	config := protection.DefaultColdSurgeConfig()
	config.LowReturnPressure = 0.3
	h := newHarness(t, config)
	now := time.Now().UTC()
	addTemperature(t, h, "s1", 1, 10, now.Add(-time.Hour))
	addTemperature(t, h, "s1", 2, 6, now)
	addReturnPressure(t, h, "s1", 2, 0.2, now)

	result, err := h.surge.Evaluate(context.Background(), "s1", time.Hour, "fv-1", true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	trip, ok := outcomeByKind(result, protection.ActionSetPressureTripLatch)
	if !ok || !trip.Executed {
		t.Fatalf("pressure trip outcome = %+v, want executed", trip)
	}
	if !h.latches.Get("s1").PressureTrip {
		t.Fatal("pressure-trip latch not armed")
	}
}

func TestEvaluateFrostValveConflictRecordsError(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	now := time.Now().UTC()
	addTemperature(t, h, "s1", 1, 1.5, now)
	addReturnPressure(t, h, "s1", 1, 0.46, now)

	// Another owner holds the valve in a moving operation; frost protection
	// must report the conflict instead of forcing the valve.
	if _, err := h.operations.Start("fv-1", "maintenance:other", 100); err != nil {
		t.Fatalf("claim frost valve: %v", err)
	}

	result, err := h.surge.Evaluate(context.Background(), "s1", time.Hour, "fv-1", true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	valveAction, ok := outcomeByKind(result, protection.ActionOpenFrostValve)
	if !ok {
		t.Fatal("critical assessment must still attempt the frost valve action")
	}
	if valveAction.Executed || valveAction.Error == "" {
		t.Fatalf("valve outcome = %+v, want non-executed conflict error", valveAction)
	}

	// The other protection links remain active even though the valve is blocked.
	latches := h.latches.Get("s1")
	if !latches.FreezeRisk {
		t.Fatal("freeze-risk latch must still arm when the frost valve is blocked")
	}
	if !h.antifreeze.Running("s1") {
		t.Fatal("antifreeze circulation must still start when the frost valve is blocked")
	}
}

func TestEvaluateCriticalWithoutValveIDReportsActionError(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	now := time.Now().UTC()
	addTemperature(t, h, "s2", 1, 1.5, now)
	addReturnPressure(t, h, "s2", 1, 0.46, now)

	result, err := h.surge.Evaluate(context.Background(), "s2", time.Hour, "", true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	valveAction, ok := outcomeByKind(result, protection.ActionOpenFrostValve)
	if !ok || valveAction.Executed || valveAction.Error == "" {
		t.Fatalf("valve outcome = %+v, want a non-executed missing-valve error", valveAction)
	}
}

func TestEvaluateRapidSamplesFallBackToForecastRate(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	now := time.Now().UTC()
	// Backfilled watermarks arriving within seconds cannot establish a trend.
	addTemperature(t, h, "s1", 1, 58, now.Add(-time.Second))
	addTemperature(t, h, "s1", 2, 1.5, now)
	addReturnPressure(t, h, "s1", 2, 0.46, now)

	result, err := h.surge.Evaluate(context.Background(), "s1", time.Hour, "fv-1", false)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Assessment.Level == protection.RiskUnknown {
		t.Fatal("fresh telemetry must not be unknown")
	}
	if result.Assessment.CoolingRateCelsiusPerHour != 2 {
		t.Fatalf("cooling rate = %.1f, want forecast fallback 2 C/h", result.Assessment.CoolingRateCelsiusPerHour)
	}
}

func TestUpdateConfigRejectsInvalidThresholds(t *testing.T) {
	h := newHarness(t, protection.DefaultColdSurgeConfig())
	bad := protection.DefaultColdSurgeConfig()
	bad.CriticalCelsius = 20
	if err := h.surge.UpdateConfig(bad); err == nil {
		t.Fatal("expected invalid config rejection")
	}
}
