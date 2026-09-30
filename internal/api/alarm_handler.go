package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *Server) alarmList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"alarms": s.plant.Alarms(), "lockouts": s.plant.ActiveLockouts(),
	})
}

func (s *Server) acknowledgeAlarm(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "alarmID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	alarm, err := s.plant.AcknowledgeAlarm(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, alarm)
}

func (s *Server) recoverAlarm(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "alarmID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	alarm, err := s.plant.RecoverAlarm(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, alarm)
}
