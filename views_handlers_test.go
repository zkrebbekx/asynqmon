package asynqmon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// Handler tests for the saved-view endpoints (§4.2, phase 11) against a real
// Redis on 127.0.0.1:6379 DB 13 — the same DB and conventions as the other
// root fleet handler tests (DB ownership: 10 = schedulers, 11 = coverage,
// 12 = jobs, 13 = fleet handlers, 14 = stats package). Skipped when Redis
// does not answer PING.
// ****************************************************************************

const (
	viewsTestRedisAddr = "127.0.0.1:6379"
	viewsTestRedisDB   = 13
)

// newViewsTestStore flushes DB 13 and returns the production store, the jobs
// store backing the audit stream, and a reset func. goconvey re-executes the
// whole tree path per leaf, so mutating tests call reset at the top of the
// root Convey body to isolate paths from each other.
func newViewsTestStore(t *testing.T) (redisViewStore, *jobs.Store, func()) {
	t.Helper()
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: viewsTestRedisAddr, DB: viewsTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", viewsTestRedisAddr, err)
	}
	reset := func() {
		if err := rc.FlushDB(ctx).Err(); err != nil {
			t.Fatalf("flushing test db: %v", err)
		}
	}
	reset()
	t.Cleanup(func() { rc.Close() })
	return newRedisViewStore(rc), jobs.NewStore(rc), reset
}

// withActor injects the §5.11 resolved actor the way the middleware would.
func withActor(r *http.Request, name string) *http.Request {
	ctx := context.WithValue(r.Context(), actorCtxKey{}, actorInfo{Name: name, Attributed: true})
	return r.WithContext(ctx)
}

// viewsRouter builds a router with the four view routes so mux.Vars works.
func viewsRouter(store viewStore, audit *jobs.Store) *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/api/views", newListViewsHandlerFunc(store)).Methods("GET")
	r.HandleFunc("/api/views", newCreateViewHandlerFunc(store, audit)).Methods("POST")
	r.HandleFunc("/api/views/{view_id}", newUpdateViewHandlerFunc(store, audit)).Methods("PUT")
	r.HandleFunc("/api/views/{view_id}", newDeleteViewHandlerFunc(store, audit)).Methods("DELETE")
	return r
}

func doViewReq(router http.Handler, method, url, actor string, body interface{}) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, url, &buf)
	if actor != "" {
		req = withActor(req, actor)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func decodeView(t *testing.T, w *httptest.ResponseRecorder) viewJSON {
	t.Helper()
	var v viewJSON
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding view: %v\nbody: %s", err, w.Body.String())
	}
	return v
}

func decodeViewList(t *testing.T, w *httptest.ResponseRecorder) []viewJSON {
	t.Helper()
	var resp listViewsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding view list: %v\nbody: %s", err, w.Body.String())
	}
	return resp.Views
}

func TestViewsCRUD(t *testing.T) {
	store, audit, reset := newViewsTestStore(t)
	router := viewsRouter(store, audit)

	Convey("Given an empty view store", t, func() {
		reset() // per-leaf-path isolation (goconvey re-runs the tree per leaf)

		Convey("When a view is created with an attributed actor", func() {
			w := doViewReq(router, "POST", "/api/views", "mira@ops",
				map[string]interface{}{
					"name":   "gw-timeout blast",
					"target": "tasks",
					"state":  map[string]string{"q": `state=retry error~"gateway timeout"`},
				})

			Convey("Then it answers 201 with the actor-attributed view", func() {
				So(w.Code, ShouldEqual, http.StatusCreated)
				v := decodeView(t, w)
				So(v.ID, ShouldStartWith, "vw_")
				So(v.Name, ShouldEqual, "gw-timeout blast")
				So(v.Target, ShouldEqual, "tasks")
				So(v.CreatedBy, ShouldEqual, "mira@ops")
				So(v.System, ShouldBeFalse)

				Convey("And the state JSON round-trips verbatim", func() {
					var state map[string]string
					So(json.Unmarshal(v.State, &state), ShouldBeNil)
					So(state["q"], ShouldEqual, `state=retry error~"gateway timeout"`)
				})

				Convey("And GET /api/views lists it", func() {
					lw := doViewReq(router, "GET", "/api/views", "", nil)
					So(lw.Code, ShouldEqual, http.StatusOK)
					views := decodeViewList(t, lw)
					So(len(views), ShouldEqual, 1)
					So(views[0].ID, ShouldEqual, v.ID)
				})

				Convey("And the mutation is audit-logged with the actor", func() {
					entries, err := audit.ReadAudit(context.Background(), 10)
					So(err, ShouldBeNil)
					So(len(entries), ShouldEqual, 1)
					So(entries[0].Event, ShouldEqual, "view_created")
					So(entries[0].Actor, ShouldEqual, "mira@ops")
					So(entries[0].Verb, ShouldEqual, "view")
					So(entries[0].JobID, ShouldEqual, v.ID)
				})

				Convey("When the view is renamed and re-stated via PUT", func() {
					uw := doViewReq(router, "PUT", "/api/views/"+v.ID, "kai@ops",
						map[string]interface{}{
							"name":  "gw-timeout blast (all states)",
							"state": map[string]string{"q": `error~"gateway timeout"`},
						})

					Convey("Then it answers 200 and preserves creator attribution", func() {
						So(uw.Code, ShouldEqual, http.StatusOK)
						u := decodeView(t, uw)
						So(u.Name, ShouldEqual, "gw-timeout blast (all states)")
						So(u.CreatedBy, ShouldEqual, "mira@ops") // creator, not editor
						So(u.CreatedAt, ShouldEqual, v.CreatedAt)
						var state map[string]string
						So(json.Unmarshal(u.State, &state), ShouldBeNil)
						So(state["q"], ShouldEqual, `error~"gateway timeout"`)
					})

					Convey("And the update is audit-logged", func() {
						entries, err := audit.ReadAudit(context.Background(), 10)
						So(err, ShouldBeNil)
						So(entries[0].Event, ShouldEqual, "view_updated")
						So(entries[0].Actor, ShouldEqual, "kai@ops")
					})
				})

				Convey("When the view is deleted", func() {
					dw := doViewReq(router, "DELETE", "/api/views/"+v.ID, "mira@ops", nil)

					Convey("Then it answers 204 and the view is gone", func() {
						So(dw.Code, ShouldEqual, http.StatusNoContent)
						lw := doViewReq(router, "GET", "/api/views", "", nil)
						So(len(decodeViewList(t, lw)), ShouldEqual, 0)
					})

					Convey("And deleting it again answers 404", func() {
						dw2 := doViewReq(router, "DELETE", "/api/views/"+v.ID, "mira@ops", nil)
						So(dw2.Code, ShouldEqual, http.StatusNotFound)
					})
				})
			})
		})

		Convey("When a view is created without the actor middleware", func() {
			w := doViewReq(router, "POST", "/api/views", "",
				map[string]interface{}{
					"name":   "anon view",
					"target": "queues",
					"state":  map[string]string{"f": "pending>0"},
				})

			Convey("Then it stays honest as unattributed rather than inventing an identity", func() {
				So(w.Code, ShouldEqual, http.StatusCreated)
				So(decodeView(t, w).CreatedBy, ShouldEqual, "unattributed")
			})
		})

		Convey("When invalid requests arrive", func() {
			Convey("Then a missing name answers 400", func() {
				w := doViewReq(router, "POST", "/api/views", "mira@ops",
					map[string]interface{}{"name": "  ", "target": "tasks", "state": map[string]string{}})
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then an unknown target answers 400", func() {
				w := doViewReq(router, "POST", "/api/views", "mira@ops",
					map[string]interface{}{"name": "x", "target": "workers", "state": map[string]string{}})
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then a non-object state answers 400", func() {
				w := doViewReq(router, "POST", "/api/views", "mira@ops",
					map[string]interface{}{"name": "x", "target": "tasks", "state": "q=foo"})
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then updating an unknown view answers 404", func() {
				w := doViewReq(router, "PUT", "/api/views/vw_nope", "mira@ops",
					map[string]interface{}{"name": "y"})
				So(w.Code, ShouldEqual, http.StatusNotFound)
			})
		})
	})
}

func TestSystemViewSeeding(t *testing.T) {
	store, audit, reset := newViewsTestStore(t)
	router := viewsRouter(store, audit)
	ctx := context.Background()

	Convey("Given system views seeded at startup", t, func() {
		reset() // per-leaf-path isolation
		So(seedSystemViews(ctx, store), ShouldBeNil)

		Convey("When GET /api/views is served", func() {
			w := doViewReq(router, "GET", "/api/views", "", nil)
			views := decodeViewList(t, w)

			Convey("Then the five shipped views are present, system-flagged, in shipped order", func() {
				So(len(views), ShouldEqual, 5)
				ids := make([]string, 0, 5)
				for _, v := range views {
					So(v.System, ShouldBeTrue)
					So(v.CreatedBy, ShouldEqual, "system")
					ids = append(ids, v.ID)
				}
				So(ids, ShouldResemble, []string{
					"sys_exhausted_retries",
					"sys_orphaned_actives",
					"sys_past_due_scheduled",
					"sys_zero_consumer_queues",
					"sys_paused_with_producers",
				})
			})

			Convey("Then the shipped states carry the contract queries", func() {
				byID := map[string]viewJSON{}
				for _, v := range views {
					byID[v.ID] = v
				}
				var st map[string]string
				So(json.Unmarshal(byID["sys_exhausted_retries"].State, &st), ShouldBeNil)
				So(st["q"], ShouldEqual, "state=retry retries>=5")
				So(byID["sys_exhausted_retries"].Target, ShouldEqual, "tasks")
				So(json.Unmarshal(byID["sys_zero_consumer_queues"].State, &st), ShouldBeNil)
				So(st["f"], ShouldEqual, "consumers=0 pending>0")
				So(byID["sys_zero_consumer_queues"].Target, ShouldEqual, "queues")
				So(json.Unmarshal(byID["sys_paused_with_producers"].State, &st), ShouldBeNil)
				So(st["f"], ShouldEqual, "paused=true pending>0")
			})
		})

		Convey("When seeding runs again (restart)", func() {
			before, _, err := store.Get(ctx, "sys_orphaned_actives")
			So(err, ShouldBeNil)
			time.Sleep(5 * time.Millisecond) // a re-seed "now" would differ
			So(seedSystemViews(ctx, store), ShouldBeNil)

			Convey("Then it is idempotent — still five views, created_at preserved", func() {
				views := decodeViewList(t, doViewReq(router, "GET", "/api/views", "", nil))
				So(len(views), ShouldEqual, 5)
				after, _, err := store.Get(ctx, "sys_orphaned_actives")
				So(err, ShouldBeNil)
				So(after.CreatedAt.UnixMilli(), ShouldEqual, before.CreatedAt.UnixMilli())
			})
		})

		Convey("When a hand-edited system view is re-seeded", func() {
			v, _, err := store.Get(ctx, "sys_past_due_scheduled")
			So(err, ShouldBeNil)
			v.State = json.RawMessage(`{"q":"state=pending"}`)
			So(store.Put(ctx, v), ShouldBeNil)
			So(seedSystemViews(ctx, store), ShouldBeNil)

			Convey("Then the shipped definition is restored (self-healing)", func() {
				healed, _, err := store.Get(ctx, "sys_past_due_scheduled")
				So(err, ShouldBeNil)
				So(string(healed.State), ShouldEqual, `{"q":"state=scheduled past_due"}`)
			})
		})

		Convey("When a system view is deleted", func() {
			w := doViewReq(router, "DELETE", "/api/views/sys_exhausted_retries", "mira@ops", nil)

			Convey("Then it answers 400 with the reason and the view survives", func() {
				So(w.Code, ShouldEqual, http.StatusBadRequest)
				var body map[string]string
				So(json.Unmarshal(w.Body.Bytes(), &body), ShouldBeNil)
				So(body["error"], ShouldContainSubstring, "undeletable")
				So(body["error"], ShouldContainSubstring, "Exhausted retries")
				_, ok, err := store.Get(ctx, "sys_exhausted_retries")
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When a system view is modified via PUT", func() {
			w := doViewReq(router, "PUT", "/api/views/sys_orphaned_actives", "mira@ops",
				map[string]interface{}{"name": "mine now"})

			Convey("Then it answers 400 with the reason", func() {
				So(w.Code, ShouldEqual, http.StatusBadRequest)
				var body map[string]string
				So(json.Unmarshal(w.Body.Bytes(), &body), ShouldBeNil)
				So(body["error"], ShouldContainSubstring, "system views cannot be modified")
			})
		})

		Convey("When user views exist beside system views", func() {
			doViewReq(router, "POST", "/api/views", "mira@ops",
				map[string]interface{}{"name": "older", "target": "tasks", "state": map[string]string{"q": "state=retry"}})
			time.Sleep(5 * time.Millisecond)
			doViewReq(router, "POST", "/api/views", "mira@ops",
				map[string]interface{}{"name": "newer", "target": "tasks", "state": map[string]string{"q": "state=archived"}})

			Convey("Then listing orders system views first, then user views newest-first", func() {
				views := decodeViewList(t, doViewReq(router, "GET", "/api/views", "", nil))
				So(len(views), ShouldEqual, 7)
				So(views[0].System, ShouldBeTrue)
				So(views[4].System, ShouldBeTrue)
				So(views[5].Name, ShouldEqual, "newer")
				So(views[6].Name, ShouldEqual, "older")
			})
		})
	})
}

func TestViewsReadOnlyMode(t *testing.T) {
	store, audit, _ := newViewsTestStore(t)

	Convey("Given the view routes behind the read-only method filter", t, func() {
		roRouter := restrictToReadOnly(viewsRouter(store, audit))

		Convey("When mutations arrive in read-only mode", func() {
			post := doViewReqRO(roRouter, "POST", "/api/views",
				map[string]interface{}{"name": "x", "target": "tasks", "state": map[string]string{}})
			put := doViewReqRO(roRouter, "PUT", "/api/views/sys_exhausted_retries",
				map[string]interface{}{"name": "x"})
			del := doViewReqRO(roRouter, "DELETE", "/api/views/vw_x", nil)

			Convey("Then all are refused with 405", func() {
				So(post.Code, ShouldEqual, http.StatusMethodNotAllowed)
				So(put.Code, ShouldEqual, http.StatusMethodNotAllowed)
				So(del.Code, ShouldEqual, http.StatusMethodNotAllowed)
			})
		})

		Convey("When GET arrives in read-only mode", func() {
			get := doViewReqRO(roRouter, "GET", "/api/views", nil)

			Convey("Then it is still served", func() {
				So(get.Code, ShouldEqual, http.StatusOK)
			})
		})
	})
}

// doViewReqRO is doJSON for a bare http.Handler (the read-only wrapper).
func doViewReqRO(h http.Handler, method, url string, body interface{}) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, url, &buf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
