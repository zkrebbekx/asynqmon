package asynqmon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestTaskMatchesSearch(t *testing.T) {
	Convey("Given a task and a free-text query", t, func() {
		task := &searchTask{
			ID:         "abc-123",
			Queue:      "critical",
			Type:       "email:welcome",
			Payload:    `{"user_id":1002,"email":"u@example.com"}`,
			rawPayload: `{"user_id":1002,"email":"u@example.com"}`,
		}

		Convey("An empty query matches everything", func() {
			So(taskMatchesSearch(task, ""), ShouldBeTrue)
		})
		Convey("It matches case-insensitively across id/type/queue/payload", func() {
			So(taskMatchesSearch(task, "WELCOME"), ShouldBeTrue)     // type
			So(taskMatchesSearch(task, "critical"), ShouldBeTrue)    // queue
			So(taskMatchesSearch(task, "abc-1"), ShouldBeTrue)       // id
			So(taskMatchesSearch(task, "example.com"), ShouldBeTrue) // payload
		})
		Convey("It fails when the substring is absent", func() {
			So(taskMatchesSearch(task, "payment"), ShouldBeFalse)
		})
		Convey("It matches on the full raw payload even when the display payload is truncated", func() {
			truncated := &searchTask{
				Payload:    `{"user_id":1002,"note":"aaaaaaaaaa…`,
				rawPayload: `{"user_id":1002,"note":"aaaaaaaaaa","needle":"deep-value"}`,
			}
			So(taskMatchesSearch(truncated, "deep-value"), ShouldBeTrue)
		})
	})
}

func TestTaskMatchesMeta(t *testing.T) {
	Convey("Given a JSON payload", t, func() {
		payload := `{"user_id":1002,"region":"eu","active":true}`

		Convey("No filters match anything", func() {
			So(taskMatchesMeta(payload, nil), ShouldBeTrue)
		})
		Convey("All filters must be present (AND)", func() {
			So(taskMatchesMeta(payload, []metaFilter{{"region", "eu"}}), ShouldBeTrue)
			So(taskMatchesMeta(payload, []metaFilter{{"user_id", "1002"}, {"active", "true"}}), ShouldBeTrue)
		})
		Convey("A single mismatch fails", func() {
			So(taskMatchesMeta(payload, []metaFilter{{"region", "us"}}), ShouldBeFalse)
			So(taskMatchesMeta(payload, []metaFilter{{"region", "eu"}, {"user_id", "9999"}}), ShouldBeFalse)
		})
		Convey("Non-JSON payloads only match an empty filter set", func() {
			So(taskMatchesMeta("plain text", nil), ShouldBeTrue)
			So(taskMatchesMeta("plain text", []metaFilter{{"a", "b"}}), ShouldBeFalse)
		})
	})
}

func TestTaskMatchesMetaNumericCoercion(t *testing.T) {
	Convey("Given a payload with numeric values", t, func() {
		payload := `{"x":10,"y":1000,"f":2.5,"big":12345678901234567890}`

		Convey("An integer value matches every spelling of the same number", func() {
			So(taskMatchesMeta(payload, []metaFilter{{"x", "10"}}), ShouldBeTrue)
			So(taskMatchesMeta(payload, []metaFilter{{"x", "10.0"}}), ShouldBeTrue)
			So(taskMatchesMeta(payload, []metaFilter{{"x", "1e1"}}), ShouldBeTrue)
			So(taskMatchesMeta(payload, []metaFilter{{"y", "1e3"}}), ShouldBeTrue)
		})
		Convey("A fractional value still compares numerically", func() {
			So(taskMatchesMeta(payload, []metaFilter{{"f", "2.5"}}), ShouldBeTrue)
			So(taskMatchesMeta(payload, []metaFilter{{"f", "2.50"}}), ShouldBeTrue)
		})
		Convey("A different number does not match", func() {
			So(taskMatchesMeta(payload, []metaFilter{{"x", "11"}}), ShouldBeFalse)
			So(taskMatchesMeta(payload, []metaFilter{{"x", "eu"}}), ShouldBeFalse)
		})
		Convey("An integer beyond 2^53 cannot match exactly: the JSON decode is float64", func() {
			// 12345678901234567890 and 12345678901234567891 share one
			// float64, so the comparison cannot tell them apart.
			So(taskMatchesMeta(payload, []metaFilter{{"big", "12345678901234567891"}}), ShouldBeTrue)
			// Only a difference wider than one float64 step (about 2048 at
			// this magnitude) is still visible.
			So(taskMatchesMeta(payload, []metaFilter{{"big", "12345678901234500000"}}), ShouldBeFalse)
		})
	})
}

func TestClampedPageSize(t *testing.T) {
	Convey("Given a legacy list request", t, func() {
		req := func(query string) *http.Request {
			return httptest.NewRequest("GET", "/api/queues/q/pending_tasks"+query, nil)
		}
		Convey("No size parameter means no clamp to report", func() {
			So(clampedPageSize(req(""), defaultPageSize), ShouldEqual, 0)
		})
		Convey("A clamped size reports the size actually applied", func() {
			So(clampedPageSize(req("?size=0"), defaultPageSize), ShouldEqual, defaultPageSize)
			So(clampedPageSize(req("?size=5000"), maxPageSize), ShouldEqual, maxPageSize)
		})
		Convey("An honored size reports nothing", func() {
			So(clampedPageSize(req("?size=10"), 10), ShouldEqual, 0)
		})
	})
}

func TestScalarString(t *testing.T) {
	Convey("scalarString renders JSON scalars like the frontend chips", t, func() {
		So(scalarString("hello"), ShouldEqual, "hello")
		So(scalarString(true), ShouldEqual, "true")
		So(scalarString(float64(1002)), ShouldEqual, "1002") // integer, no .0
		So(scalarString(float64(3.5)), ShouldEqual, "3.5")
		So(scalarString(nil), ShouldEqual, "")
		So(scalarString(map[string]interface{}{"x": 1}), ShouldEqual, "") // nested -> empty
	})
}

// foldFacets and foldAggregate drive the streaming aggregators the handlers
// use, so these pure tests exercise exactly the production folding code.
func foldFacets(matches []*searchTask, limit int) []metaFacet {
	a := newFacetAgg()
	for _, t := range matches {
		a.add(t)
	}
	return a.result(limit)
}

func foldAggregate(matches []*searchTask, by string, limit int) []aggregateGroup {
	a := newAggregateAgg(by)
	for _, t := range matches {
		a.add(t)
	}
	return a.result(limit)
}

func TestCollectFacets(t *testing.T) {
	Convey("Given a set of matched tasks", t, func() {
		matches := []*searchTask{
			{rawPayload: `{"region":"eu","tier":"gold"}`},
			{rawPayload: `{"region":"eu","tier":"silver"}`},
			{rawPayload: `{"region":"us","nested":{"x":1},"list":[1,2]}`},
			{rawPayload: `not json`},
		}

		Convey("The facet aggregator folds distinct key=value with counts, most frequent first", func() {
			facets := foldFacets(matches, 50)
			So(facets[0], ShouldResemble, metaFacet{Key: "region", Value: "eu", Count: 2})
			So(facets, ShouldContain, metaFacet{Key: "tier", Value: "gold", Count: 1})
			So(facets, ShouldContain, metaFacet{Key: "region", Value: "us", Count: 1})
		})

		Convey("It skips nested objects/arrays and non-JSON payloads", func() {
			facets := foldFacets(matches, 50)
			for _, f := range facets {
				So(f.Key, ShouldNotEqual, "nested")
				So(f.Key, ShouldNotEqual, "list")
			}
		})

		Convey("It respects the limit", func() {
			So(foldFacets(matches, 1), ShouldHaveLength, 1)
		})
	})
}

func TestAggregateBy(t *testing.T) {
	Convey("Given matched tasks", t, func() {
		matches := []*searchTask{
			{Type: "email:welcome", LastError: "timeout", Queue: "default"},
			{Type: "email:welcome", LastError: "timeout", Queue: "critical"},
			{Type: "image:resize", LastError: "boom", Queue: "default"},
			{Type: "image:resize", LastError: "", Queue: "default"},
		}

		Convey("by=type groups and ranks by count", func() {
			g := foldAggregate(matches, "type", 50)
			So(g[0], ShouldResemble, aggregateGroup{Label: "email:welcome", Count: 2})
			So(g, ShouldContain, aggregateGroup{Label: "image:resize", Count: 2})
		})
		Convey("by=error groups by error message and skips empty errors", func() {
			g := foldAggregate(matches, "error", 50)
			So(g[0], ShouldResemble, aggregateGroup{Label: "timeout", Count: 2})
			So(g, ShouldContain, aggregateGroup{Label: "boom", Count: 1})
			for _, x := range g {
				So(x.Label, ShouldNotEqual, "")
			}
		})
		Convey("by=queue groups by queue", func() {
			g := foldAggregate(matches, "queue", 50)
			So(g[0], ShouldResemble, aggregateGroup{Label: "default", Count: 3})
		})
		Convey("respects the limit", func() {
			So(foldAggregate(matches, "type", 1), ShouldHaveLength, 1)
		})
	})
}

func TestParseMetaFilters(t *testing.T) {
	Convey("parseMetaFilters splits key:value on the first colon", t, func() {
		got := parseMetaFilters([]string{"region:eu", "url:http://x:8080", "bad", ":novalue"})
		So(got, ShouldResemble, []metaFilter{
			{Key: "region", Value: "eu"},
			{Key: "url", Value: "http://x:8080"},
		})
	})
}

func TestPageBounds(t *testing.T) {
	Convey("Given a result window", t, func() {
		Convey("Normal pages slice as expected", func() {
			s, e := pageBounds(50, 1, 20)
			So(s, ShouldEqual, 0)
			So(e, ShouldEqual, 20)
			s, e = pageBounds(50, 3, 20)
			So(s, ShouldEqual, 40)
			So(e, ShouldEqual, 50)
		})
		Convey("Pages past the end clamp to an empty tail window", func() {
			s, e := pageBounds(50, 4, 20)
			So(s, ShouldEqual, 50)
			So(e, ShouldEqual, 50)
		})
		Convey("Hostile page values cannot overflow into a negative start", func() {
			// (page-1)*size would wrap negative with naive arithmetic and
			// panic the slice expression (issue P1-1 in docs/review).
			s, e := pageBounds(50, 4611686018427387904, 20)
			So(s, ShouldEqual, 50)
			So(e, ShouldEqual, 50)
			s, e = pageBounds(0, 1<<62, 1<<62)
			So(s, ShouldEqual, 0)
			So(e, ShouldEqual, 0)
		})
		Convey("Degenerate inputs stay in range", func() {
			s, e := pageBounds(5, 0, 20)
			So(s, ShouldEqual, 0)
			So(e, ShouldEqual, 5)
			s, e = pageBounds(5, 1, 0)
			So(s, ShouldEqual, 5)
			So(e, ShouldEqual, 5)
		})
	})
}

func TestCollectFacetsHighCardinalityGuard(t *testing.T) {
	Convey("Given payloads with one low- and one high-cardinality key", t, func() {
		var matches []*searchTask
		for i := 0; i < 40; i++ {
			matches = append(matches, &searchTask{
				rawPayload: fmt.Sprintf(`{"collection":"articles","doc_id":"doc_%06d"}`, i),
			})
		}
		facets := foldFacets(matches, 50)

		Convey("The repeated value keeps its chip with the full count", func() {
			So(len(facets), ShouldBeGreaterThan, 0)
			So(facets[0].Key, ShouldEqual, "collection")
			So(facets[0].Count, ShouldEqual, 40)
		})
		Convey("The unique-per-task key produces no count-1 chip flood", func() {
			for _, f := range facets {
				So(f.Key, ShouldNotEqual, "doc_id")
			}
		})
	})
}
