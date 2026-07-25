package asynqmon

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Integration tests for the full-payload-on-task-detail behavior (upstream
// hibiken/asynqmon#301) against a real Redis on 127.0.0.1:6379 DB 13 — the
// same DB and conventions as the other root handler suites (stats, views,
// fleet events, health); only DB 13 is flushed here. Skipped when Redis does
// not answer PING.
//
// The seam under test: Options.DetailPayloadFormatter/DetailResultFormatter
// route GET /api/queues/{qname}/tasks/{task_id} through their own formatter
// pair while every LIST endpoint keeps the (truncating) PayloadFormatter —
// mirroring cmd/asynqmon's --max-payload-length vs --max-detail-payload-length
// wiring, which this test reproduces with the same truncate semantics.
//
// Convey discipline: all queue mutations and HTTP round-trips run
// imperatively BEFORE the Convey tree; the tree only reads captured results.
// ****************************************************************************

const (
	detailTestRedisAddr = "127.0.0.1:6379"
	detailTestRedisDB   = 13

	// The list-cell cap and the detail safety cap used by this suite,
	// standing in for --max-payload-length / --max-detail-payload-length.
	detailTestListCap   = 200
	detailTestDetailCap = 10000
)

// truncateRunes mirrors cmd/asynqmon's truncate: cut at limit utf8 runes and
// append an ellipsis. Kept in the test so the round-trip asserts the exact
// production semantics (a truncated string is limit+1 runes ending in "…").
func truncateRunes(s string, limit int) string {
	i := 0
	for pos := range s {
		if i == limit {
			return s[:pos] + "…"
		}
		i++
	}
	return s
}

// detailTestFormatters builds the cmd-equivalent formatter pairs.
func detailTestListFormatter() PayloadFormatter {
	return PayloadFormatterFunc(func(taskType string, payload []byte) string {
		return truncateRunes(SmartPayloadFormatter.FormatPayload(taskType, payload), detailTestListCap)
	})
}

func detailTestDetailFormatter(limit int) PayloadFormatter {
	return PayloadFormatterFunc(func(taskType string, payload []byte) string {
		s := SmartPayloadFormatter.FormatPayload(taskType, payload)
		if limit <= 0 {
			return s
		}
		return truncateRunes(s, limit)
	})
}

func TestTaskDetailFullPayload(t *testing.T) {
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: detailTestRedisAddr, DB: detailTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", detailTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db %d: %v", detailTestRedisDB, err)
	}
	opt := asynq.RedisClientOpt{Addr: detailTestRedisAddr, DB: detailTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
		rc.Close()
	})

	// Two pending tasks in queue "detail301":
	//   big   — 5,000-char JSON payload: over the list cap, under the detail cap
	//   huge  — 12,000-char JSON payload: over BOTH caps
	bigPayload := `{"blob":"` + strings.Repeat("a", 4989) + `"}` // 5,000 chars exactly
	hugePayload := `{"blob":"` + strings.Repeat("b", 11989) + `"}`
	bigInfo, err := client.Enqueue(asynq.NewTask("detail:big", []byte(bigPayload)), asynq.Queue("detail301"))
	if err != nil {
		t.Fatalf("enqueue big: %v", err)
	}
	hugeInfo, err := client.Enqueue(asynq.NewTask("detail:huge", []byte(hugePayload)), asynq.Queue("detail301"))
	if err != nil {
		t.Fatalf("enqueue huge: %v", err)
	}

	// Router A: cmd-equivalent wiring — truncating list formatter + capped
	// detail formatter (#301). Router B: legacy wiring with NO detail
	// formatters, asserting the nil fallback keeps old behavior.
	routerDetail := muxRouter(Options{
		PayloadFormatter: detailTestListFormatter(),
		ResultFormatter: ResultFormatterFunc(func(tt string, b []byte) string {
			return truncateRunes(SmartResultFormatter.FormatResult(tt, b), detailTestListCap)
		}),
		DetailPayloadFormatter: detailTestDetailFormatter(detailTestDetailCap),
		DetailResultFormatter: ResultFormatterFunc(func(tt string, b []byte) string {
			return truncateRunes(SmartResultFormatter.FormatResult(tt, b), detailTestDetailCap)
		}),
		DetailPayloadLimit: detailTestDetailCap,
	}, rc, insp, nil, nil)
	routerLegacy := muxRouter(Options{
		PayloadFormatter: detailTestListFormatter(),
	}, rc, insp, nil, nil)

	// List round-trip (row cells).
	type listResp struct {
		Tasks []struct {
			ID      string `json:"id"`
			Payload string `json:"payload"`
		} `json:"tasks"`
	}
	var list listResp
	wList := doJSON(t, routerDetail, "GET", "/api/queues/detail301/pending_tasks?size=10", nil, &list)
	listPayloadByID := map[string]string{}
	for _, tk := range list.Tasks {
		listPayloadByID[tk.ID] = tk.Payload
	}

	// Detail round-trips.
	var bigDetail, hugeDetail, legacyDetail taskInfo
	wBig := doJSON(t, routerDetail, "GET", "/api/queues/detail301/tasks/"+bigInfo.ID, nil, &bigDetail)
	wHuge := doJSON(t, routerDetail, "GET", "/api/queues/detail301/tasks/"+hugeInfo.ID, nil, &hugeDetail)
	wLegacy := doJSON(t, routerLegacy, "GET", "/api/queues/detail301/tasks/"+bigInfo.ID, nil, &legacyDetail)

	// Capability surface: /api/features carries the detail cap for the UI's
	// truncation note.
	var features featuresResponse
	doJSON(t, routerDetail, "GET", "/api/features", nil, &features)
	var legacyFeatures featuresResponse
	doJSON(t, routerLegacy, "GET", "/api/features", nil, &legacyFeatures)

	Convey("Given pending tasks with payloads over the list cap (#301)", t, func() {
		Convey("When the LIST endpoint serves the row cells", func() {
			Convey("Then every payload cell stays truncated at the list cap", func() {
				So(wList.Code, ShouldEqual, http.StatusOK)
				So(list.Tasks, ShouldHaveLength, 2)
				for id, p := range listPayloadByID {
					So(utf8.RuneCountInString(p), ShouldEqual, detailTestListCap+1) // cap + ellipsis
					So(strings.HasSuffix(p, "…"), ShouldBeTrue)
					So(id, ShouldNotBeEmpty)
				}
			})
		})

		Convey("When the DETAIL endpoint serves a payload under the detail cap", func() {
			Convey("Then the payload comes back whole — byte-for-byte", func() {
				So(wBig.Code, ShouldEqual, http.StatusOK)
				So(bigDetail.Payload, ShouldEqual, bigPayload)
				So(utf8.RuneCountInString(bigDetail.Payload), ShouldEqual, 5000)
			})
		})

		Convey("When the DETAIL endpoint serves a payload over the detail cap", func() {
			Convey("Then the safety cap is respected: detail-cap runes + ellipsis", func() {
				So(wHuge.Code, ShouldEqual, http.StatusOK)
				So(utf8.RuneCountInString(hugeDetail.Payload), ShouldEqual, detailTestDetailCap+1)
				So(strings.HasSuffix(hugeDetail.Payload, "…"), ShouldBeTrue)
				So(strings.HasPrefix(hugeDetail.Payload, `{"blob":"bbb`), ShouldBeTrue)
			})
		})

		Convey("When no detail formatters are configured (embedder back-compat)", func() {
			Convey("Then the detail endpoint falls back to the list formatter", func() {
				So(wLegacy.Code, ShouldEqual, http.StatusOK)
				So(utf8.RuneCountInString(legacyDetail.Payload), ShouldEqual, detailTestListCap+1)
				So(strings.HasSuffix(legacyDetail.Payload, "…"), ShouldBeTrue)
			})
		})

		Convey("When the frontend probes GET /api/features", func() {
			Convey("Then payload_detail_limit carries the detail cap (0 when unset)", func() {
				So(features.PayloadDetailLimit, ShouldEqual, detailTestDetailCap)
				So(legacyFeatures.PayloadDetailLimit, ShouldEqual, 0)
			})
		})
	})
}
