package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wyw14/cry-97/internal/model"
)

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

// writeControlError maps a device-control failure to 423 Locked when the target
// is under maintenance, otherwise to the supplied fallback status.
func writeControlError(w http.ResponseWriter, fallback int, err error) {
	if errors.Is(err, model.ErrDeviceLocked) {
		writeError(w, http.StatusLocked, err)
		return
	}
	writeError(w, fallback, err)
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
