package asynqmon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Error-response tests (#53 item 1, #39). A 5xx body must not echo the raw
// Redis or Prometheus error: the dashboard has no login and those strings
// carry internal addresses. A Redis outage must map to 503, not 500.
// ****************************************************************************

// bodyError decodes the {"error": "..."} body of a recorder.
func bodyError(rec *httptest.ResponseRecorder) string {
	var body errorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error
}

func TestWriteErrorHidesServerErrors(t *testing.T) {
	leaky := errors.New(`dial tcp 10.4.5.6:6379: connect: connection refused`)

	rec500 := httptest.NewRecorder()
	writeError(rec500, http.StatusInternalServerError, leaky)

	rec503 := httptest.NewRecorder()
	writeError(rec503, http.StatusServiceUnavailable, leaky)

	rec502 := httptest.NewRecorder()
	writeError(rec502, http.StatusBadGateway, leaky)

	rec404 := httptest.NewRecorder()
	writeError(rec404, http.StatusNotFound, fmt.Errorf("asynq: %w", asynq.ErrQueueNotFound))

	rec400 := httptest.NewRecorder()
	writeError(rec400, http.StatusBadRequest, errors.New("asynq: queue is not empty"))

	Convey("Given an error carrying an internal redis address (#53)", t, func() {
		Convey("When it is written as a 500", func() {
			Convey("Then the body is generic and names no address", func() {
				So(rec500.Code, ShouldEqual, http.StatusInternalServerError)
				So(bodyError(rec500), ShouldEqual, "internal error")
				So(rec500.Body.String(), ShouldNotContainSubstring, "10.4.5.6")
			})
		})
		Convey("When it is written as a 503", func() {
			Convey("Then the body names the unavailable dependency only", func() {
				So(rec503.Code, ShouldEqual, http.StatusServiceUnavailable)
				So(bodyError(rec503), ShouldEqual, "redis unavailable")
				So(rec503.Body.String(), ShouldNotContainSubstring, "10.4.5.6")
			})
		})
		Convey("When it is written as any other 5xx", func() {
			Convey("Then the body is generic too", func() {
				So(bodyError(rec502), ShouldEqual, "internal error")
			})
		})
	})

	Convey("Given a mapped client error (#53)", t, func() {
		Convey("When it is written as a 404", func() {
			Convey("Then the specific text survives, minus the asynq prefix", func() {
				So(rec404.Code, ShouldEqual, http.StatusNotFound)
				So(bodyError(rec404), ShouldContainSubstring, "queue not found")
				So(bodyError(rec404), ShouldNotStartWith, "asynq: ")
			})
		})
		Convey("When it is written as a 400", func() {
			Convey("Then the specific text survives", func() {
				So(bodyError(rec400), ShouldEqual, "queue is not empty")
			})
		})
	})
}

// errorStatus must map a Redis outage to 503 (#39): handlers returned 500,
// which reads as an asynqmon bug rather than a dependency outage.
func TestErrorStatusMapsRedisOutagesTo503(t *testing.T) {
	timeoutErr := &net.OpError{Op: "read", Err: &timeoutError{}}

	Convey("Given the inspector error mapper (#39)", t, func() {
		Convey("When the queue or task is gone", func() {
			Convey("Then it maps to 404", func() {
				So(errorStatus(fmt.Errorf("asynq: %w", asynq.ErrQueueNotFound)), ShouldEqual, http.StatusNotFound)
				So(errorStatus(fmt.Errorf("asynq: %w", asynq.ErrTaskNotFound)), ShouldEqual, http.StatusNotFound)
			})
		})
		Convey("When the queue is not empty", func() {
			Convey("Then it maps to 400", func() {
				So(errorStatus(fmt.Errorf("asynq: %w", asynq.ErrQueueNotEmpty)), ShouldEqual, http.StatusBadRequest)
			})
		})
		Convey("When redis does not answer", func() {
			Convey("Then a deadline maps to 503", func() {
				So(errorStatus(context.DeadlineExceeded), ShouldEqual, http.StatusServiceUnavailable)
				So(errorStatus(fmt.Errorf("inspecting: %w", context.DeadlineExceeded)), ShouldEqual, http.StatusServiceUnavailable)
			})
			Convey("Then a network error maps to 503", func() {
				So(errorStatus(timeoutErr), ShouldEqual, http.StatusServiceUnavailable)
				So(errorStatus(&net.OpError{Op: "dial", Err: errors.New("connection refused")}), ShouldEqual, http.StatusServiceUnavailable)
			})
			Convey("Then a go-redis client error maps to 503", func() {
				So(errorStatus(redis.ErrClosed), ShouldEqual, http.StatusServiceUnavailable)
				So(errorStatus(redis.ErrPoolTimeout), ShouldEqual, http.StatusServiceUnavailable)
			})
		})
		Convey("When the error is none of those", func() {
			Convey("Then it stays a 500", func() {
				So(errorStatus(errors.New("something else")), ShouldEqual, http.StatusInternalServerError)
			})
		})
	})
}

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
