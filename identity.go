package asynqmon

import (
	"context"
	"fmt"
	"log"
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

// ipInNets reports whether ip is inside one of the networks.
func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedClientIP returns the client IP behind a trusted proxy chain: the
// rightmost X-Forwarded-For entry that is not itself a trusted proxy. The
// caller must have checked that the peer is a trusted proxy. It returns ""
// when the header is absent or every entry is a trusted proxy or malformed.
func forwardedClientIP(r *http.Request, trusted []*net.IPNet) string {
	var entries []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		entries = append(entries, strings.Split(h, ",")...)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		s := strings.TrimSpace(entries[i])
		ip := net.ParseIP(s)
		if ip == nil {
			// A malformed entry ends the walk: everything to its left was
			// appended by hops we cannot reason about.
			return ""
		}
		if ipInNets(ip, trusted) {
			continue
		}
		return ip.String()
	}
	return ""
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
//     boundary; the asynqmon binary refuses that configuration unless
//     Options.AllowUntrustedAuthHeader is set);
//  2. basic-auth username — accepted only when Options.TrustBasicAuthUser
//     is set or the peer is inside a non-empty Options.TrustedProxies set.
//     The middleware never verifies the password, so an unverified
//     username is a forgeable actor and is ignored by default;
//  3. with Options.RequireIdentity, mutating requests without (1)/(2) are
//     refused with a 403 JSON error;
//  4. otherwise "anonymous@<client-ip>", flagged unattributed. The client
//     IP is the peer address, or, when the peer is a trusted proxy, the
//     rightmost X-Forwarded-For entry that is not a trusted proxy.
func newActorMiddleware(opts Options) func(http.Handler) http.Handler {
	trusted := parseTrustedProxies(opts.TrustedProxies)
	headerName := opts.AuthHeader

	// Trusting the header from ANY peer is a convenience for single-proxy
	// deployments where the network is the boundary — but it means any
	// client that can reach the listener directly (sidecar bypass,
	// cluster-internal access) can forge an arbitrary actor: audit entries
	// get attributed to the victim's name, and RequireIdentity is satisfied
	// by the spoofed header. Warn loudly so the insecure default is a
	// choice, not an accident.
	if headerName != "" && len(trusted) == 0 {
		suffix := ""
		if opts.RequireIdentity {
			suffix = " With RequireIdentity set, a spoofed header also fully satisfies the identity requirement."
		}
		log.Printf("asynqmon: AuthHeader %q is trusted from EVERY peer because TrustedProxies is empty — "+
			"any client that can reach this listener directly can forge the audit actor. "+
			"Set TrustedProxies (--trusted-proxies) to the CIDRs of your reverse proxy "+
			"(AllowUntrustedAuthHeader / --allow-untrusted-auth-header acknowledges this risk).%s",
			headerName, suffix)
	}

	// peerIsTrustedProxy is the strict check: a non-empty trusted set that
	// contains the peer. It gates the basic-auth username and the
	// X-Forwarded-For client IP.
	peerIsTrustedProxy := func(r *http.Request) bool {
		return len(trusted) > 0 && ipInNets(net.ParseIP(remoteIP(r)), trusted)
	}

	// headerTrusted keeps the documented single-proxy convenience: an empty
	// trusted set trusts the header from any peer (with the warning above).
	headerTrusted := func(r *http.Request) bool {
		return len(trusted) == 0 || peerIsTrustedProxy(r)
	}

	resolve := func(r *http.Request) (string, bool) {
		if headerName != "" && headerTrusted(r) {
			if v := strings.TrimSpace(r.Header.Get(headerName)); v != "" {
				return v, true
			}
		}
		if opts.TrustBasicAuthUser || peerIsTrustedProxy(r) {
			if user, _, ok := r.BasicAuth(); ok && user != "" {
				return user, true
			}
		}
		return "", false
	}

	// clientIP is the address the anonymous@ fallback names.
	clientIP := func(r *http.Request) string {
		if peerIsTrustedProxy(r) {
			if ip := forwardedClientIP(r, trusted); ip != "" {
				return ip
			}
		}
		return remoteIP(r)
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, attributed := resolve(r)
			if !attributed {
				if opts.RequireIdentity && isMutating(r) {
					writeErrorMsg(w, http.StatusForbidden,
						"identity required: no trusted auth header or trusted basic-auth user on a mutating request (--require-identity)")
					return
				}
				name = "anonymous@" + clientIP(r)
			}
			ctx := context.WithValue(r.Context(), actorCtxKey{}, actorInfo{Name: name, Attributed: attributed})
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
