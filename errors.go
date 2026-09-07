package asynqmon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
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

// Generic 5xx bodies. A server-side failure must not echo the raw Redis or
// Prometheus error to the browser: those strings carry internal addresses
// and URLs. The full error goes to the process log instead.
const (
	msgInternalError    = "internal error"
	msgRedisUnavailable = "redis unavailable"
)

// writeError writes err as a JSON error body {"error": "..."} with the given
// status code. A 4xx body keeps the specific error text with the "asynq: "
// prefix stripped. A 5xx body is generic: "redis unavailable" for 503 and
// "internal error" for every other 5xx. The full error is logged.
func writeError(w http.ResponseWriter, code int, err error) {
	if code >= 500 {
		log.Printf("asynqmon: %d: %v", code, err)
		writeErrorMsg(w, code, genericServerErrorMsg(code))
		return
	}
	writeErrorMsg(w, code, strings.TrimPrefix(err.Error(), "asynq: "))
}

// genericServerErrorMsg returns the client-facing body for a 5xx status.
func genericServerErrorMsg(code int) string {
	if code == http.StatusServiceUnavailable {
		return msgRedisUnavailable
	}
	return msgInternalError
}

// writeErrorMsg writes msg as a JSON error body with the given status code.
func writeErrorMsg(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

// errorStatus maps well-known inspector errors to HTTP status codes so that
// e.g. polling a queue deleted in another tab yields a 404, not a 500, and a
// Redis outage yields a 503, not a 500.
func errorStatus(err error) int {
	switch {
	case errors.Is(err, asynq.ErrQueueNotFound), errors.Is(err, asynq.ErrTaskNotFound):
		return http.StatusNotFound
	case errors.Is(err, asynq.ErrQueueNotEmpty):
		return http.StatusBadRequest
	case isRedisUnavailable(err):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// isRedisUnavailable reports whether err describes a Redis that did not
// answer: a context deadline, a network error (dial failure, i/o timeout,
// connection reset), or a go-redis client/pool state error.
func isRedisUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, redis.ErrClosed) || errors.Is(err, redis.ErrPoolTimeout) || errors.Is(err, redis.ErrPoolExhausted) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
