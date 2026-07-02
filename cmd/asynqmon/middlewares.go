package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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

// csrfProtection rejects cross-origin mutating requests. Browsers attach an
// Origin header to every cross-origin request — including simple form POSTs
// that never trigger a CORS preflight — so checking it here blocks CSRF
// against the unauthenticated dashboard. Requests without an Origin header
// (curl, scripts, same-origin GET navigations) are unaffected.
func csrfProtection(allowedOrigins []string) func(http.Handler) http.Handler {
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
				if origin != "" && !allowed[strings.ToLower(origin)] && !sameOrigin(origin, r) {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
			h.ServeHTTP(w, r)
		})
	}
}

// sameOrigin reports whether the given Origin header value points at the host
// this request was addressed to.
func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
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
