package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/maintenance"
)

type lockoutRequest struct {
	Reason      string   `json:"reason"`
	Responsible string   `json:"responsible"`
	Checks      []string `json:"checks"`
}

type restoreRequest struct {
	Checks []string `json:"checks"`
}

func (s *Server) maintenanceDevices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":  s.plant.MaintenanceDevices(),
		"lockouts": s.plant.ActiveLockouts(),
	})
}

func (s *Server) activeLockouts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"lockouts": s.plant.ActiveLockouts()})
}

func (s *Server) placeLockout(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	var input lockoutRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.Reason == "" {
		writeError(w, http.StatusBadRequest, errors.New("maintenance reason is required"))
		return
	}
	if input.Responsible == "" {
		writeError(w, http.StatusBadRequest, errors.New("responsible person is required"))
		return
	}
	record, err := s.plant.PlaceLockout(r.Context(), deviceID, input.Reason, input.Responsible, input.Checks)
	if err != nil {
		writeLockoutError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (s *Server) clearLockout(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	var input restoreRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.plant.ClearLockout(r.Context(), deviceID, input.Checks)
	if err != nil {
		writeLockoutError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeLockoutError maps domain failures onto suitable HTTP statuses.
func writeLockoutError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, maintenance.ErrDeviceLockedOut):
		writeError(w, http.StatusConflict, err)
	default:
		// Unknown device, incomplete checklist, failed verification.
		writeError(w, http.StatusUnprocessableEntity, err)
	}
}
