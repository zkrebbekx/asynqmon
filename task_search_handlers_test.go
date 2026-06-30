package asynqmon

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestTaskMatchesSearch(t *testing.T) {
	Convey("Given a task and a free-text query", t, func() {
		task := &searchTask{
			ID:      "abc-123",
			Queue:   "critical",
			Type:    "email:welcome",
			Payload: `{"user_id":1002,"email":"u@example.com"}`,
		}

		Convey("An empty query matches everything", func() {
			So(taskMatchesSearch(task, ""), ShouldBeTrue)
		})
		Convey("It matches case-insensitively across id/type/queue/payload", func() {
			So(taskMatchesSearch(task, "WELCOME"), ShouldBeTrue)   // type
			So(taskMatchesSearch(task, "critical"), ShouldBeTrue)  // queue
			So(taskMatchesSearch(task, "abc-1"), ShouldBeTrue)     // id
			So(taskMatchesSearch(task, "example.com"), ShouldBeTrue) // payload
		})
		Convey("It fails when the substring is absent", func() {
			So(taskMatchesSearch(task, "payment"), ShouldBeFalse)
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

func TestParseMetaFilters(t *testing.T) {
	Convey("parseMetaFilters splits key:value on the first colon", t, func() {
		got := parseMetaFilters([]string{"region:eu", "url:http://x:8080", "bad", ":novalue"})
		So(got, ShouldResemble, []metaFilter{
			{Key: "region", Value: "eu"},
			{Key: "url", Value: "http://x:8080"},
		})
	})
}
