package asynqmon

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Prometheus proxy basic-auth tests (upstream hibiken/asynqmon#248): the
// /api/metrics handler fans one browser request out into several PromQL
// queries against the configured Prometheus; every one of those proxied
// requests must carry the configured Authorization header — and none when no
// credentials are configured. A stub httptest.Server records the headers.
// ****************************************************************************

// stubPrometheus records the Authorization header of every request it serves
// and answers with a minimal valid query_range payload.
type stubPrometheus struct {
	mu      sync.Mutex
	headers []string
}

func (s *stubPrometheus) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}
}

func (s *stubPrometheus) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.headers...)
}

func TestMetricsProxyBasicAuth(t *testing.T) {
	// With credentials configured, every proxied query authenticates.
	authStub := &stubPrometheus{}
	authSrv := httptest.NewServer(authStub.handler())
	t.Cleanup(authSrv.Close)
	authHandler := newGetMetricsHandlerFunc(authSrv.Client(), authSrv.URL, parsePrometheusBasicAuth("alice:s3cret"))
	authW := getJSON(t, authHandler, "/api/metrics", nil)
	authHeaders := authStub.seen()
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))

	// Without credentials, no Authorization header is invented.
	plainStub := &stubPrometheus{}
	plainSrv := httptest.NewServer(plainStub.handler())
	t.Cleanup(plainSrv.Close)
	plainHandler := newGetMetricsHandlerFunc(plainSrv.Client(), plainSrv.URL, parsePrometheusBasicAuth(""))
	plainW := getJSON(t, plainHandler, "/api/metrics", nil)
	plainHeaders := plainStub.seen()

	// Colon-less credential parsing (bare username, empty password).
	bare := parsePrometheusBasicAuth("token-only")

	Convey("Given the Prometheus metrics proxy (#248)", t, func() {
		Convey("When basic-auth credentials are configured", func() {
			Convey("Then the proxy answers 200 and every proxied query carries them", func() {
				So(authW.Code, ShouldEqual, http.StatusOK)
				So(len(authHeaders), ShouldBeGreaterThan, 1) // one per PromQL fan-out query
				for _, h := range authHeaders {
					So(h, ShouldEqual, wantHeader)
				}
			})
		})
		Convey("When no credentials are configured", func() {
			Convey("Then no Authorization header is sent", func() {
				So(plainW.Code, ShouldEqual, http.StatusOK)
				So(len(plainHeaders), ShouldBeGreaterThan, 1)
				for _, h := range plainHeaders {
					So(h, ShouldBeEmpty)
				}
			})
		})
		Convey("When the credential string has no colon", func() {
			Convey("Then it parses as a bare username with an empty password", func() {
				So(bare, ShouldNotBeNil)
				So(bare.username, ShouldEqual, "token-only")
				So(bare.password, ShouldBeEmpty)
			})
		})
	})
}
