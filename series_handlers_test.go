package asynqmon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// PURE handler tests for GET /api/series[, /batch] and the markers
// endpoints: parameter validation, response shape, the batch cap and null
// no-sample encoding — against injected fakes, no Redis. (The real
// sweep→ring→HTTP pipeline is integration-tested in series_integration_test.go
// against DB 8.)
// ****************************************************************************

// fakeSeriesReader returns one canned series per spec: three points with a
// null gap in the middle, echoing the spec's identity.
func fakeSeriesReader(t0 int64) seriesReader {
	return func(_ context.Context, specs []stats.SeriesSpec, now time.Time) ([]*stats.Series, error) {
		out := make([]*stats.Series, len(specs))
		for i, spec := range specs {
			v1, v2 := int64(5), int64(9)
			out[i] = &stats.Series{
				Scope:  spec.Scope,
				Metric: spec.Metric,
				Window: spec.Window,
				Period: 30,
				Points: []stats.SeriesPoint{
					{T: t0, V: &v1},
					{T: t0 + 30, V: nil},
					{T: t0 + 60, V: &v2},
				},
				LearningUntil: time.Unix(t0, 0).Add(24 * time.Hour),
			}
		}
		return out, nil
	}
}

func getSeriesJSON(t *testing.T, h http.HandlerFunc, target string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	var body map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr, body
}

func TestGetSeriesHandler(t *testing.T) {
	t0 := int64(1_753_400_010)
	h := newGetSeriesHandlerFunc(fakeSeriesReader(t0))

	Convey("Given GET /api/series", t, func() {
		Convey("When called with a valid fleet spec", func() {
			rr, body := getSeriesJSON(t, h, "/api/series?scope=fleet&metric=pending&window=3h")

			Convey("Then it answers the frozen shape with null no-samples", func() {
				So(rr.Code, ShouldEqual, http.StatusOK)
				So(body["scope"], ShouldEqual, "fleet")
				So(body["metric"], ShouldEqual, "pending")
				So(body["window"], ShouldEqual, "3h")
				So(body["period"], ShouldEqual, 30)
				So(body["learning_until"], ShouldNotBeEmpty)
				points := body["points"].([]interface{})
				So(points, ShouldHaveLength, 3)
				p0 := points[0].(map[string]interface{})
				So(p0["t"], ShouldEqual, float64(t0))
				So(p0["v"], ShouldEqual, 5)
				p1 := points[1].(map[string]interface{})
				v, present := p1["v"]
				So(present, ShouldBeTrue) // the key is always there…
				So(v, ShouldBeNil)        // …and null means no-sample, never zero
			})
		})

		Convey("Then queue and server scopes parse", func() {
			rr, body := getSeriesJSON(t, h, "/api/series?scope=queue:email&metric=failed_delta&window=24h")
			So(rr.Code, ShouldEqual, http.StatusOK)
			So(body["scope"], ShouldEqual, "queue:email")

			rr, body = getSeriesJSON(t, h, "/api/series?scope=server:host1:99&metric=busy_workers&window=3h")
			So(rr.Code, ShouldEqual, http.StatusOK)
			So(body["scope"], ShouldEqual, "server:host1:99")
		})

		Convey("Then a bad scope is a 400", func() {
			rr, body := getSeriesJSON(t, h, "/api/series?scope=galaxy&metric=pending&window=3h")
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			So(body["error"], ShouldContainSubstring, "invalid series scope")
		})

		Convey("Then a metric the scope does not collect is a 400 (server scope has no pending)", func() {
			rr, body := getSeriesJSON(t, h, "/api/series?scope=server:h:1&metric=pending&window=3h")
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			So(body["error"], ShouldContainSubstring, "not collected for scope")
		})

		Convey("Then a bad window is a 400", func() {
			rr, body := getSeriesJSON(t, h, "/api/series?scope=fleet&metric=pending&window=7d")
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			So(body["error"], ShouldContainSubstring, "invalid window")
		})

		Convey("Then a bad points value is a 400", func() {
			rr, _ := getSeriesJSON(t, h, "/api/series?scope=fleet&metric=pending&window=3h&points=zero")
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			rr, _ = getSeriesJSON(t, h, "/api/series?scope=fleet&metric=pending&window=3h&points=100000")
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
		})
	})
}

func TestGetSeriesBatchHandler(t *testing.T) {
	t0 := int64(1_753_400_010)
	h := newGetSeriesBatchHandlerFunc(fakeSeriesReader(t0))

	batchURL := func(specs string) string {
		return "/api/series/batch?specs=" + url.QueryEscape(specs)
	}

	Convey("Given GET /api/series/batch", t, func() {
		Convey("When called with two specs", func() {
			rr, body := getSeriesJSON(t, h, batchURL(
				`[{"scope":"fleet","metric":"pending","window":"3h","points":60},`+
					`{"scope":"queue:email","metric":"failed_delta","window":"24h"}]`))

			Convey("Then the response mirrors the specs order", func() {
				So(rr.Code, ShouldEqual, http.StatusOK)
				series := body["series"].([]interface{})
				So(series, ShouldHaveLength, 2)
				s0 := series[0].(map[string]interface{})
				s1 := series[1].(map[string]interface{})
				So(s0["scope"], ShouldEqual, "fleet")
				So(s0["metric"], ShouldEqual, "pending")
				So(s1["scope"], ShouldEqual, "queue:email")
				So(s1["metric"], ShouldEqual, "failed_delta")
			})
		})

		Convey("Then a missing/empty/malformed specs parameter is a 400", func() {
			rr, _ := getSeriesJSON(t, h, "/api/series/batch")
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			rr, _ = getSeriesJSON(t, h, batchURL("[]"))
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			rr, _ = getSeriesJSON(t, h, batchURL("{not json"))
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
		})

		Convey("Then one invalid spec fails the whole batch with its index", func() {
			rr, body := getSeriesJSON(t, h, batchURL(
				`[{"scope":"fleet","metric":"pending","window":"3h"},`+
					`{"scope":"fleet","metric":"bogus","window":"3h"}]`))
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			So(body["error"], ShouldContainSubstring, "specs[1]")
		})

		Convey("Then the 60-spec cap holds", func() {
			var specs []string
			for i := 0; i < 61; i++ {
				specs = append(specs, `{"scope":"fleet","metric":"pending","window":"3h"}`)
			}
			rr, body := getSeriesJSON(t, h, batchURL("["+strings.Join(specs, ",")+"]"))
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
			So(body["error"], ShouldContainSubstring, "capped at 60")
		})
	})
}

// fakeMarkerStore records adds and serves canned lists.
type fakeMarkerStore struct {
	added []Marker
	list  []Marker
	since time.Time
}

func (f *fakeMarkerStore) Add(_ context.Context, m Marker) error {
	f.added = append(f.added, m)
	return nil
}

func (f *fakeMarkerStore) List(_ context.Context, since time.Time) ([]Marker, error) {
	f.since = since
	return f.list, nil
}

func TestMarkerHandlers(t *testing.T) {
	Convey("Given POST /api/markers", t, func() {
		st := &fakeMarkerStore{}
		h := newCreateMarkerHandlerFunc(st, nil) // audit nil: store-level test; audit is integration-covered

		post := func(body string, actor string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", "/api/markers", strings.NewReader(body))
			if actor != "" {
				ctx := context.WithValue(req.Context(), actorCtxKey{}, actorInfo{Name: actor, Attributed: true})
				req = req.WithContext(ctx)
			}
			rr := httptest.NewRecorder()
			h(rr, req)
			return rr
		}

		Convey("When a label is posted with a resolved identity", func() {
			rr := post(`{"label":"deploy v2.4.1"}`, "zac@example.com")

			Convey("Then the marker is stored with the §5.11 actor and echoed back", func() {
				So(rr.Code, ShouldEqual, http.StatusCreated)
				So(st.added, ShouldHaveLength, 1)
				So(st.added[0].Label, ShouldEqual, "deploy v2.4.1")
				So(st.added[0].Actor, ShouldEqual, "zac@example.com")
				So(st.added[0].ID, ShouldNotBeEmpty)
				var m markerJSON
				So(json.Unmarshal(rr.Body.Bytes(), &m), ShouldBeNil)
				So(m.ID, ShouldEqual, st.added[0].ID)
				So(m.Actor, ShouldEqual, "zac@example.com")
				So(m.At, ShouldNotBeEmpty)
			})
		})

		Convey("Then an empty/whitespace label is a 400", func() {
			So(post(`{"label":"  "}`, "a").Code, ShouldEqual, http.StatusBadRequest)
			So(st.added, ShouldBeEmpty)
		})

		Convey("Then an over-long label is a 400", func() {
			So(post(`{"label":"`+strings.Repeat("x", 201)+`"}`, "a").Code, ShouldEqual, http.StatusBadRequest)
		})

		Convey("Then a malformed body is a 400", func() {
			So(post(`{"label": `, "a").Code, ShouldEqual, http.StatusBadRequest)
		})

		Convey("Then a request without identity middleware stays honest as unattributed", func() {
			rr := post(`{"label":"hotfix"}`, "")
			So(rr.Code, ShouldEqual, http.StatusCreated)
			So(st.added[0].Actor, ShouldEqual, "unattributed")
		})
	})

	Convey("Given GET /api/markers", t, func() {
		at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
		st := &fakeMarkerStore{list: []Marker{
			{ID: "m1", Label: "deploy a", Actor: "zac", At: at},
			{ID: "m2", Label: "deploy b", Actor: "ci", At: at.Add(time.Hour)},
		}}
		h := newListMarkersHandlerFunc(st)

		Convey("When listed with the default window", func() {
			rr, body := getSeriesJSON(t, h, "/api/markers")

			Convey("Then markers come back ascending with RFC3339 stamps and a ~24h since bound", func() {
				So(rr.Code, ShouldEqual, http.StatusOK)
				markers := body["markers"].([]interface{})
				So(markers, ShouldHaveLength, 2)
				m0 := markers[0].(map[string]interface{})
				So(m0["id"], ShouldEqual, "m1")
				So(m0["label"], ShouldEqual, "deploy a")
				So(m0["at"], ShouldEqual, at.Format(time.RFC3339))
				So(time.Since(st.since), ShouldBeBetween, 23*time.Hour, 25*time.Hour)
			})
		})

		Convey("Then the 30d window widens the since bound", func() {
			rr, _ := getSeriesJSON(t, h, "/api/markers?window=30d")
			So(rr.Code, ShouldEqual, http.StatusOK)
			So(time.Since(st.since), ShouldBeBetween, 29*24*time.Hour, 31*24*time.Hour)
		})

		Convey("Then an unknown window is a 400", func() {
			rr, _ := getSeriesJSON(t, h, "/api/markers?window=1y")
			So(rr.Code, ShouldEqual, http.StatusBadRequest)
		})
	})
}
