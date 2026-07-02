package asynqmon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines:
//   - shared helpers to write structured JSON error responses so the frontend
//     can render meaningful messages instead of raw text/plain bodies.
// ****************************************************************************

// errorResponse is the JSON shape of every error body returned by the API.
type errorResponse struct {
	Error string `json:"error"`
}

// writeError writes err as a JSON error body {"error": "..."} with the given
// status code, stripping the "asynq: " prefix from inspector errors.
func writeError(w http.ResponseWriter, code int, err error) {
	writeErrorMsg(w, code, strings.TrimPrefix(err.Error(), "asynq: "))
}

// writeErrorMsg writes msg as a JSON error body with the given status code.
func writeErrorMsg(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

// errorStatus maps well-known inspector errors to HTTP status codes so that
// e.g. polling a queue deleted in another tab yields a 404, not a 500.
func errorStatus(err error) int {
	switch {
	case errors.Is(err, asynq.ErrQueueNotFound), errors.Is(err, asynq.ErrTaskNotFound):
		return http.StatusNotFound
	case errors.Is(err, asynq.ErrQueueNotEmpty):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
