package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/monitor"
)

type startBatchRequest struct {
	FlowM3Hour float64 `json:"flow_m3_hour"`
	Source     string  `json:"source"`
}

type advanceBatchRequest struct {
	Stage model.ProcessStage `json:"stage"`
}

func (s *Server) processLines(w http.ResponseWriter, _ *http.Request) {
	lines := s.plant.Lines()
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": monitor.SummarizeLines(lines), "stages": monitor.CountStages(lines),
		"permits": s.plant.Permits(), "lockouts": s.plant.Lockouts(),
	})
}

func (s *Server) startBatch(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input startBatchRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.Source == "" {
		writeError(w, http.StatusBadRequest, errors.New("intake source is required"))
		return
	}
	batch, err := s.plant.StartBatch(r.Context(), lineID, input.FlowM3Hour, input.Source)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, batch)
}

func (s *Server) advanceBatch(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input advanceBatchRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	batch, err := s.plant.AdvanceBatch(r.Context(), lineID, input.Stage)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (s *Server) lineStatus(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, err := s.plant.Status(lineID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
