package protection

import (
	"fmt"
	"sync"

	"heatpulse/internal/valve"
)

type FrostProtection struct {
	operations *valve.Operations
	mu         sync.RWMutex
	admitted   map[string]string // valveID -> stationID，防冻阀已开启记录
}

func NewFrostProtection(operations *valve.Operations) *FrostProtection {
	return &FrostProtection{operations: operations, admitted: make(map[string]string)}
}

func (f *FrostProtection) OpenValve(stationID, valveID string) (valve.Operation, error) {
	operation, err := f.operations.Start(valveID, "frost:"+stationID, 100)
	if err != nil {
		return valve.Operation{}, fmt.Errorf("frost protection valve admission: %w", err)
	}
	completed, err := f.operations.Complete(operation.ID)
	if err != nil {
		return valve.Operation{}, fmt.Errorf("complete frost valve operation: %w", err)
	}
	f.mu.Lock()
	f.admitted[valveID] = stationID
	f.mu.Unlock()
	return completed, nil
}

func (f *FrostProtection) Available(valveID string) bool {
	return f.operations.Owner(valveID) == ""
}

// Owner 返回阀门当前占用者标识（防冻操作为 "frost:<stationID>"），空串表示空闲。
func (f *FrostProtection) Owner(valveID string) string {
	return f.operations.Owner(valveID)
}

// AdmissionStation 返回最近一次由防冻保护开启该阀门的站点；操作完成后占用即释放，
// 由此记录判断防冻阀是否已处于防冻开启状态。
func (f *FrostProtection) AdmissionStation(valveID string) string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.admitted[valveID]
}
