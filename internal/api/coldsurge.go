package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"heatpulse/internal/protection"
)

type coldSurgeEvaluationRequest struct {
	WindowMinutes float64 `json:"window_minutes"`
	ValveID       string  `json:"valve_id,omitempty"`
	Execute       bool    `json:"execute"`
}

func (a *App) evaluateColdSurge(w http.ResponseWriter, r *http.Request) {
	input := coldSurgeEvaluationRequest{WindowMinutes: 60}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if input.WindowMinutes <= 0 || input.WindowMinutes > 24*60 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("window_minutes must be within (0, 1440]"))
		return
	}
	result, err := a.ColdSurge.Evaluate(
		r.Context(),
		chi.URLParam(r, "stationID"),
		time.Duration(input.WindowMinutes*float64(time.Minute)),
		input.ValveID,
		input.Execute,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *App) getColdSurgeConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.ColdSurge.Config())
}

func (a *App) putColdSurgeConfig(w http.ResponseWriter, r *http.Request) {
	var config protection.ColdSurgeConfig
	if err := decodeJSON(r, &config); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.ColdSurge.UpdateConfig(config); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, a.ColdSurge.Config())
}
