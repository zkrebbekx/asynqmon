package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/zkrebbekx/asynqmon"
)

// A responseRecorderWriter records response status and size.
// It implements http.ResponseWriter interface.
type responseRecorderWriter struct {
	http.ResponseWriter
	// The status code that the server sends back to the client.
	status int
	// The size of the object returned to the client, not including the response headers.
	size int
}

func (w *responseRecorderWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	w.status = status
}

func (w *responseRecorderWriter) Write(b []byte) (int, error) {
	// If WriteHeader is not called explicitly, the first call to Write
	// will trigger an implicit WriteHeader(http.StatusOK).
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.size += n
	return n, err
}

// Unwrap exposes the wrapped ResponseWriter so http.ResponseController can
// reach the optional interfaces this wrapper would otherwise hide — Flush
// for the /api/fleet/events SSE stream, and per-request write-deadline
// control (SSE clears the server's WriteTimeout for its own response).
func (w *responseRecorderWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush implements http.Flusher for callers that type-assert directly
// instead of going through http.ResponseController. Delegates only when the
// wrapped writer really supports flushing (net/http's writers always do).
func (w *responseRecorderWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// securityHeaders sets the dashboard's security response headers
// (X-Content-Type-Options, X-Frame-Options, Referrer-Policy,
// Content-Security-Policy) on every response of the binary, including
// /metrics and the CSRF rejection bodies. The asynqmon handler sets the
// same set on its own SPA responses (and the index page replaces the CSP
// with a nonce-bearing one), so a header set here is overwritten only by
// the handler's own values.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asynqmon.SetSecurityHeaders(w.Header())
		h.ServeHTTP(w, r)
	})
}

// parseTrustedProxyCIDRs parses the --trusted-proxies entries. An entry is
// a CIDR ("10.0.0.0/8") or a bare IP ("10.0.0.1", read as /32 or /128).
// Entries are trimmed and empty entries are skipped. A malformed entry is
// an error.
func parseTrustedProxyCIDRs(entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, fmt.Errorf("--trusted-proxies: invalid entry %q", e)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, ipnet, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("--trusted-proxies: invalid entry %q: %w", e, err)
		}
		nets = append(nets, ipnet)
	}
	return nets, nil
}

// peerIsTrustedProxy reports whether the request's RemoteAddr is inside one
// of the trusted proxy networks. An empty list trusts no peer.
func peerIsTrustedProxy(r *http.Request, trusted []*net.IPNet) bool {
	if len(trusted) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// csrfProtection rejects cross-origin mutating requests. Browsers attach an
// Origin header to every cross-origin request — including simple form POSTs
// that never trigger a CORS preflight — so checking it here blocks CSRF
// against the unauthenticated dashboard. Requests without an Origin header
// (curl, scripts, same-origin GET navigations) are unaffected.
//
// The Origin host is compared with the request Host. When the peer is one of
// trustedProxies, the X-Forwarded-Host header is accepted as well, so a
// reverse proxy that rewrites Host does not turn every mutation into a 403.
func csrfProtection(allowedOrigins []string, trustedProxies []*net.IPNet) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(o), "/"))] = true
	}
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				origin := r.Header.Get("Origin")
				if origin != "" && !allowed[strings.ToLower(origin)] && !sameOrigin(origin, r, trustedProxies) {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
			h.ServeHTTP(w, r)
		})
	}
}

// sameOrigin reports whether the given Origin header value points at the host
// this request was addressed to: the Host header, or X-Forwarded-Host when
// the peer is a trusted proxy.
func sameOrigin(origin string, r *http.Request, trustedProxies []*net.IPNet) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if !peerIsTrustedProxy(r, trustedProxies) {
		return false
	}
	// A proxy chain may append several hosts; the first one is the host
	// the browser addressed.
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	fwd = strings.TrimSpace(fwd)
	return fwd != "" && strings.EqualFold(u.Host, fwd)
}

// recoverPanics answers a panicking handler with 500 and the JSON body
// {"error":"internal error"}, and logs the panic value with the stack.
// net/http would otherwise close the connection without a response and
// print the stack itself.
//
// Apply it outermost, so that it also covers a panic inside another
// middleware. A response that already started keeps its status code: the
// panic is logged, but the body stays as the handler left it.
//
// http.ErrAbortHandler is re-raised, because net/http uses it to abort a
// response on purpose and expects to see it.
func recoverPanics(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseRecorderWriter{ResponseWriter: w}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			log.Printf("asynqmon: panic in handler %s %s: %v\n%s", r.Method, r.URL.Path, p, debug.Stack())
			if rw.status != 0 {
				return // the response already started
			}
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.WriteHeader(http.StatusInternalServerError)
			io.WriteString(rw, `{"error":"internal error"}`)
		}()
		h.ServeHTTP(rw, r)
	})
}

func loggingMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseRecorderWriter{ResponseWriter: w}
		h.ServeHTTP(rw, r)

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		username := "-"
		if user := r.URL.User; user != nil {
			username = user.Username()
		}
		size := "-"
		if rw.size > 0 {
			size = strconv.Itoa(rw.size)
		}
		// Write a log in Apache common log format (http://httpd.apache.org/docs/2.2/logs.html#common).
		fmt.Fprintf(os.Stdout, "%s - %s [%s] \"%s %s %s\" %d %s\n",
			host, username, time.Now().Format("02/Jan/2006:15:04:05 -0700"),
			r.Method, r.URL, r.Proto, rw.status, size)
	})
}
