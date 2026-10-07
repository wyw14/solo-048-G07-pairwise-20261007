package protection

import (
	"context"
	"fmt"
	"time"
)

// EngageResult 预防动作执行结果。
type EngageResult struct {
	Assessment ColdwaveAssessment `json:"assessment"`
	Executed   []string           `json:"executed"`
	Errors     []string           `json:"errors,omitempty"`
}

// ActionService 寒潮预防动作服务：评估结果与现有防冻循环、阀门操作、锁存器联动。
type ActionService struct {
	predictor  *Predictor
	antifreeze *Antifreeze
	frost      *FrostProtection
	latches    *LatchStore
	incidents  *Service
}

func NewActionService(
	predictor *Predictor,
	antifreeze *Antifreeze,
	frost *FrostProtection,
	latches *LatchStore,
	incidents *Service,
) *ActionService {
	return &ActionService{
		predictor:  predictor,
		antifreeze: antifreeze,
		frost:      frost,
		latches:    latches,
		incidents:  incidents,
	}
}

// Evaluate 执行一次评估，并把预防动作与三套保护系统的当前实况关联起来。
func (s *ActionService) Evaluate(stationID string, request ColdwaveRequest, now time.Time) ColdwaveAssessment {
	assessment := s.predictor.Assess(stationID, request, now)
	links := s.links(stationID, request.FrostValve)
	assessment.FreezeLatched = links.FreezeRiskLatch
	assessment.Actions = PlanActions(stationID, assessment.RiskLevel, links, request.FrostValve)
	return assessment
}

// links 汇总预防动作关联的保护系统实况。
func (s *ActionService) links(stationID, frostValve string) ActionLinks {
	links := ActionLinks{AntifreezeRunning: s.antifreeze.Running(stationID)}
	if frostValve != "" {
		links.ValveOwner = s.valveHolder(stationID, frostValve)
	}
	current := s.latches.Get(stationID)
	links.FreezeRiskLatch = current.FreezeRisk
	links.PressureTripLatch = current.PressureTrip
	return links
}

// valveHolder 返回防冻阀当前的实际持有者：进行中的阀门操作优先，否则参考
// 最近一次防冻开启记录（"frost:<stationID>"），空串表示空闲。
func (s *ActionService) valveHolder(stationID, frostValve string) string {
	if owner := s.frost.Owner(frostValve); owner != "" {
		return owner
	}
	if admitted := s.frost.AdmissionStation(frostValve); admitted != "" {
		return "frost:" + admitted
	}
	return ""
}

// Engage 在高/严重风险时实际执行预防动作。数据过期（unknown）时一律拒绝，
// 不得对防冻循环、阀门或锁存器做任何变更。
func (s *ActionService) Engage(requestContext context.Context, stationID string, request ColdwaveRequest, now time.Time) (EngageResult, error) {
	if err := requestContext.Err(); err != nil {
		return EngageResult{}, err
	}
	assessment := s.Evaluate(stationID, request, now)
	result := EngageResult{Assessment: assessment, Executed: []string{}}

	if assessment.RiskLevel == RiskUnknown {
		return result, fmt.Errorf("coldwave risk unknown for %s (stale data), protective actions refused", stationID)
	}
	if assessment.RiskLevel != RiskHigh && assessment.RiskLevel != RiskSevere {
		// 低/升温风险下仅待命，不触发保护资源。
		return result, nil
	}

	// 1) 防冻循环：未运行则启动；已运行则保持。
	if !s.antifreeze.Running(stationID) {
		if _, err := s.antifreeze.Start(requestContext, stationID); err != nil {
			result.Errors = append(result.Errors, "antifreeze circulation: "+err.Error())
		} else {
			result.Executed = append(result.Executed, "antifreeze_circulation")
		}
	}

	// 2) 防冻阀：仅在指定且空闲时由防冻保护开启；被他人占用则记录阻塞。
	if request.FrostValve != "" {
		switch owner := s.valveHolder(stationID, request.FrostValve); {
		case owner == "frost:"+stationID:
			// 已由本站防冻保护开启，无需重复操作。
		case owner != "":
			result.Errors = append(result.Errors, "frost valve "+request.FrostValve+" blocked by "+owner)
		default:
			if _, err := s.frost.OpenValve(stationID, request.FrostValve); err != nil {
				result.Errors = append(result.Errors, "frost valve admission: "+err.Error())
			} else {
				result.Executed = append(result.Executed, "frost_valve_admission")
			}
		}
	}

	// 3) 锁存器：置位 FreezeRisk（回水压力偏弱时同时置位 PressureTrip），
	//    沿用 revision 乐观锁，保留其它锁存位；已置位则不重复写。
	current := s.latches.Get(stationID)
	if !current.FreezeRisk || (assessment.PressureWeak && !current.PressureTrip) {
		candidate := current
		candidate.StationID = stationID
		candidate.Revision = current.Revision + 1 // 零值时从 1 开始
		candidate.FreezeRisk = true
		if assessment.PressureWeak {
			candidate.PressureTrip = true
		}
		if _, err := s.latches.Apply(candidate); err != nil {
			result.Errors = append(result.Errors, "freeze latch: "+err.Error())
		} else {
			result.Executed = append(result.Executed, "freeze_latch")
		}
	}

	// 4) 严重风险上报保护事件，供值班与审计跟踪。
	severity := "warning"
	message := "coldwave forecast crosses freeze threshold within window"
	if assessment.RiskLevel == RiskSevere {
		severity = "critical"
		message = "apparent temperature at/below freeze threshold"
	}
	if _, err := s.incidents.Raise(stationID, "coldwave_freeze", severity, message); err != nil {
		result.Errors = append(result.Errors, "incident: "+err.Error())
	}

	// 重新评估，使动作计划反映联动后的最终状态。
	result.Assessment = s.Evaluate(stationID, request, now)
	return result, nil
}
