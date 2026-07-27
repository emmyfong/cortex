package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

// maxJSONBody bounds request bodies for JSON endpoints. Uploads use their own,
// much larger limit.
const maxJSONBody = 1 << 20 // 1 MiB

type errorResponse struct {
	Error string `json:"error"`
}

// writeError sends a JSON error. Messages here are written for the caller and
// must never carry internal detail — log that instead.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

// decodeJSON reads a JSON body, writing an error response and returning false
// if it cannot. Unknown fields are rejected so a typo'd field is reported
// rather than silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}
