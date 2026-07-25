package stats

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// ****************************************************************************
// This file defines:
//   - the attention engine (§3.1 detector table, §5.2): the eight phase-4
//     INSTANTANEOUS detectors plus SCHEDULER_GONE (#9, §3.8/§5.12 — its
//     history substrate is the scheduler snapshot set, scheduler.go),
//     evaluated once per sweep on the lease-holding replica as pure
//     functions over the sweep's observations
//   - finding construction: severity, human sentence, chips, since tracking,
//     and the pre-written console query
//   - the published report (asynqmon:cache:attention) served by every replica
//
// Phase 10 adds the baseline-dependent detectors over the §5.8 ring-buffer
// substrate: FAIL_SPIKE (7-day daily-counter baseline — live from day one,
// the dailies pre-exist in Redis), DEPTH_ANOMALY (24h rollup baseline —
// reports itself LEARNING until the substrate is old enough) and
// CONSUMERS_DROPPED (30m consumer-count window — LEARNING for its first
// 30m). Their inputs are computed by the baseline refresher (baseline.go)
// and passed in, so evaluation stays a pure function.
// ****************************************************************************

// Detector identifiers — the `detector` field of the frozen API contract.
const (
	DetectorNoConsumers      = "NO_CONSUMERS"
	DetectorOrphans          = "ORPHANS"
	DetectorPendingAge       = "PENDING_AGE"
	DetectorRetryStorm       = "RETRY_STORM"
	DetectorPastDue          = "PAST_DUE"
	DetectorPausedLong       = "PAUSED_LONG"
	DetectorArchiveTrim      = "ARCHIVE_TRIM"
	DetectorGroupStall       = "GROUP_STALL"
	DetectorSchedulerGone    = "SCHEDULER_GONE"
	DetectorFailSpike        = "FAIL_SPIKE"
	DetectorDepthAnomaly     = "DEPTH_ANOMALY"
	DetectorConsumersDropped = "CONSUMERS_DROPPED"
)

const (
	// detectorsTotal is the census (phase 10: 9 instantaneous + FAIL_SPIKE +
	// the two learning-capable baseline detectors = 12). The report footer
	// splits it into live vs learning per sweep.
	detectorsTotal = 12

	// maxAttentionFindings caps the published rail at 20 ranked findings
	// (§3.1 Region B). Detector STATE is not capped — auto-clear and since
	// tracking stay correct for every queue even when the report truncates.
	maxAttentionFindings = 20

	// retryStormWindow is the fixed look-ahead of the RETRY_STORM ZCOUNT
	// (§3.1: "retry-ETA mass in next 5 min"). The threshold is a knob; the
	// window is part of the sweep's hot-read shape and stays fixed.
	retryStormWindow = 5 * time.Minute

	// maxArchiveSize mirrors asynq's archive cap: every Archive call runs
	// ZREMRANGEBYRANK to keep at most this many entries
	// (github.com/hibiken/asynq@v0.24.1/internal/rdb/rdb.go:820,
	// `maxArchiveSize = 10000`, enforced in archiveCmd). archived == cap
	// therefore means forensic data is actively being discarded.
	maxArchiveSize = 10000
)

// AttentionFinding is one ranked row of the Needs Attention rail. The JSON
// field set is a frozen frontend contract — additive changes only.
type AttentionFinding struct {
	Queue    string `json:"queue"`
	Detector string `json:"detector"`
	// Severity is 1 (hygiene) to 5 (work is not running).
	Severity int `json:"severity"`
	// Sentence is the human, pre-written 3am summary.
	Sentence string `json:"sentence"`
	// Value is the finding's key number: a count for count detectors, whole
	// seconds for age detectors (PENDING_AGE, PAUSED_LONG, GROUP_STALL).
	Value int64 `json:"value"`
	// Chips are the rail's short labels (§3.1 table).
	Chips []string `json:"chips"`
	// Since is when the underlying condition was first observed (RFC3339).
	// Tracked in-process on the lease holder and persisted across sweeps
	// there; a lease handover restarts observation, so Since is honest as
	// "first seen by the current sweeper", never fabricated further back.
	Since string `json:"since"`
	// SuggestedQuery is the query the system wrote for you: console (Tasks)
	// grammar for task-level findings, queue-directory filter grammar for
	// queue-level ones (PAUSED_LONG).
	SuggestedQuery string `json:"suggested_query"`
	// SeriesScope/SeriesMetric reference the §5.8 series that visualizes this
	// finding (phase 10, additive): the rail renders a 30m spark from
	// GET /api/series?scope=<series_scope>&metric=<series_metric>. Empty when
	// no collected series backs the detector (PAST_DUE, GROUP_STALL, …).
	SeriesScope  string `json:"series_scope,omitempty"`
	SeriesMetric string `json:"series_metric,omitempty"`
}

// LearningDetector is one baseline detector still gathering its baseline
// (§3.1: "learning baseline — active in ~Xh").
type LearningDetector struct {
	Detector string `json:"detector"`
	// Until is when the detector goes live (RFC3339).
	Until string `json:"until"`
}

// AttentionReport is the GET /api/fleet/attention response body and the
// payload of the SSE `attention` event. Frozen frontend contract (phase-10
// additions are additive: `learning`, per-finding series refs).
type AttentionReport struct {
	Findings          []AttentionFinding `json:"findings"`
	UpdatedAt         string             `json:"updated_at"`
	DetectorsLive     int                `json:"detectors_live"`
	DetectorsLearning int                `json:"detectors_learning"`
	// Learning lists the gated detectors with their go-live times, so tiles
	// and the rail can render "learning baseline — active in ~Xh" badges.
	Learning []LearningDetector `json:"learning,omitempty"`
}

// attentionConfig carries the resolved detector knobs (Config → NewEngine).
type attentionConfig struct {
	pendingAgeSLO       time.Duration // PENDING_AGE: oldest pending beyond this
	retryStormThreshold int64         // RETRY_STORM: mass in next 5m at/above this
	pausedLongAfter     time.Duration // PAUSED_LONG: paused longer than this
	groupStallAfter     time.Duration // GROUP_STALL: oldest group member older than this
	orphanGrace         time.Duration // wording only; the count comes from the sweep

	// Phase-10 knobs (§3.1 detector table).
	failSpikeRatio    float64 // FAIL_SPIKE: today ≥ ratio × same-hour baseline (default 4)
	failSpikeMinCount int64   // FAIL_SPIKE: and at least this many failures (default 100)
	depthAnomalyRatio float64 // DEPTH_ANOMALY: pending ≥ ratio × 24h mean (default 3)
	depthAnomalyMin   int64   // DEPTH_ANOMALY: and at least this deep (default 500)
}

// groupStallObs is one examined queue's group-head observation: the oldest
// member (entry time, Unix seconds — asynq's addToGroupCmd ZADDs
// clock.Now().Unix() as the score, rdb.go AddToGroup, v0.24.1) across the
// examined groups, and which group holds it. A zero oldestSince means the
// queue was examined and no group member exists.
type groupStallObs struct {
	group       string
	oldestSince time.Time
}

// detectorKey identifies one (queue, detector) condition stream.
type detectorKey struct {
	queue    string
	detector string
}

// detectorState is the in-process, holder-only memory of one condition:
// when it was first observed and for how many consecutive sweeps — the
// substrate for both debounce (§3.1: 2 consecutive sweeps for NO_CONSUMERS /
// ORPHANS) and the `since` field. carried is GROUP_STALL-only: the last
// emitted finding, replayed verbatim on sweeps where the bounded group reader
// did not revisit this queue (so a still-possibly-stalled queue is not
// auto-cleared just because the rotation looked elsewhere).
type detectorState struct {
	firstObserved time.Time
	consecutive   int
	carried       *AttentionFinding
}

// attentionEvaluator evaluates all instantaneous detectors for one sweep.
// It is owned by the Engine and only ever touched from the sweep goroutine,
// so it needs no locking of its own.
type attentionEvaluator struct {
	cfg   attentionConfig
	state map[detectorKey]*detectorState
}

func newAttentionEvaluator(cfg attentionConfig) *attentionEvaluator {
	return &attentionEvaluator{cfg: cfg, state: make(map[detectorKey]*detectorState)}
}

// observe records that key's condition holds this sweep, carrying since /
// consecutive across sweeps, and reports whether the debounce requirement is
// met. Conditions that stop holding are simply never observed into `next`,
// which is how findings auto-clear.
func (a *attentionEvaluator) observe(next map[detectorKey]*detectorState, key detectorKey, minSweeps int, now time.Time) (time.Time, bool) {
	st := a.state[key]
	if st == nil {
		st = &detectorState{firstObserved: now}
	}
	st.consecutive++
	next[key] = st
	return st.firstObserved, st.consecutive >= minSweeps
}

// evaluate runs every detector over the sweep's snapshots. groupObs holds the
// bounded group-head reads (only queues examined THIS sweep have an entry);
// schedGone holds the sweep's SCHEDULER_GONE determinations (scheduler.go);
// det carries the phase-10 baseline inputs (baseline.go) — the zero value
// disables the baseline detectors without gating them as learning.
// Pure over its inputs plus the evaluator's own cross-sweep state.
func (a *attentionEvaluator) evaluate(snaps map[string]*QueueSnapshot, groupObs map[string]groupStallObs, schedGone []SchedulerGoneObs, det detectorInputs, now time.Time) *AttentionReport {
	next := make(map[detectorKey]*detectorState)
	findings := make([]AttentionFinding, 0)

	names := make([]string, 0, len(snaps))
	for q := range snaps {
		names = append(names, q)
	}
	sort.Strings(names)

	for _, q := range names {
		s := snaps[q]

		// 1. NO_CONSUMERS — severity 5: the work will not run. Debounced to
		// 2 consecutive sweeps so a server-heartbeat TTL flap (consumer map
		// briefly empty) does not page anyone.
		if s.Pending > 0 && s.Consumers == 0 {
			if since, ok := a.observe(next, detectorKey{q, DetectorNoConsumers}, 2, now); ok {
				findings = append(findings, AttentionFinding{
					Queue:    q,
					Detector: DetectorNoConsumers,
					Severity: 5,
					Sentence: fmt.Sprintf("%s pending, zero live consumers for %s — work will not run",
						humanCount(s.Pending), humanDuration(now.Sub(since))),
					Value:          s.Pending,
					Chips:          []string{"NO CONSUMERS"},
					Since:          rfc3339(since),
					SuggestedQuery: "state=pending queue=" + q,
					SeriesScope:    "queue:" + q,
					SeriesMetric:   MetricConsumerCount,
				})
			}
		}

		// 2. ORPHANS — severity 4. The sweep already counts lease entries
		// expired beyond the grace period; debounce to 2 consecutive sweeps
		// because asynq's recoverer normally requeues them within seconds.
		if s.OrphanCandidates > 0 {
			if since, ok := a.observe(next, detectorKey{q, DetectorOrphans}, 2, now); ok {
				findings = append(findings, AttentionFinding{
					Queue:    q,
					Detector: DetectorOrphans,
					Severity: 4,
					Sentence: fmt.Sprintf("%s active leases expired >%s ago across consecutive sweeps — a worker likely died mid-task",
						humanCount(s.OrphanCandidates), compactDuration(a.cfg.orphanGrace)),
					Value:          s.OrphanCandidates,
					Chips:          []string{"ORPHANS"},
					Since:          rfc3339(since),
					SuggestedQuery: "state=active queue=" + q + " orphaned",
					SeriesScope:    "queue:" + q,
					SeriesMetric:   MetricActive,
				})
			}
		}

		// 3. PENDING_AGE — instantaneous. Severity 3, escalating to 4 once
		// the oldest task has waited 6x the SLO (deterministic, documented —
		// not a baseline model).
		if age := s.OldestPendingAge(now); age > a.cfg.pendingAgeSLO {
			since, _ := a.observe(next, detectorKey{q, DetectorPendingAge}, 1, now)
			sev := 3
			if age >= 6*a.cfg.pendingAgeSLO {
				sev = 4
			}
			findings = append(findings, AttentionFinding{
				Queue:    q,
				Detector: DetectorPendingAge,
				Severity: sev,
				Sentence: fmt.Sprintf("oldest pending task has waited %s — over the %s SLO",
					humanDuration(age), compactDuration(a.cfg.pendingAgeSLO)),
				Value:          int64(age.Seconds()),
				Chips:          []string{"PENDING AGE"},
				Since:          rfc3339(since),
				SuggestedQuery: fmt.Sprintf("state=pending queue=%s pending_age>%s", q, compactDuration(a.cfg.pendingAgeSLO)),
				SeriesScope:    "queue:" + q,
				SeriesMetric:   MetricPending,
			})
		}

		// 4. RETRY_STORM — instantaneous. Severity 3, escalating to 4 at 10x
		// the threshold.
		if s.RetryDueSoon >= a.cfg.retryStormThreshold {
			since, _ := a.observe(next, detectorKey{q, DetectorRetryStorm}, 1, now)
			sev := 3
			if s.RetryDueSoon >= 10*a.cfg.retryStormThreshold {
				sev = 4
			}
			findings = append(findings, AttentionFinding{
				Queue:    q,
				Detector: DetectorRetryStorm,
				Severity: sev,
				Sentence: fmt.Sprintf("%s retries fire within the next 5m — at or above the %s storm threshold",
					humanCount(s.RetryDueSoon), humanCount(a.cfg.retryStormThreshold)),
				Value:          s.RetryDueSoon,
				Chips:          []string{"RETRY STORM"},
				Since:          rfc3339(since),
				SuggestedQuery: "state=retry queue=" + q + " next_run<5m",
				SeriesScope:    "queue:" + q,
				SeriesMetric:   MetricRetry,
			})
		}

		// 5. PAST_DUE — severity 4: scheduled tasks whose process-at time
		// already passed mean asynq's forwarder is stalled or absent.
		if s.PastDueScheduled > 0 {
			since, _ := a.observe(next, detectorKey{q, DetectorPastDue}, 1, now)
			findings = append(findings, AttentionFinding{
				Queue:    q,
				Detector: DetectorPastDue,
				Severity: 4,
				Sentence: fmt.Sprintf("%s scheduled tasks are past their process-at time — asynq's forwarder is stalled or absent",
					humanCount(s.PastDueScheduled)),
				Value:          s.PastDueScheduled,
				Chips:          []string{"PAST DUE"},
				Since:          rfc3339(since),
				SuggestedQuery: "state=scheduled queue=" + q + " past_due",
			})
		}

		// 6. PAUSED_LONG — severity 1 (hygiene). Verified against asynq
		// v0.24.1: RDB.Pause does SETNX(paused-key, clock.Now().Unix(), 0)
		// (internal/rdb/inspect.go), so the key's VALUE is the pause time in
		// Unix seconds and PausedSince is real data, not a fabrication. If a
		// future asynq stops storing a parsable timestamp, PausedSince
		// decodes to zero and this detector stays silent rather than
		// inventing a pause time.
		if s.Paused && !s.PausedSince.IsZero() {
			if pausedFor := now.Sub(s.PausedSince); pausedFor > a.cfg.pausedLongAfter {
				since, _ := a.observe(next, detectorKey{q, DetectorPausedLong}, 1, now)
				findings = append(findings, AttentionFinding{
					Queue:    q,
					Detector: DetectorPausedLong,
					Severity: 1,
					Sentence: fmt.Sprintf("paused for %s — past the %s threshold; producers may still be enqueueing",
						humanDuration(pausedFor), compactDuration(a.cfg.pausedLongAfter)),
					Value:          int64(pausedFor.Seconds()),
					Chips:          []string{"PAUSED"},
					Since:          rfc3339(since),
					SuggestedQuery: "name=" + q + " paused=true",
				})
			}
		}

		// 7. ARCHIVE_TRIM — severity 2. Exactly at asynq's cap (see
		// maxArchiveSize above): every further Archive discards the oldest
		// dead task, so failure forensics are being lost right now.
		if s.Archived == maxArchiveSize {
			since, _ := a.observe(next, detectorKey{q, DetectorArchiveTrim}, 1, now)
			findings = append(findings, AttentionFinding{
				Queue:    q,
				Detector: DetectorArchiveTrim,
				Severity: 2,
				Sentence: fmt.Sprintf("archived set is at asynq's %s cap — each new archive discards the oldest dead task",
					humanCount(maxArchiveSize)),
				Value:          s.Archived,
				Chips:          []string{"ARCHIVE TRIM"},
				Since:          rfc3339(since),
				SuggestedQuery: "state=archived queue=" + q,
				SeriesScope:    "queue:" + q,
				SeriesMetric:   MetricArchived,
			})
		}

		// 8. GROUP_STALL — severity 3, only for queues with groups. The
		// group reader is bounded (maxGroupStallQueues per sweep, rotating),
		// so a queue not examined THIS sweep replays its previous finding
		// unchanged instead of auto-clearing on ignorance.
		if s.Groups > 0 {
			key := detectorKey{q, DetectorGroupStall}
			obs, examined := groupObs[q]
			switch {
			case !examined:
				if st := a.state[key]; st != nil && st.carried != nil {
					next[key] = st
					findings = append(findings, *st.carried)
				}
			case !obs.oldestSince.IsZero() && now.Sub(obs.oldestSince) > a.cfg.groupStallAfter:
				age := now.Sub(obs.oldestSince)
				since, _ := a.observe(next, key, 1, now)
				f := AttentionFinding{
					Queue:    q,
					Detector: DetectorGroupStall,
					Severity: 3,
					Sentence: fmt.Sprintf("oldest task in group %q has aggregated for %s — over the %s stall threshold",
						obs.group, humanDuration(age), compactDuration(a.cfg.groupStallAfter)),
					Value:          int64(age.Seconds()),
					Chips:          []string{"GROUP STALL"},
					Since:          rfc3339(since),
					SuggestedQuery: "state=aggregating queue=" + q + " group=" + obs.group,
				}
				next[key].carried = &f
				findings = append(findings, f)
			}
		}

		// 10. FAIL_SPIKE — phase 10, live from day one (§3.1: asynq's daily
		// counters pre-exist in Redis for 90 days). Today's failed counter is
		// compared against the 7-day average prorated to the current time of
		// UTC day ("same-hour baseline" — dailies carry no finer grain).
		// A queue with zero baseline days (never seen before yesterday) has
		// no norm to spike against and stays silent; the min-count knob
		// guards the early-morning tiny-expectation window.
		if b, ok := det.failBaselines[q]; ok && b.days > 0 && s.FailedToday >= a.cfg.failSpikeMinCount {
			elapsed := float64(secondsIntoUTCDay(now)) / 86400.0
			expected := b.avgDaily * elapsed
			if expected <= 0 || float64(s.FailedToday) >= a.cfg.failSpikeRatio*expected {
				since, _ := a.observe(next, detectorKey{q, DetectorFailSpike}, 1, now)
				var vs string
				ratio := 0.0
				if expected > 0 {
					ratio = float64(s.FailedToday) / expected
					vs = fmt.Sprintf("%.0f× the 7-day same-hour baseline (~%s expected by now)",
						ratio, humanCount(int64(expected)))
				} else {
					vs = "against a 7-day baseline of zero failures"
				}
				sev := 4
				if ratio >= 10*a.cfg.failSpikeRatio {
					sev = 5
				}
				findings = append(findings, AttentionFinding{
					Queue:          q,
					Detector:       DetectorFailSpike,
					Severity:       sev,
					Sentence:       fmt.Sprintf("%s tasks failed today — %s", humanCount(s.FailedToday), vs),
					Value:          s.FailedToday,
					Chips:          []string{"FAIL SPIKE"},
					Since:          rfc3339(since),
					SuggestedQuery: "state=archived queue=" + q,
					SeriesScope:    "queue:" + q,
					SeriesMetric:   MetricFailedDelta,
				})
			}
		}

		// 11. DEPTH_ANOMALY — phase 10, learning-gated (§3.1: "requires
		// history — hidden behind a learning badge until the buffer is
		// filled"). Pending depth vs the queue's own 24h rollup-ring mean;
		// per-queue baselines additionally require the queue's ring to carry
		// enough samples. Debounced 2 sweeps so a single enqueue burst
		// crossing the threshold between sweeps does not flap.
		if det.depthLearningUntil.IsZero() {
			if b := det.depthBaselines[q]; b.ok && s.Pending >= a.cfg.depthAnomalyMin {
				baseline := b.mean
				if baseline < 1 {
					baseline = 1 // a normally-empty queue: judge against depth 1
				}
				if float64(s.Pending) >= a.cfg.depthAnomalyRatio*baseline {
					if since, ok := a.observe(next, detectorKey{q, DetectorDepthAnomaly}, 2, now); ok {
						findings = append(findings, AttentionFinding{
							Queue:    q,
							Detector: DetectorDepthAnomaly,
							Severity: 3,
							Sentence: fmt.Sprintf("pending depth %s is %.0f× the 24h baseline (~%s) — backlog is off its norm",
								humanCount(s.Pending), float64(s.Pending)/baseline, humanCount(int64(b.mean))),
							Value:          s.Pending,
							Chips:          []string{"DEPTH ANOMALY"},
							Since:          rfc3339(since),
							SuggestedQuery: "state=pending queue=" + q,
							SeriesScope:    "queue:" + q,
							SeriesMetric:   MetricPending,
						})
					}
				}
			}
		}

		// 12. CONSUMERS_DROPPED — phase 10, learning-gated for its first 30m
		// of series history. Fires when the live consumer count fell to half
		// (or less) of the 30m peak. Debounced 2 sweeps (the same heartbeat
		// TTL flap guard as NO_CONSUMERS); a drop TO zero with pending work
		// is NO_CONSUMERS' finding — this one covers the 6→2 degradation.
		if det.consumerLearningUntil.IsZero() {
			if o, ok := det.consumerDrops[q]; ok && o.covered && o.max >= 2 &&
				o.current <= o.max/2 && !(o.current == 0 && s.Pending > 0) {
				if since, ok := a.observe(next, detectorKey{q, DetectorConsumersDropped}, 2, now); ok {
					findings = append(findings, AttentionFinding{
						Queue:    q,
						Detector: DetectorConsumersDropped,
						Severity: 4,
						Sentence: fmt.Sprintf("consumers dropped %d→%d within 30m — capacity for this queue is going away",
							o.max, o.current),
						Value:          int64(o.current),
						Chips:          []string{fmt.Sprintf("CONSUMERS %d→%d", o.max, o.current)},
						Since:          rfc3339(since),
						SuggestedQuery: fmt.Sprintf("name=%s consumers<=%d", q, o.current),
						SeriesScope:    "queue:" + q,
						SeriesMetric:   MetricConsumerCount,
					})
				}
			}
		}
	}

	// 9. SCHEDULER_GONE — severity 4 (§3.8/§5.12): a persisted entry snapshot
	// with no live counterpart past the gone threshold. The observation is
	// already time-gated by the sweep (last_seen older than SchedulerGoneAfter),
	// so there is no extra debounce; `since` is the honest last observation,
	// not when the sweeper first noticed. Auto-clears the moment the entry
	// (same stable key) is observed live again. The finding's queue column is
	// the entry's TARGET queue — where its tasks would have gone.
	for _, obs := range schedGone {
		findings = append(findings, AttentionFinding{
			Queue:    obs.Queue,
			Detector: DetectorSchedulerGone,
			Severity: 4,
			Sentence: fmt.Sprintf("entry %s last seen %s ago — scheduler process gone or entry removed",
				obs.TaskType, humanDuration(now.Sub(obs.LastSeen))),
			Value:          int64(now.Sub(obs.LastSeen).Seconds()),
			Chips:          []string{"SCHEDULER GONE"},
			Since:          rfc3339(obs.LastSeen),
			SuggestedQuery: "schedulers type=" + obs.TaskType,
		})
	}

	a.state = next

	// Rank: severity first, then the key number, then stable name order so
	// equal findings do not reshuffle between sweeps.
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity > findings[j].Severity
		}
		if findings[i].Value != findings[j].Value {
			return findings[i].Value > findings[j].Value
		}
		if findings[i].Queue != findings[j].Queue {
			return findings[i].Queue < findings[j].Queue
		}
		return findings[i].Detector < findings[j].Detector
	})
	if len(findings) > maxAttentionFindings {
		findings = findings[:maxAttentionFindings]
	}

	// Learning census (§3.1): baseline detectors report themselves learning
	// instead of pretending, with the time they go live.
	var learning []LearningDetector
	if !det.depthLearningUntil.IsZero() {
		learning = append(learning, LearningDetector{Detector: DetectorDepthAnomaly, Until: rfc3339(det.depthLearningUntil)})
	}
	if !det.consumerLearningUntil.IsZero() {
		learning = append(learning, LearningDetector{Detector: DetectorConsumersDropped, Until: rfc3339(det.consumerLearningUntil)})
	}

	return &AttentionReport{
		Findings:          findings,
		UpdatedAt:         rfc3339(now),
		DetectorsLive:     detectorsTotal - len(learning),
		DetectorsLearning: len(learning),
		Learning:          learning,
	}
}

// secondsIntoUTCDay returns how far into the UTC day now is, in seconds.
func secondsIntoUTCDay(now time.Time) int {
	u := now.UTC()
	return u.Hour()*3600 + u.Minute()*60 + u.Second()
}

// ----------------------------------------------------------------------------
// Formatting helpers. Sentences are read at 3am: thousands separators and
// two-unit durations, no "1h58m32.221s" noise.
// ----------------------------------------------------------------------------

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// humanCount renders 48210 as "48,210".
func humanCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var out []byte
	lead := len(s) % 3
	if lead > 0 {
		out = append(out, s[:lead]...)
	}
	for i := lead; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}

// humanDuration renders a duration in at most two units: "21d", "9d3h",
// "1h58m", "42m", "30s".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		days := d / (24 * time.Hour)
		hours := (d % (24 * time.Hour)) / time.Hour
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	case d >= time.Hour:
		mins := (d % time.Hour) / time.Minute
		if mins == 0 {
			return fmt.Sprintf("%dh", d/time.Hour)
		}
		return fmt.Sprintf("%dh%dm", d/time.Hour, mins)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}

// compactDuration renders a threshold knob in its largest whole unit ("5m",
// "7d", "30s") for sentences and suggested queries; falls back to the Go
// default form for awkward values.
func compactDuration(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
