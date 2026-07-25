package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestResponseRecorderWriterStreaming guards the SSE contract of the logging
// middleware: its ResponseWriter wrapper must not hide http.Flusher (the
// /api/fleet/events stream would otherwise buffer forever behind it), both
// via a direct type assertion and via the http.ResponseController Unwrap
// convention.
func TestResponseRecorderWriterStreaming(t *testing.T) {
	Convey("Given the logging middleware's ResponseWriter wrapper", t, func() {
		rec := httptest.NewRecorder()
		rw := &responseRecorderWriter{ResponseWriter: rec}

		Convey("When a handler type-asserts http.Flusher directly", func() {
			f, ok := interface{}(rw).(http.Flusher)
			Convey("Then the wrapper satisfies it and delegates the flush", func() {
				So(ok, ShouldBeTrue)
				f.Flush()
				So(rec.Flushed, ShouldBeTrue)
			})
		})

		Convey("When a handler goes through http.ResponseController", func() {
			ctrl := http.NewResponseController(rw)
			Convey("Then Flush reaches the underlying writer via Unwrap", func() {
				So(ctrl.Flush(), ShouldBeNil)
				So(rec.Flushed, ShouldBeTrue)
			})
		})

		Convey("When the wrapper is unwrapped", func() {
			Convey("Then it exposes exactly the writer it wraps", func() {
				So(rw.Unwrap(), ShouldEqual, rec)
			})
		})

		Convey("When the underlying writer cannot flush", func() {
			bare := &responseRecorderWriter{ResponseWriter: nonFlushingWriter{header: http.Header{}}}
			Convey("Then Flush is a safe no-op rather than a panic", func() {
				So(func() { bare.Flush() }, ShouldNotPanic)
			})
		})
	})
}

// nonFlushingWriter is a minimal ResponseWriter with no Flush support.
type nonFlushingWriter struct{ header http.Header }

func (w nonFlushingWriter) Header() http.Header         { return w.header }
func (w nonFlushingWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w nonFlushingWriter) WriteHeader(int)             {}
