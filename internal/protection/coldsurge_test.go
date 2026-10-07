package protection

import (
	"testing"
	"time"
)

func readingAt(temp, pressure float64, at time.Time) ColdSurgeReading {
	return ColdSurgeReading{
		TemperatureCelsius: temp, TemperatureAt: at,
		ReturnPressure: pressure, PressureAt: at,
	}
}

func TestAssessSafeWhenTemperatureStaysAboveWatch(t *testing.T) {
	predictor := NewSurgePredictor()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	assessment := predictor.Assess("s1", readingAt(20, 0.5, now), DefaultColdSurgeConfig(), time.Hour, now)

	if assessment.Level != RiskSafe {
		t.Fatalf("level = %s, want safe", assessment.Level)
	}
	if len(assessment.Actions) != 0 {
		t.Fatalf("safe assessment must carry no actions, got %d", len(assessment.Actions))
	}
	if assessment.EstimatedTriggerAt == nil {
		t.Fatal("expected an estimated trigger time for the critical threshold")
	}
	// 20C - 2C/h reaches 2C in 9 hours.
	want := now.Add(9 * time.Hour)
	if !assessment.EstimatedTriggerAt.Equal(want) {
		t.Fatalf("eta = %v, want %v", assessment.EstimatedTriggerAt, want)
	}
}

func TestAssessWatchPlansLatchReview(t *testing.T) {
	predictor := NewSurgePredictor()
	config := DefaultColdSurgeConfig()
	now := time.Now().UTC()
	reading := readingAt(10, 0.5, now)
	reading.TrendKnown = true
	reading.TrendCelsiusPerHour = -3 // cooling 3 C/h; projected 7C within the hour
	assessment := predictor.Assess("s1", reading, config, time.Hour, now)

	if assessment.Level != RiskWatch {
		t.Fatalf("level = %s, want watch", assessment.Level)
	}
	if len(assessment.Actions) != 1 || assessment.Actions[0].Kind != ActionReviewLatches {
		t.Fatalf("watch actions = %+v, want review_latches only", assessment.Actions)
	}
	if assessment.ProjectedMinCelsius == nil || *assessment.ProjectedMinCelsius != 7 {
		t.Fatalf("projected min = %v, want 7", assessment.ProjectedMinCelsius)
	}
}

func TestAssessHighArmsLatchAndAntifreeze(t *testing.T) {
	predictor := NewSurgePredictor()
	config := DefaultColdSurgeConfig()
	now := time.Now().UTC()
	reading := readingAt(6, 0.5, now)
	reading.TrendKnown = true
	reading.TrendCelsiusPerHour = -3 // projected 3C -> warning band
	assessment := predictor.Assess("s1", reading, config, time.Hour, now)

	if assessment.Level != RiskHigh {
		t.Fatalf("level = %s, want high", assessment.Level)
	}
	kinds := actionKinds(assessment.Actions)
	if !containsKind(kinds, ActionSetFreezeRiskLatch) || !containsKind(kinds, ActionStartAntifreeze) {
		t.Fatalf("high actions = %v, want freeze latch + antifreeze", kinds)
	}
	if containsKind(kinds, ActionSetPressureTripLatch) {
		t.Fatalf("healthy return pressure must not arm pressure trip, got %v", kinds)
	}
}

func TestAssessCriticalWhenAlreadyBelowThreshold(t *testing.T) {
	predictor := NewSurgePredictor()
	config := DefaultColdSurgeConfig()
	config.FrostValveID = "north-fv"
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	assessment := predictor.Assess("s1", readingAt(1.5, 0.5, now), config, time.Hour, now)

	if assessment.Level != RiskCritical {
		t.Fatalf("level = %s, want critical", assessment.Level)
	}
	if assessment.EstimatedTriggerAt == nil || !assessment.EstimatedTriggerAt.Equal(now) {
		t.Fatalf("eta = %v, want now (%v)", assessment.EstimatedTriggerAt, now)
	}
	var valveAction *PreventiveAction
	for i := range assessment.Actions {
		if assessment.Actions[i].Kind == ActionOpenFrostValve {
			valveAction = &assessment.Actions[i]
		}
	}
	if valveAction == nil {
		t.Fatal("critical assessment must plan frost valve admission")
	}
	if valveAction.ValveID != "north-fv" || !valveAction.Urgent {
		t.Fatalf("frost action = %+v, want urgent action on configured valve", valveAction)
	}
}

func TestLowReturnPressureEscalatesAndAcceleratesCooling(t *testing.T) {
	predictor := NewSurgePredictor()
	config := DefaultColdSurgeConfig()
	now := time.Now().UTC()

	// 10C cooling 1C/h -> projected 9C: watch at healthy pressure, but at the
	// low-pressure limit the cooling rate doubles -> projected 8C, and the
	// pressure escalation rule must lift watch to high.
	reading := readingAt(10, config.LowReturnPressure, now)
	reading.TrendKnown = true
	reading.TrendCelsiusPerHour = -1
	assessment := predictor.Assess("s1", reading, config, time.Hour, now)

	if assessment.Level != RiskHigh {
		t.Fatalf("level = %s, want high after low-pressure escalation", assessment.Level)
	}
	if assessment.CoolingRateCelsiusPerHour != 2 {
		t.Fatalf("cooling rate = %.1f, want 2 C/h at the low-pressure factor", assessment.CoolingRateCelsiusPerHour)
	}
	if !containsKind(actionKinds(assessment.Actions), ActionSetPressureTripLatch) {
		t.Fatalf("actions = %+v, want pressure-trip latch", assessment.Actions)
	}
}

func TestWindChillAddsCoolingRate(t *testing.T) {
	predictor := NewSurgePredictor()
	config := DefaultColdSurgeConfig()
	config.WindChill.WindSpeedMPS = 10
	config.WindChill.ExposureFactor = 1.5
	now := time.Now().UTC()
	// No observed trend: fallback 2 C/h + 0.6 * 10 * 1.5 = 9 C/h wind, so 11 C/h.
	assessment := predictor.Assess("s1", readingAt(12, 0.5, now), config, time.Hour, now)
	if assessment.CoolingRateCelsiusPerHour != 11 {
		t.Fatalf("cooling rate = %.1f, want 11 C/h", assessment.CoolingRateCelsiusPerHour)
	}
	if assessment.Level != RiskCritical {
		t.Fatalf("level = %s, want critical (projected 1C reaches critical threshold)", assessment.Level)
	}
}

func TestUnknownAssessmentHasNoActions(t *testing.T) {
	now := time.Now().UTC()
	assessment := UnknownAssessment("s1", now, []string{"stale temperature"})
	if assessment.Level != RiskUnknown {
		t.Fatalf("level = %s, want unknown", assessment.Level)
	}
	if assessment.EstimatedTriggerAt != nil || assessment.ProjectedMinCelsius != nil {
		t.Fatal("unknown assessment must not predict a trigger time or minimum")
	}
	if len(assessment.Actions) != 0 {
		t.Fatalf("unknown assessment must carry no actions, got %d", len(assessment.Actions))
	}
}

func TestConfigValidate(t *testing.T) {
	config := DefaultColdSurgeConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	config.WatchCelsius = 1
	if err := config.Validate(); err == nil {
		t.Fatal("expected threshold ordering error")
	}
}

func TestConfigJSONRoundTrip(t *testing.T) {
	config := DefaultColdSurgeConfig()
	raw, err := config.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ColdSurgeConfig
	if err := decoded.UnmarshalJSON(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.MaxDataAge != config.MaxDataAge {
		t.Fatalf("max data age = %v, want %v", decoded.MaxDataAge, config.MaxDataAge)
	}
	if decoded.WatchCelsius != config.WatchCelsius {
		t.Fatalf("watch threshold = %.1f, want %.1f", decoded.WatchCelsius, config.WatchCelsius)
	}
}

func actionKinds(actions []PreventiveAction) []ActionKind {
	kinds := make([]ActionKind, 0, len(actions))
	for _, action := range actions {
		kinds = append(kinds, action.Kind)
	}
	return kinds
}

func containsKind(kinds []ActionKind, want ActionKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}
