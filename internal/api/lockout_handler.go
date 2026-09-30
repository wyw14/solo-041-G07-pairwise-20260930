package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/process"
)

type tagDeviceRequest struct {
	Reason      string              `json:"reason"`
	Owner       string              `json:"owner"`
	TaggedBy    string              `json:"tagged_by"`
	ExtraChecks []model.SafetyCheck `json:"extra_checks"`
}

type lockCheckRequest struct {
	Code        string `json:"code"`
	ConfirmedBy string `json:"confirmed_by"`
}

type preRunRequest struct {
	Note string `json:"note"`
}

type releaseLockRequest struct {
	ReleasedBy string `json:"released_by"`
}

func (s *Server) lockoutList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":  s.plant.Devices(),
		"lockouts": s.plant.Lockouts(),
	})
}

func (s *Server) tagDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	var input tagDeviceRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.Reason == "" {
		writeError(w, http.StatusBadRequest, errors.New("lockout reason is required"))
		return
	}
	if input.Owner == "" {
		writeError(w, http.StatusBadRequest, errors.New("lockout responsible owner is required"))
		return
	}
	record, err := s.plant.TagDevice(r.Context(), process.TagDeviceInput{
		DeviceID: deviceID, Reason: input.Reason, Owner: input.Owner,
		TaggedBy: input.TaggedBy, ExtraChecks: input.ExtraChecks,
	})
	if err != nil {
		writeLockoutError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (s *Server) confirmLockCheck(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	var input lockCheckRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.plant.ConfirmLockCheck(r.Context(), deviceID, input.Code, input.ConfirmedBy)
	if err != nil {
		writeLockoutError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) verifyPreRun(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	var input preRunRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.plant.VerifyPreRun(r.Context(), deviceID, input.Note)
	if err != nil {
		writeLockoutError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) releaseLock(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	var input releaseLockRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.plant.ReleaseDevice(r.Context(), deviceID, input.ReleasedBy)
	if err != nil {
		writeLockoutError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// writeLockoutError maps maintenance workflow failures to HTTP status codes.
func writeLockoutError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrDeviceLocked):
		writeError(w, http.StatusLocked, err)
	case errors.Is(err, model.ErrLockoutNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, model.ErrLockoutAlreadyActive), errors.Is(err, model.ErrChecksIncomplete):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusConflict, err)
	}
}
