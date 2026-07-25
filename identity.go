package asynqmon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ****************************************************************************
// This file defines:
//   - the actor-resolution middleware (build contract §5.11): trusted
//     reverse-proxy header → basic-auth user → hard-refuse (opt-in) →
//     anonymous@<ip>
//
// Identity is resolution + audit only; authorization stays at the proxy
// layer (§7 non-goals — no RBAC).
// ****************************************************************************

// actorCtxKey carries the resolved actor through the request context.
type actorCtxKey struct{}

// actorInfo is the resolved identity for one request.
type actorInfo struct {
	Name string
	// Attributed is false when the name is the anonymous@<ip> fallback.
	Attributed bool
}

// actorFromContext returns the actor resolved by the middleware, or the
// zero value when the middleware is not installed (direct handler tests).
func actorFromContext(ctx context.Context) actorInfo {
	if a, ok := ctx.Value(actorCtxKey{}).(actorInfo); ok {
		return a
	}
	return actorInfo{}
}

// parseTrustedProxies parses CIDR strings, panicking on malformed input —
// misconfigured trust boundaries must fail loudly at boot, not silently
// trust (or distrust) the wrong network. Bare IPs are accepted as /32 (/128).
func parseTrustedProxies(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
				continue
			}
			panic(fmt.Sprintf("asynqmon: invalid trusted proxy %q (want ip or cidr)", raw))
		}
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			panic(fmt.Sprintf("asynqmon: invalid trusted proxy cidr %q: %v", raw, err))
		}
		out = append(out, ipnet)
	}
	return out
}

// remoteIP extracts the peer IP from RemoteAddr (never from forwarded
// headers — those are what the trust check is protecting against).
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isMutating reports whether the request can change state. Mirrors the
// read-only middleware's method test.
func isMutating(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, "":
		return false
	}
	return true
}

// newActorMiddleware builds the §5.11 actor-resolution chain:
//
//  1. Options.AuthHeader (e.g. X-Auth-Request-User) — trusted only when the
//     peer is inside Options.TrustedProxies (any peer when no CIDRs are
//     configured, for single-proxy deployments where the network is the
//     boundary);
//  2. basic-auth username;
//  3. with Options.RequireIdentity, mutating requests without (1)/(2) are
//     refused with a 403 JSON error;
//  4. otherwise "anonymous@<remote-ip>", flagged unattributed.
func newActorMiddleware(opts Options) func(http.Handler) http.Handler {
	trusted := parseTrustedProxies(opts.TrustedProxies)
	headerName := opts.AuthHeader

	proxyTrusted := func(r *http.Request) bool {
		if len(trusted) == 0 {
			return true
		}
		ip := net.ParseIP(remoteIP(r))
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

	resolve := func(r *http.Request) (string, bool) {
		if headerName != "" && proxyTrusted(r) {
			if v := strings.TrimSpace(r.Header.Get(headerName)); v != "" {
				return v, true
			}
		}
		if user, _, ok := r.BasicAuth(); ok && user != "" {
			return user, true
		}
		return "", false
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, attributed := resolve(r)
			if !attributed {
				if opts.RequireIdentity && isMutating(r) {
					writeErrorMsg(w, http.StatusForbidden,
						"identity required: no trusted auth header or basic-auth user on a mutating request (--require-identity)")
					return
				}
				name = "anonymous@" + remoteIP(r)
			}
			ctx := context.WithValue(r.Context(), actorCtxKey{}, actorInfo{Name: name, Attributed: attributed})
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
