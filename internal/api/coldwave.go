package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"heatpulse/internal/protection"
)

// putColdwaveConfig 设置站点风寒配置（风速、湿度、暴露系数）。
func (a *App) putColdwaveConfig(w http.ResponseWriter, r *http.Request) {
	var config protection.WindChillConfig
	if err := decodeJSON(r, &config); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	config.StationID = chi.URLParam(r, "stationID")
	saved, err := a.ColdwavePredictor.SetConfig(config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// getColdwaveConfig 读取站点风寒配置。
func (a *App) getColdwaveConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.ColdwavePredictor.Config(chi.URLParam(r, "stationID")))
}

// evaluateColdwave 结合温度遥测、回水压力、风寒配置与未来时间窗输出风险等级、
// 预计触发时间及预防动作。数据过期时风险等级为 unknown 且动作全部 unavailable。
func (a *App) evaluateColdwave(w http.ResponseWriter, r *http.Request) {
	var request protection.ColdwaveRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	assessment := a.ColdwaveActions.Evaluate(
		chi.URLParam(r, "stationID"), request, time.Now().UTC(),
	)
	writeJSON(w, http.StatusOK, assessment)
}

// engageColdwave 在高/严重风险时实际联动防冻循环、防冻阀与冻结锁存器。
// 数据过期（unknown）时拒绝执行并返回 409。
func (a *App) engageColdwave(w http.ResponseWriter, r *http.Request) {
	var request protection.ColdwaveRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	result, err := a.ColdwaveActions.Engage(
		r.Context(), chi.URLParam(r, "stationID"), request, time.Now().UTC(),
	)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
