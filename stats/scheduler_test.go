package stats

import (
	"testing"
	"time"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// PURE unit tests for the scheduler snapshot substrate (§3.8/§5.12): stable
// key derivation, entry-ID history, hash round-trips and the SCHEDULER_GONE
// detector. No Redis — the end-to-end sweep/GONE/outcome paths live in the
// root package's scheduler_snapshot_handlers_test.go (integration DB 10).
// ****************************************************************************

func TestStableSchedulerKey(t *testing.T) {
	Convey("Given a scheduler entry's identity fields", t, func() {
		key := StableSchedulerKey("0 2 * * *", "report:nightly", []byte(`{"n":1}`), []string{`Queue("reports")`, "MaxRetry(3)"})

		Convey("Then the key is deterministic and hex-shaped", func() {
			So(key, ShouldHaveLength, 64)
			So(StableSchedulerKey("0 2 * * *", "report:nightly", []byte(`{"n":1}`), []string{`Queue("reports")`, "MaxRetry(3)"}), ShouldEqual, key)
		})

		Convey("Then option order does not change the identity", func() {
			So(StableSchedulerKey("0 2 * * *", "report:nightly", []byte(`{"n":1}`), []string{"MaxRetry(3)", `Queue("reports")`}), ShouldEqual, key)
		})

		Convey("Then any change to what the entry does is a different identity", func() {
			So(StableSchedulerKey("0 3 * * *", "report:nightly", []byte(`{"n":1}`), []string{`Queue("reports")`, "MaxRetry(3)"}), ShouldNotEqual, key)
			So(StableSchedulerKey("0 2 * * *", "report:weekly", []byte(`{"n":1}`), []string{`Queue("reports")`, "MaxRetry(3)"}), ShouldNotEqual, key)
			So(StableSchedulerKey("0 2 * * *", "report:nightly", []byte(`{"n":2}`), []string{`Queue("reports")`, "MaxRetry(3)"}), ShouldNotEqual, key)
			So(StableSchedulerKey("0 2 * * *", "report:nightly", []byte(`{"n":1}`), []string{"MaxRetry(3)"}), ShouldNotEqual, key)
		})

		Convey("Then field values cannot bleed across the hash boundaries", func() {
			// Same concatenation, different split — must stay distinct.
			So(StableSchedulerKey("a|b", "c", nil, nil), ShouldNotEqual, StableSchedulerKey("a", "b|c", nil, nil))
		})
	})
}

func TestSchedulerEntryDataFromAsynq(t *testing.T) {
	Convey("Given a live asynq entry with queue and retention options", t, func() {
		e := &asynq.SchedulerEntry{
			ID:   "eph-1",
			Spec: "@every 5m",
			Task: asynq.NewTask("cache:warm", []byte(`{"k":"v"}`)),
			Opts: []asynq.Option{asynq.Queue("cache"), asynq.Retention(2 * time.Hour), asynq.MaxRetry(1)},
			Next: time.Now().Add(time.Minute),
		}
		data := SchedulerEntryDataFromAsynq(e)

		Convey("Then the target queue and retention come from the typed option values", func() {
			So(data.Queue, ShouldEqual, "cache")
			So(data.RetentionSec, ShouldEqual, int64(7200))
			So(data.TaskType, ShouldEqual, "cache:warm")
			So(data.EntryID, ShouldEqual, "eph-1")
			So(data.Opts, ShouldHaveLength, 3)
		})

		Convey("Then the stable key matches the option-string derivation", func() {
			So(StableKeyForAsynqEntry(e), ShouldEqual,
				StableSchedulerKey("@every 5m", "cache:warm", []byte(`{"k":"v"}`), []string{`Queue("cache")`, "Retention(2h0m0s)", "MaxRetry(1)"}))
		})
	})

	Convey("Given an entry with no queue option", t, func() {
		e := &asynq.SchedulerEntry{ID: "eph-2", Spec: "@every 1m", Task: asynq.NewTask("noop", nil)}

		Convey("Then the target queue defaults to asynq's default and retention to 0", func() {
			data := SchedulerEntryDataFromAsynq(e)
			So(data.Queue, ShouldEqual, DefaultTargetQueue)
			So(data.RetentionSec, ShouldEqual, 0)
		})
	})
}

func TestSchedulerSnapshotHashRoundTrip(t *testing.T) {
	Convey("Given a snapshot with history", t, func() {
		now := time.Now().Truncate(time.Second)
		snap := &SchedulerSnapshot{
			StableKey: "abc123",
			Entry: SchedulerEntryData{
				EntryID: "id-2", Spec: "0 2 * * *", TaskType: "report:nightly",
				Payload: []byte(`{"n":1}`), Opts: []string{`Queue("reports")`},
				Queue: "reports", RetentionSec: 3600,
				Next: now.Add(time.Hour), Prev: now.Add(-time.Hour),
			},
			EntryIDs:  []string{"id-2", "id-1"},
			FirstSeen: now.Add(-48 * time.Hour),
			LastSeen:  now,
		}

		Convey("When encoded to a hash and decoded back", func() {
			fields, err := snap.toHash()
			So(err, ShouldBeNil)
			h := make(map[string]string, len(fields))
			for k, v := range fields {
				h[k] = v.(string)
			}
			got := schedulerSnapshotFromHash("abc123", h)

			Convey("Then every field survives, including the §5.12 hash layout", func() {
				So(got, ShouldNotBeNil)
				So(h["last_entry_id"], ShouldEqual, "id-2")
				So(got.Entry.EntryID, ShouldEqual, "id-2")
				So(got.Entry.Queue, ShouldEqual, "reports")
				So(got.Entry.RetentionSec, ShouldEqual, 3600)
				So(got.Entry.Payload, ShouldResemble, []byte(`{"n":1}`))
				So(got.EntryIDs, ShouldResemble, []string{"id-2", "id-1"})
				So(got.FirstSeen.UnixNano(), ShouldEqual, snap.FirstSeen.UnixNano())
				So(got.LastSeen.UnixNano(), ShouldEqual, snap.LastSeen.UnixNano())
			})
		})

		Convey("When the entry JSON is corrupt", func() {
			Convey("Then decoding degrades to absent, never a fabricated row", func() {
				So(schedulerSnapshotFromHash("abc123", map[string]string{"entry": "{not json"}), ShouldBeNil)
			})
		})

		Convey("When entry_ids is missing (older writer)", func() {
			Convey("Then the ID history degrades to last_entry_id", func() {
				got := schedulerSnapshotFromHash("abc123", map[string]string{
					"entry":         `{"entry_id":"id-9","queue":"default"}`,
					"last_entry_id": "id-9",
				})
				So(got, ShouldNotBeNil)
				So(got.EntryIDs, ShouldResemble, []string{"id-9"})
			})
		})
	})
}

func TestPushEntryID(t *testing.T) {
	Convey("Given an entry-ID history", t, func() {
		Convey("Then a new ID is prepended and duplicates collapse", func() {
			So(pushEntryID(nil, "a"), ShouldResemble, []string{"a"})
			So(pushEntryID([]string{"a"}, "a"), ShouldResemble, []string{"a"})
			So(pushEntryID([]string{"a"}, "b"), ShouldResemble, []string{"b", "a"})
			So(pushEntryID([]string{"b", "a"}, "a"), ShouldResemble, []string{"a", "b"})
			So(pushEntryID([]string{"a"}, ""), ShouldResemble, []string{"a"})
		})

		Convey("Then the history is capped at the max", func() {
			ids := []string{}
			for i := 0; i < maxSchedulerEntryIDs+3; i++ {
				ids = pushEntryID(ids, string(rune('a'+i)))
			}
			So(ids, ShouldHaveLength, maxSchedulerEntryIDs)
			So(ids[0], ShouldEqual, string(rune('a'+maxSchedulerEntryIDs+2))) // newest first
		})
	})
}

func TestSchedulerGoneDetector(t *testing.T) {
	Convey("Given a GONE observation handed to the attention evaluator", t, func() {
		now := time.Date(2026, 7, 25, 15, 0, 0, 0, time.UTC)
		lastSeen := now.Add(-2 * time.Hour)
		ev := newAttentionEvaluator(attentionConfig{
			pendingAgeSLO:       defaultPendingAgeSLO,
			retryStormThreshold: defaultRetryStormThreshold,
			pausedLongAfter:     defaultPausedLongAfter,
			groupStallAfter:     defaultGroupStallAfter,
			orphanGrace:         defaultOrphanGrace,
		})
		gone := []SchedulerGoneObs{{
			StableKey: "deadbeef",
			TaskType:  "report:nightly",
			Queue:     "reports",
			LastSeen:  lastSeen,
		}}

		Convey("When the sweep evaluates with the observation", func() {
			rep := ev.evaluate(map[string]*QueueSnapshot{}, nil, gone, detectorInputs{}, now)

			Convey("Then detector #9 raises the severity-4 finding, no debounce", func() {
				So(rep.Findings, ShouldHaveLength, 1)
				f := rep.Findings[0]
				So(f.Detector, ShouldEqual, DetectorSchedulerGone)
				So(f.Severity, ShouldEqual, 4)
				So(f.Queue, ShouldEqual, "reports")
				So(f.Sentence, ShouldEqual, "entry report:nightly last seen 2h ago — scheduler process gone or entry removed")
				So(f.Value, ShouldEqual, int64(7200)) // age in whole seconds
				So(f.Chips, ShouldResemble, []string{"SCHEDULER GONE"})
				So(f.Since, ShouldEqual, lastSeen.Format(time.RFC3339))
				So(f.SuggestedQuery, ShouldEqual, "schedulers type=report:nightly")
			})

			Convey("Then the census counts every detector live (zero-value inputs carry no learning gates)", func() {
				So(rep.DetectorsLive, ShouldEqual, detectorsTotal)
			})
		})

		Convey("When the entry is observed live again (no observation passed)", func() {
			ev.evaluate(map[string]*QueueSnapshot{}, nil, gone, detectorInputs{}, now)
			rep := ev.evaluate(map[string]*QueueSnapshot{}, nil, nil, detectorInputs{}, now.Add(5*time.Second))

			Convey("Then the finding auto-clears", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})
	})
}
