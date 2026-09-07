package asynqmon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Unit tests for the §5.11 actor-resolution middleware (identity.go). No
// Redis: the middleware only reads the request and stamps the context.
// ****************************************************************************

// resolveActor runs one request through the middleware and returns the
// actor the wrapped handler saw plus the response code.
func resolveActor(opts Options, method, remoteAddr string, headers map[string]string, basicUser string) (actorInfo, int) {
	var seen actorInfo
	h := newActorMiddleware(opts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = actorFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(method, "/api/queues", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if basicUser != "" {
		r.SetBasicAuth(basicUser, "")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return seen, w.Code
}

func TestActorMiddlewareBasicAuth(t *testing.T) {
	const peer = "10.1.2.3:4444"

	Convey("Given the actor middleware and a request carrying a Basic-Auth username", t, func() {
		Convey("When neither TrustBasicAuthUser nor a trusted proxy applies", func() {
			actor, code := resolveActor(Options{}, "GET", peer, nil, "ceo")
			Convey("Then the username is ignored and the request stays anonymous", func() {
				So(code, ShouldEqual, http.StatusOK)
				So(actor.Attributed, ShouldBeFalse)
				So(actor.Name, ShouldEqual, "anonymous@10.1.2.3")
			})
		})

		Convey("When RequireIdentity is set and the request mutates", func() {
			_, code := resolveActor(Options{RequireIdentity: true}, "POST", peer, nil, "ceo")
			Convey("Then the unverified username does not satisfy the requirement", func() {
				So(code, ShouldEqual, http.StatusForbidden)
			})
		})

		Convey("When TrustBasicAuthUser is set", func() {
			actor, _ := resolveActor(Options{TrustBasicAuthUser: true}, "GET", peer, nil, "ceo")
			Convey("Then the username is the attributed actor", func() {
				So(actor.Attributed, ShouldBeTrue)
				So(actor.Name, ShouldEqual, "ceo")
			})
		})

		Convey("When the peer is a trusted proxy", func() {
			opts := Options{TrustedProxies: []string{"10.1.2.0/24"}}
			actor, _ := resolveActor(opts, "GET", peer, nil, "ceo")
			Convey("Then the username is the attributed actor", func() {
				So(actor.Attributed, ShouldBeTrue)
				So(actor.Name, ShouldEqual, "ceo")
			})
		})

		Convey("When the peer is outside the trusted proxy set", func() {
			opts := Options{TrustedProxies: []string{"10.9.0.0/16"}}
			actor, _ := resolveActor(opts, "GET", peer, nil, "ceo")
			Convey("Then the username is ignored", func() {
				So(actor.Attributed, ShouldBeFalse)
				So(actor.Name, ShouldEqual, "anonymous@10.1.2.3")
			})
		})
	})
}

func TestActorMiddlewareForwardedFor(t *testing.T) {
	trusted := Options{TrustedProxies: []string{"10.0.0.0/8"}}

	Convey("Given the actor middleware with TrustedProxies 10.0.0.0/8", t, func() {
		Convey("When an untrusted peer sends X-Forwarded-For", func() {
			actor, _ := resolveActor(trusted, "GET", "192.168.5.5:1234",
				map[string]string{"X-Forwarded-For": "203.0.113.9"}, "")
			Convey("Then the header is ignored and the peer address is used", func() {
				So(actor.Attributed, ShouldBeFalse)
				So(actor.Name, ShouldEqual, "anonymous@192.168.5.5")
			})
		})

		Convey("When a trusted proxy forwards a client through a second trusted hop", func() {
			actor, _ := resolveActor(trusted, "GET", "10.0.0.2:1234",
				map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.7"}, "")
			Convey("Then the rightmost entry outside the trusted set is the client", func() {
				So(actor.Attributed, ShouldBeFalse)
				So(actor.Name, ShouldEqual, "anonymous@203.0.113.9")
			})
		})

		Convey("When a trusted proxy forwards a spoofed left entry and a real client", func() {
			actor, _ := resolveActor(trusted, "GET", "10.0.0.2:1234",
				map[string]string{"X-Forwarded-For": "1.1.1.1, 198.51.100.4"}, "")
			Convey("Then the rightmost untrusted entry wins, not the client-supplied one", func() {
				So(actor.Name, ShouldEqual, "anonymous@198.51.100.4")
			})
		})

		Convey("When every X-Forwarded-For entry is a trusted proxy", func() {
			actor, _ := resolveActor(trusted, "GET", "10.0.0.2:1234",
				map[string]string{"X-Forwarded-For": "10.0.0.7"}, "")
			Convey("Then the peer address is used", func() {
				So(actor.Name, ShouldEqual, "anonymous@10.0.0.2")
			})
		})

		Convey("When the X-Forwarded-For entry is malformed", func() {
			actor, _ := resolveActor(trusted, "GET", "10.0.0.2:1234",
				map[string]string{"X-Forwarded-For": "not-an-ip"}, "")
			Convey("Then the peer address is used", func() {
				So(actor.Name, ShouldEqual, "anonymous@10.0.0.2")
			})
		})

		Convey("When no TrustedProxies are configured and a peer sends X-Forwarded-For", func() {
			actor, _ := resolveActor(Options{}, "GET", "10.0.0.2:1234",
				map[string]string{"X-Forwarded-For": "203.0.113.9"}, "")
			Convey("Then the header is ignored", func() {
				So(actor.Name, ShouldEqual, "anonymous@10.0.0.2")
			})
		})
	})
}

func TestActorMiddlewareAuthHeader(t *testing.T) {
	Convey("Given the actor middleware with AuthHeader X-Auth-Request-User", t, func() {
		hdr := map[string]string{"X-Auth-Request-User": "alice"}

		Convey("When TrustedProxies is set and the peer is inside it", func() {
			opts := Options{AuthHeader: "X-Auth-Request-User", TrustedProxies: []string{"10.0.0.1"}}
			actor, _ := resolveActor(opts, "POST", "10.0.0.1:5555", hdr, "")
			Convey("Then the header names the attributed actor", func() {
				So(actor.Attributed, ShouldBeTrue)
				So(actor.Name, ShouldEqual, "alice")
			})
		})

		Convey("When TrustedProxies is set and the peer is outside it", func() {
			opts := Options{AuthHeader: "X-Auth-Request-User", TrustedProxies: []string{"10.0.0.1"}, RequireIdentity: true}
			actor, code := resolveActor(opts, "POST", "10.0.0.2:5555", hdr, "")
			Convey("Then the header is ignored and the mutation is refused", func() {
				So(code, ShouldEqual, http.StatusForbidden)
				So(actor.Attributed, ShouldBeFalse)
			})
		})

		Convey("When TrustedProxies is empty (single-proxy convenience)", func() {
			opts := Options{AuthHeader: "X-Auth-Request-User", AllowUntrustedAuthHeader: true}
			actor, _ := resolveActor(opts, "GET", "10.0.0.2:5555", hdr, "")
			Convey("Then the header is trusted from any peer", func() {
				So(actor.Attributed, ShouldBeTrue)
				So(actor.Name, ShouldEqual, "alice")
			})
		})
	})
}
