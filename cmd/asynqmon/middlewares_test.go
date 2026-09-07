package main

import (
	"net"
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

// securityHeaders (#34) must stamp every response of the binary, including
// the routes the asynqmon handler does not serve (/metrics) and the CSRF
// rejection bodies.
func TestSecurityHeadersMiddleware(t *testing.T) {
	ok := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(ok, httptest.NewRequest("GET", "/metrics", nil))

	Convey("Given the securityHeaders middleware (#34)", t, func() {
		Convey("When any response passes through it", func() {
			Convey("Then the clickjacking and sniffing guards are set", func() {
				So(ok.Header().Get("X-Content-Type-Options"), ShouldEqual, "nosniff")
				So(ok.Header().Get("X-Frame-Options"), ShouldEqual, "DENY")
				So(ok.Header().Get("Referrer-Policy"), ShouldEqual, "same-origin")
			})
			Convey("Then the CSP forbids framing and foreign origins", func() {
				csp := ok.Header().Get("Content-Security-Policy")
				So(csp, ShouldContainSubstring, "default-src 'self'")
				So(csp, ShouldContainSubstring, "frame-ancestors 'none'")
				So(csp, ShouldContainSubstring, "connect-src 'self'")
				So(csp, ShouldContainSubstring, "img-src 'self' data:")
			})
		})
	})
}

// parseTrustedProxyCIDRs accepts CIDRs and bare IPs and rejects junk, so a
// typo fails at startup instead of silently trusting nobody (#53.2).
func TestParseTrustedProxyCIDRs(t *testing.T) {
	Convey("Given the --trusted-proxies parser (#53)", t, func() {
		Convey("When the list mixes CIDRs, bare IPs and whitespace", func() {
			nets, err := parseTrustedProxyCIDRs([]string{"10.0.0.0/8", " 192.168.1.5 ", "", "::1"})
			Convey("Then every entry becomes a network", func() {
				So(err, ShouldBeNil)
				So(len(nets), ShouldEqual, 3)
				So(nets[0].Contains(net.ParseIP("10.1.2.3")), ShouldBeTrue)
				So(nets[1].Contains(net.ParseIP("192.168.1.5")), ShouldBeTrue)
				So(nets[1].Contains(net.ParseIP("192.168.1.6")), ShouldBeFalse)
				So(nets[2].Contains(net.ParseIP("::1")), ShouldBeTrue)
			})
		})
		Convey("When an entry is malformed", func() {
			_, err := parseTrustedProxyCIDRs([]string{"10.0.0.0/8", "nonsense"})
			Convey("Then it is rejected", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "nonsense")
			})
		})
		Convey("When the list is empty", func() {
			nets, err := parseTrustedProxyCIDRs(nil)
			Convey("Then no network is trusted", func() {
				So(err, ShouldBeNil)
				So(nets, ShouldBeEmpty)
			})
		})
	})
}

// csrfProtection with X-Forwarded-Host (#53.2). A reverse proxy configured
// with passHostHeader=false rewrites Host, so the Origin never matched and
// every browser mutation returned 403. The forwarded host is honored only
// when the peer is inside --trusted-proxies; an untrusted peer must not be
// able to forge it.
func TestCSRFForwardedHost(t *testing.T) {
	trusted, err := parseTrustedProxyCIDRs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("parsing the trusted proxies: %v", err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	do := func(mw func(http.Handler) http.Handler, remoteAddr, origin, forwardedHost string) int {
		req := httptest.NewRequest("PUT", "/api/views/v1", nil)
		req.Host = "asynqmon.internal:8080" // what the proxy rewrote it to
		req.RemoteAddr = remoteAddr
		req.Header.Set("Origin", origin)
		if forwardedHost != "" {
			req.Header.Set("X-Forwarded-Host", forwardedHost)
		}
		rec := httptest.NewRecorder()
		mw(next).ServeHTTP(rec, req)
		return rec.Code
	}

	withProxies := csrfProtection(nil, trusted)
	noProxies := csrfProtection(nil, nil)

	Convey("Given a reverse proxy that rewrites the Host header (#53)", t, func() {
		Convey("When a trusted proxy forwards the browser's host", func() {
			Convey("Then the mutation is accepted", func() {
				So(do(withProxies, "10.1.2.3:5000", "https://asynqmon.example.com", "asynqmon.example.com"), ShouldEqual, http.StatusNoContent)
			})
		})
		Convey("When an untrusted peer sends the same header", func() {
			Convey("Then the mutation is still rejected", func() {
				So(do(withProxies, "203.0.113.9:5000", "https://asynqmon.example.com", "asynqmon.example.com"), ShouldEqual, http.StatusForbidden)
			})
		})
		Convey("When no trusted proxies are configured", func() {
			Convey("Then the forwarded host is ignored", func() {
				So(do(noProxies, "10.1.2.3:5000", "https://asynqmon.example.com", "asynqmon.example.com"), ShouldEqual, http.StatusForbidden)
			})
		})
		Convey("When the origin matches the real Host", func() {
			Convey("Then it is accepted with no forwarded host at all", func() {
				So(do(noProxies, "10.1.2.3:5000", "https://asynqmon.internal:8080", ""), ShouldEqual, http.StatusNoContent)
			})
		})
		Convey("When a trusted proxy forwards a chain of hosts", func() {
			Convey("Then the first entry, the browser's host, is compared", func() {
				So(do(withProxies, "10.1.2.3:5000", "https://asynqmon.example.com", "asynqmon.example.com, inner.svc"), ShouldEqual, http.StatusNoContent)
			})
		})
		Convey("When a trusted proxy forwards a host the origin does not match", func() {
			Convey("Then the mutation is rejected", func() {
				So(do(withProxies, "10.1.2.3:5000", "https://evil.example", "asynqmon.example.com"), ShouldEqual, http.StatusForbidden)
			})
		})
	})
}
