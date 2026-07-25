package asynqmon

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines the request shape, validation, and asynq.Option mapping
// for the flag-gated enqueue capability (build contract §5.10):
//
//   POST /api/queues/{qname}/tasks
//
// The HTTP layer lives in enqueue_handlers.go; this file is pure logic so the
// bounds and mappings are testable without a server. Every rejection carries
// a field-specific message ("<field>: <why>") for the clone-and-edit modal
// (§3.5) to surface inline.
// ****************************************************************************

const (
	// maxEnqueuePayloadBytes caps the decoded payload at 1MB. Redis handles
	// far larger values, but a dashboard is not a bulk-data ingest path.
	maxEnqueuePayloadBytes = 1 << 20

	// maxEnqueueBodyBytes bounds the request body reader: the payload cap
	// with base64 expansion (4/3) plus generous headroom for the other
	// fields. Anything larger fails the decode, never a handler OOM.
	maxEnqueueBodyBytes = maxEnqueuePayloadBytes*4/3 + 64*1024

	// Sane numeric bounds (§5.10). These are dashboard guardrails against
	// typos (extra zeros), not asynq limits.
	maxEnqueueMaxRetry         = 1000
	maxEnqueueTimeoutSeconds   = int64(7 * 24 * 60 * 60)   // 7 days
	maxEnqueueRetentionSeconds = int64(90 * 24 * 60 * 60)  // 90 days
	maxEnqueueUniqueTTLSeconds = int64(30 * 24 * 60 * 60)  // 30 days
	maxEnqueueProcessInSeconds = int64(365 * 24 * 60 * 60) // 1 year

	// maxEnqueueScheduleAhead bounds process_at: more than a year out is
	// almost certainly a typed-the-wrong-year mistake.
	maxEnqueueScheduleAhead = 365 * 24 * time.Hour
)

// enqueueTaskRequest is the JSON body of POST /api/queues/{qname}/tasks.
// Field names are a frontend contract (ui/src/api.ts EnqueueTaskRequest).
// Pointer fields distinguish "absent" (use asynq defaults) from zero.
type enqueueTaskRequest struct {
	// Type is the task type name. Required.
	Type string `json:"type"`
	// Payload is the task payload. With PayloadBase64 it is base64-encoded
	// binary; otherwise the string bytes are used verbatim. May be empty.
	Payload       string `json:"payload"`
	PayloadBase64 bool   `json:"payload_base64"`

	MaxRetry         *int   `json:"max_retry"`
	TimeoutSeconds   *int64 `json:"timeout_seconds"`
	Deadline         string `json:"deadline"` // RFC3339
	RetentionSeconds *int64 `json:"retention_seconds"`
	UniqueTTLSeconds *int64 `json:"unique_ttl_seconds"`

	// ProcessAt (RFC3339) and ProcessInSeconds are mutually exclusive ways
	// to schedule the task; absent both, the task is due immediately.
	ProcessAt        string `json:"process_at"`
	ProcessInSeconds *int64 `json:"process_in_seconds"`

	// Reason is optional free text recorded on the audit entry.
	Reason string `json:"reason"`
}

// buildEnqueueTask validates the request against the §5.10 bounds and maps it
// onto (type, payload, asynq options). A non-empty errMsg is a 400 with a
// field-specific message; the other returns are then meaningless.
func buildEnqueueTask(qname string, req *enqueueTaskRequest, now time.Time) (taskType string, payload []byte, opts []asynq.Option, errMsg string) {
	fail := func(format string, a ...interface{}) (string, []byte, []asynq.Option, string) {
		return "", nil, nil, fmt.Sprintf(format, a...)
	}

	if strings.TrimSpace(qname) == "" {
		return fail("queue: name must not be blank")
	}
	taskType = strings.TrimSpace(req.Type)
	if taskType == "" {
		return fail("type: required")
	}

	if req.PayloadBase64 {
		decoded, err := base64.StdEncoding.DecodeString(req.Payload)
		if err != nil {
			return fail("payload: invalid base64 (%v)", err)
		}
		payload = decoded
	} else {
		payload = []byte(req.Payload)
	}
	if len(payload) > maxEnqueuePayloadBytes {
		return fail("payload: exceeds the %d byte (1MB) limit (got %d bytes)", maxEnqueuePayloadBytes, len(payload))
	}

	opts = []asynq.Option{asynq.Queue(qname)}

	if req.MaxRetry != nil {
		if *req.MaxRetry < 0 {
			return fail("max_retry: must be >= 0")
		}
		if *req.MaxRetry > maxEnqueueMaxRetry {
			return fail("max_retry: must be <= %d", maxEnqueueMaxRetry)
		}
		opts = append(opts, asynq.MaxRetry(*req.MaxRetry))
	}
	if req.TimeoutSeconds != nil {
		if *req.TimeoutSeconds < 0 {
			return fail("timeout_seconds: must be >= 0")
		}
		if *req.TimeoutSeconds > maxEnqueueTimeoutSeconds {
			return fail("timeout_seconds: must be <= %d (7 days)", maxEnqueueTimeoutSeconds)
		}
		opts = append(opts, asynq.Timeout(time.Duration(*req.TimeoutSeconds)*time.Second))
	}
	if req.Deadline != "" {
		t, err := time.Parse(time.RFC3339, req.Deadline)
		if err != nil {
			return fail("deadline: must be RFC3339 (e.g. 2026-01-02T15:04:05Z)")
		}
		opts = append(opts, asynq.Deadline(t))
	}
	if req.RetentionSeconds != nil {
		if *req.RetentionSeconds < 0 {
			return fail("retention_seconds: must be >= 0")
		}
		if *req.RetentionSeconds > maxEnqueueRetentionSeconds {
			return fail("retention_seconds: must be <= %d (90 days)", maxEnqueueRetentionSeconds)
		}
		opts = append(opts, asynq.Retention(time.Duration(*req.RetentionSeconds)*time.Second))
	}
	if req.UniqueTTLSeconds != nil {
		if *req.UniqueTTLSeconds < 1 {
			// asynq itself refuses Unique TTLs under a second.
			return fail("unique_ttl_seconds: must be >= 1")
		}
		if *req.UniqueTTLSeconds > maxEnqueueUniqueTTLSeconds {
			return fail("unique_ttl_seconds: must be <= %d (30 days)", maxEnqueueUniqueTTLSeconds)
		}
		opts = append(opts, asynq.Unique(time.Duration(*req.UniqueTTLSeconds)*time.Second))
	}

	if req.ProcessAt != "" && req.ProcessInSeconds != nil {
		return fail("process_at and process_in_seconds are mutually exclusive — set one")
	}
	if req.ProcessAt != "" {
		t, err := time.Parse(time.RFC3339, req.ProcessAt)
		if err != nil {
			return fail("process_at: must be RFC3339 (e.g. 2026-01-02T15:04:05Z)")
		}
		if t.Sub(now) > maxEnqueueScheduleAhead {
			return fail("process_at: more than 1 year in the future")
		}
		// A past process_at is fine: the task is simply due immediately.
		opts = append(opts, asynq.ProcessAt(t))
	}
	if req.ProcessInSeconds != nil {
		if *req.ProcessInSeconds < 0 {
			return fail("process_in_seconds: must be >= 0")
		}
		if *req.ProcessInSeconds > maxEnqueueProcessInSeconds {
			return fail("process_in_seconds: must be <= %d (1 year)", maxEnqueueProcessInSeconds)
		}
		opts = append(opts, asynq.ProcessIn(time.Duration(*req.ProcessInSeconds)*time.Second))
	}

	return taskType, payload, opts, ""
}
