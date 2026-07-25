package asynqmon

import (
	"testing"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Unit tests for the coverage join (coverage.go): pure functions, no Redis.
// Integration coverage against real asynq servers is in
// coverage_handlers_test.go (DB 11).
// ****************************************************************************

func srv(id, host string, pid, concurrency int, strict bool, queues map[string]int) *asynq.ServerInfo {
	return &asynq.ServerInfo{
		ID:             id,
		Host:           host,
		PID:            pid,
		Concurrency:    concurrency,
		Queues:         queues,
		StrictPriority: strict,
	}
}

func rowByQueue(rows []*coverageRow, q string) *coverageRow {
	for _, r := range rows {
		if r.Queue == q {
			return r
		}
	}
	return nil
}

func TestBuildCoverageRowsUncovered(t *testing.T) {
	Convey("Given a queue with pending work and no live consumer", t, func() {
		queues := []string{"orphaned", "covered"}
		servers := []*asynq.ServerInfo{
			srv("s1", "wrk-01", 100, 4, false, map[string]int{"covered": 1}),
		}

		Convey("When depth data is available from the stats cache", func() {
			depths := map[string]coverageDepth{
				"orphaned": {Pending: 42},
				"covered":  {Pending: 7},
			}
			rows := buildCoverageRows(servers, queues, depths)

			Convey("Then the consumer-less queue is flagged uncovered with its pending depth", func() {
				r := rowByQueue(rows, "orphaned")
				So(r, ShouldNotBeNil)
				So(r.Status, ShouldEqual, coverageStatusUncovered)
				So(r.Consumers, ShouldBeEmpty)
				So(r.Pending, ShouldNotBeNil)
				So(*r.Pending, ShouldEqual, 42)
			})
			Convey("Then the consumed queue is ok", func() {
				So(rowByQueue(rows, "covered").Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When the queue has consumers but zero pending", func() {
			depths := map[string]coverageDepth{"orphaned": {Pending: 0}, "covered": {Pending: 0}}
			rows := buildCoverageRows(servers, queues, depths)

			Convey("Then a consumer-less empty queue is not uncovered (§3.7 requires pending > 0)", func() {
				So(rowByQueue(rows, "orphaned").Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When the stats cache is unavailable (nil depths)", func() {
			rows := buildCoverageRows(servers, queues, nil)

			Convey("Then pending is omitted and no uncovered claim is made", func() {
				r := rowByQueue(rows, "orphaned")
				So(r.Pending, ShouldBeNil)
				So(r.Status, ShouldEqual, coverageStatusOK)
			})
		})
	})
}

func TestBuildCoverageRowsStarved(t *testing.T) {
	// The canonical starved setup: one strict server, "low" at the minimum
	// weight, higher-priority "high" non-empty.
	strictServer := srv("s1", "wrk-01", 100, 8, true, map[string]int{"high": 3, "low": 1})
	queues := []string{"high", "low"}

	Convey("Given a strict-priority server with a busy higher-priority queue", t, func() {
		depths := map[string]coverageDepth{"high": {Pending: 500}, "low": {Pending: 3}}

		Convey("When the join runs", func() {
			rows := buildCoverageRows([]*asynq.ServerInfo{strictServer}, queues, depths)

			Convey("Then the lowest-weight queue is starved", func() {
				So(rowByQueue(rows, "low").Status, ShouldEqual, coverageStatusStarved)
			})
			Convey("Then the top-weight queue is not", func() {
				So(rowByQueue(rows, "high").Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When the higher class has only active (not pending) work", func() {
			d := map[string]coverageDepth{"high": {Pending: 0, Active: 8}, "low": {Pending: 3}}
			rows := buildCoverageRows([]*asynq.ServerInfo{strictServer}, queues, d)

			Convey("Then active work still counts as non-empty and low is starved", func() {
				So(rowByQueue(rows, "low").Status, ShouldEqual, coverageStatusStarved)
			})
		})
	})

	Convey("Given conditions the heuristic must NOT flag", t, func() {
		Convey("When the higher-priority queue is drained", func() {
			depths := map[string]coverageDepth{"high": {Pending: 0, Active: 0}, "low": {Pending: 3}}
			rows := buildCoverageRows([]*asynq.ServerInfo{strictServer}, queues, depths)

			Convey("Then low is not starved — the server will reach it", func() {
				So(rowByQueue(rows, "low").Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When the consuming server is not strict-priority", func() {
			fair := srv("s1", "wrk-01", 100, 8, false, map[string]int{"high": 3, "low": 1})
			depths := map[string]coverageDepth{"high": {Pending: 500}, "low": {Pending: 3}}
			rows := buildCoverageRows([]*asynq.ServerInfo{fair}, queues, depths)

			Convey("Then weighted-fair scheduling gives low turns — not starved", func() {
				So(rowByQueue(rows, "low").Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When a second, non-strict server also consumes the queue", func() {
			fair := srv("s2", "wrk-02", 200, 4, false, map[string]int{"low": 1})
			depths := map[string]coverageDepth{"high": {Pending: 500}, "low": {Pending: 3}}
			rows := buildCoverageRows([]*asynq.ServerInfo{strictServer, fair}, queues, depths)

			Convey("Then ANY server that serves the queue un-starves it", func() {
				So(rowByQueue(rows, "low").Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When the queue is mid-tier rather than at the lowest weight", func() {
			mid := srv("s1", "wrk-01", 100, 8, true, map[string]int{"high": 3, "mid": 2, "low": 1})
			depths := map[string]coverageDepth{
				"high": {Pending: 500}, "mid": {Pending: 5}, "low": {Pending: 3},
			}
			rows := buildCoverageRows([]*asynq.ServerInfo{mid}, []string{"high", "mid", "low"}, depths)

			Convey("Then only the lowest class is flagged (documented narrowness)", func() {
				So(rowByQueue(rows, "mid").Status, ShouldEqual, coverageStatusOK)
				So(rowByQueue(rows, "low").Status, ShouldEqual, coverageStatusStarved)
			})
		})

		Convey("When all queues share one weight on a strict server", func() {
			flat := srv("s1", "wrk-01", 100, 8, true, map[string]int{"a": 1, "b": 1})
			depths := map[string]coverageDepth{"a": {Pending: 10}, "b": {Pending: 10}}
			rows := buildCoverageRows([]*asynq.ServerInfo{flat}, []string{"a", "b"}, depths)

			Convey("Then no higher class exists, so nothing is starved", func() {
				So(rowByQueue(rows, "a").Status, ShouldEqual, coverageStatusOK)
				So(rowByQueue(rows, "b").Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When depth data is unavailable", func() {
			rows := buildCoverageRows([]*asynq.ServerInfo{strictServer}, queues, nil)

			Convey("Then starvation is never claimed without evidence", func() {
				So(rowByQueue(rows, "low").Status, ShouldEqual, coverageStatusOK)
			})
		})
	})
}

func TestBuildCoverageRowsShape(t *testing.T) {
	Convey("Given servers with overlapping queue maps", t, func() {
		servers := []*asynq.ServerInfo{
			srv("s2", "wrk-02", 200, 8, false, map[string]int{"shared": 2}),
			srv("s1", "wrk-01", 100, 4, false, map[string]int{"shared": 5, "confonly": 1}),
		}
		// "confonly" is in a server config but not yet registered in
		// asynq:queues; "unwatched" is registered with pending but has no
		// consumer.
		queues := []string{"shared", "unwatched"}
		depths := map[string]coverageDepth{
			"shared":    {Pending: 9},
			"unwatched": {Pending: 1},
		}

		Convey("When the join runs", func() {
			rows := buildCoverageRows(servers, queues, depths)

			Convey("Then the row set is the union of registered and config-only queues", func() {
				So(len(rows), ShouldEqual, 3)
				So(rowByQueue(rows, "confonly"), ShouldNotBeNil)
			})
			Convey("Then a config-only queue has no cached depth and omits pending", func() {
				So(rowByQueue(rows, "confonly").Pending, ShouldBeNil)
			})
			Convey("Then consumers are sorted by host then pid with per-server weights", func() {
				r := rowByQueue(rows, "shared")
				So(len(r.Consumers), ShouldEqual, 2)
				So(r.Consumers[0].Host, ShouldEqual, "wrk-01")
				So(r.Consumers[0].Weight, ShouldEqual, 5)
				So(r.Consumers[1].Host, ShouldEqual, "wrk-02")
				So(r.Consumers[1].Weight, ShouldEqual, 2)
			})
			Convey("Then total_concurrency sums the consuming servers' slots", func() {
				So(rowByQueue(rows, "shared").TotalConcurrency, ShouldEqual, 12)
				So(rowByQueue(rows, "unwatched").TotalConcurrency, ShouldEqual, 0)
			})
			Convey("Then problem rows sort first: uncovered, then the rest alphabetical", func() {
				So(rows[0].Queue, ShouldEqual, "unwatched")
				So(rows[0].Status, ShouldEqual, coverageStatusUncovered)
				So(rows[1].Queue, ShouldEqual, "confonly")
				So(rows[2].Queue, ShouldEqual, "shared")
			})
		})

		Convey("When there are no servers and no queues", func() {
			rows := buildCoverageRows(nil, nil, nil)

			Convey("Then the result is an empty, non-nil row set", func() {
				So(rows, ShouldNotBeNil)
				So(rows, ShouldBeEmpty)
			})
		})
	})
}
