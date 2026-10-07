package protection

import (
	"context"
	"fmt"
	"sync"
	"time"

	"heatpulse/internal/telemetry"
)

// ActionOutcome reports what happened when one planned preventive action was
// executed (or merely planned) against the live protection systems.
type ActionOutcome struct {
	Kind          ActionKind `json:"kind"`
	Executed      bool       `json:"executed"`
	Detail        string     `json:"detail,omitempty"`
	ValveID       string     `json:"valve_id,omitempty"`
	OperationID   string     `json:"operation_id,omitempty"`
	SessionID     string     `json:"session_id,omitempty"`
	LatchRevision uint64     `json:"latch_revision,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// EvaluationResult bundles the prediction with the execution record.
type EvaluationResult struct {
	Assessment Assessment      `json:"assessment"`
	Outcomes   []ActionOutcome `json:"outcomes"`
	Executed   bool            `json:"executed"`
}

// ColdSurgeProtection wires the surge predictor into the existing protection
// mechanisms: antifreeze circulation, frost valve operations and the
// freeze/pressure latch store. When telemetry is stale it only ever returns an
// unknown assessment and never triggers protection.
type ColdSurgeProtection struct {
	series     *telemetry.Series
	antifreeze *Antifreeze
	frost      *FrostProtection
	latches    *LatchStore
	predictor  *SurgePredictor

	mu     sync.RWMutex
	config ColdSurgeConfig
}

func NewColdSurgeProtection(series *telemetry.Series, antifreeze *Antifreeze, frost *FrostProtection, latches *LatchStore, config ColdSurgeConfig) (*ColdSurgeProtection, error) {
	if series == nil || antifreeze == nil || frost == nil || latches == nil {
		return nil, fmt.Errorf("cold surge protection requires series, antifreeze, frost protection and latches")
	}
	if config.MaxDataAge == 0 {
		config = DefaultColdSurgeConfig()
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cold surge config: %w", err)
	}
	return &ColdSurgeProtection{
		series: series, antifreeze: antifreeze, frost: frost, latches: latches,
		predictor: NewSurgePredictor(), config: config,
	}, nil
}

func (c *ColdSurgeProtection) Config() ColdSurgeConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config
}

func (c *ColdSurgeProtection) UpdateConfig(config ColdSurgeConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	c.config = config
	c.mu.Unlock()
	return nil
}

// Evaluate produces an assessment for the station over the future window.
// When execute is true the planned actions are carried out against the live
// antifreeze runner, valve operations and latch store; otherwise they are
// returned as a plan only. valveID overrides the configured frost valve.
func (c *ColdSurgeProtection) Evaluate(requestContext context.Context, stationID string, window time.Duration, valveID string, execute bool) (EvaluationResult, error) {
	if stationID == "" {
		return EvaluationResult{}, fmt.Errorf("station id is required")
	}
	if window <= 0 {
		return EvaluationResult{}, fmt.Errorf("future window must be positive")
	}
	config := c.Config()
	now := time.Now().UTC()

	reading, unknownReasons := c.collectReading(stationID, config, now)
	if len(unknownReasons) > 0 {
		// Stale or missing data: unknown only, no actions, never execute.
		return EvaluationResult{Assessment: UnknownAssessment(stationID, now, unknownReasons), Outcomes: []ActionOutcome{}}, nil
	}

	assessment := c.predictor.Assess(stationID, reading, config, window, now)
	if valveID != "" {
		for i := range assessment.Actions {
			if assessment.Actions[i].Kind == ActionOpenFrostValve && assessment.Actions[i].ValveID == "" {
				assessment.Actions[i].ValveID = valveID
			}
		}
	}

	outcomes := make([]ActionOutcome, 0, len(assessment.Actions))
	if execute {
		outcomes = c.execute(requestContext, assessment, config)
	} else {
		for _, action := range assessment.Actions {
			outcomes = append(outcomes, ActionOutcome{Kind: action.Kind, Detail: "planned_only", ValveID: action.ValveID})
		}
	}
	return EvaluationResult{Assessment: assessment, Outcomes: outcomes, Executed: execute}, nil
}

// collectReading returns stale reasons instead of an error: any staleness or
// gap must surface as an unknown assessment, not an HTTP failure.
func (c *ColdSurgeProtection) collectReading(stationID string, config ColdSurgeConfig, now time.Time) (ColdSurgeReading, []string) {
	reasons := make([]string, 0, 3)

	temp, ok := c.series.Latest(stationID, telemetry.KindTemperature)
	if !ok {
		reasons = append(reasons, "no temperature telemetry recorded for station")
	} else if age := now.Sub(temp.At); age > config.MaxDataAge {
		reasons = append(reasons, fmt.Sprintf("temperature telemetry stale: %.1f minutes old (limit %.1f)", age.Minutes(), config.MaxDataAge.Minutes()))
	}

	pressure, okPressure := c.series.Latest(stationID, telemetry.KindReturnPressure)
	if !okPressure {
		reasons = append(reasons, "no return pressure telemetry recorded for station")
	} else if age := now.Sub(pressure.At); age > config.MaxDataAge {
		reasons = append(reasons, fmt.Sprintf("return pressure telemetry stale: %.1f minutes old (limit %.1f)", age.Minutes(), config.MaxDataAge.Minutes()))
	}

	if len(reasons) > 0 {
		return ColdSurgeReading{}, reasons
	}

	reading := ColdSurgeReading{
		TemperatureCelsius: temp.Value, TemperatureAt: temp.At,
		ReturnPressure: pressure.Value, PressureAt: pressure.At,
	}

	// Derive the temperature trend from the two most recent samples so the
	// projection reacts to the observed cooling rate when available. Samples
	// arriving within minTrendInterval (e.g. backfilled watermarks) cannot
	// establish a physical trend; fall back to the forecast cooling rate.
	watermarks := c.series.Watermarks(stationID, []telemetry.Kind{telemetry.KindTemperature})
	if len(watermarks) >= 2 {
		if previous, ok := c.series.At(stationID, telemetry.KindTemperature, watermarks[len(watermarks)-2]); ok {
			elapsed := temp.At.Sub(previous.At)
			if elapsed >= minTrendInterval {
				reading.TrendCelsiusPerHour = (temp.Value - previous.Value) / elapsed.Hours()
				reading.TrendKnown = true
			}
		}
	}
	return reading, nil
}

// minTrendInterval is the shortest spacing between consecutive temperature
// samples that still permits deriving a physical cooling trend.
const minTrendInterval = 30 * time.Second

func (c *ColdSurgeProtection) execute(requestContext context.Context, assessment Assessment, config ColdSurgeConfig) []ActionOutcome {
	outcomes := make([]ActionOutcome, 0, len(assessment.Actions))
	for _, action := range assessment.Actions {
		if err := requestContext.Err(); err != nil {
			outcomes = append(outcomes, ActionOutcome{Kind: action.Kind, Error: err.Error()})
			continue
		}
		outcome := ActionOutcome{Kind: action.Kind, ValveID: action.ValveID}
		switch action.Kind {
		case ActionReviewLatches:
			outcome.Detail = "review_only"
		case ActionStartAntifreeze:
			c.applyAntifreeze(requestContext, assessment.StationID, &outcome)
		case ActionSetFreezeRiskLatch:
			c.applyLatch(assessment.StationID, true, false, &outcome)
		case ActionSetPressureTripLatch:
			c.applyLatch(assessment.StationID, true, true, &outcome)
		case ActionOpenFrostValve:
			c.applyFrostValve(assessment.StationID, action.ValveID, &outcome)
		default:
			outcome.Error = fmt.Sprintf("unknown preventive action %q", action.Kind)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

func (c *ColdSurgeProtection) applyAntifreeze(requestContext context.Context, stationID string, outcome *ActionOutcome) {
	if c.antifreeze.Running(stationID) {
		if session, ok := c.antifreeze.Session(stationID); ok {
			outcome.SessionID = session.ID
		}
		outcome.Executed = true
		outcome.Detail = "antifreeze circulation already active"
		return
	}
	session, err := c.antifreeze.Start(requestContext, stationID)
	if err != nil {
		outcome.Error = fmt.Sprintf("start antifreeze circulation: %v", err)
		return
	}
	outcome.SessionID = session.ID
	outcome.Executed = true
	outcome.Detail = "antifreeze circulation started"
}

func (c *ColdSurgeProtection) applyLatch(stationID string, freezeRisk, pressureTrip bool, outcome *ActionOutcome) {
	// Revision races are possible with concurrent evaluators; re-read and
	// retry a bounded number of times instead of dropping the protection.
	for attempt := 0; attempt < 3; attempt++ {
		current := c.latches.Get(stationID)
		candidate := current
		candidate.StationID = stationID
		candidate.Revision = current.Revision + 1
		if candidate.Revision == 0 {
			candidate.Revision = 1
		}
		if freezeRisk {
			candidate.FreezeRisk = true
		}
		if pressureTrip {
			candidate.PressureTrip = true
		}
		applied, err := c.latches.Apply(candidate)
		if err == nil {
			outcome.Executed = true
			outcome.LatchRevision = applied.Revision
			switch {
			case freezeRisk && pressureTrip:
				outcome.Detail = "freeze-risk and pressure-trip latches armed"
			case freezeRisk:
				outcome.Detail = "freeze-risk latch armed"
			default:
				outcome.Detail = "pressure-trip latch armed"
			}
			return
		}
		if attempt == 2 {
			outcome.Error = fmt.Sprintf("arm latch: %v", err)
		}
	}
}

func (c *ColdSurgeProtection) applyFrostValve(stationID, valveID string, outcome *ActionOutcome) {
	if valveID == "" {
		outcome.Error = "frost valve required: configure frost_valve_id or supply valve_id"
		return
	}
	if !c.frost.Available(valveID) {
		outcome.ValveID = valveID
		outcome.Error = fmt.Sprintf("frost valve %s owned by another operation", valveID)
		return
	}
	operation, err := c.frost.OpenValve(stationID, valveID)
	if err != nil {
		outcome.ValveID = valveID
		outcome.Error = fmt.Sprintf("open frost valve: %v", err)
		return
	}
	outcome.ValveID = valveID
	outcome.OperationID = operation.ID
	outcome.Executed = true
	outcome.Detail = "frost admission valve opened"
}
