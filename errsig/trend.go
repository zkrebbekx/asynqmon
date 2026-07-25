package errsig

import "time"

// ****************************************************************************
// This file defines:
//   - the trend heuristic behind the §3.6 trend badge
//     (new | growing | steady | recovering)
//
// The heuristic, exactly (documented because the endpoint contract promises
// it): trend derives from first_seen recency plus the count delta between
// snapshot windows. The indexer rolls a snapshot of each signature's total
// count at least every trendWindow (10m): prev_count holds the total as of
// one completed window ago, so the delta always looks 10–20 minutes back —
// long enough to not flap on a single retry resample, short enough to catch
// a storm igniting.
//
//   new        — first_seen within the last hour. A signature this young has
//                no meaningful delta history; its existence is the news.
//   growing    — total count rose since the previous window.
//   recovering — total count fell since the previous window (retry tasks
//                draining after a fix), OR the delta is flat/unknown and
//                nothing has been seen for 30 minutes (the failure stopped;
//                archived counts are monotonic, so "no longer growing +
//                gone quiet" is the honest recovery signal).
//   steady     — flat delta with recent activity.
// ****************************************************************************

const (
	// TrendNewWindow: signatures first seen within this window are "new".
	TrendNewWindow = time.Hour

	// TrendQuietAfter: a flat signature with no observation for this long is
	// "recovering" rather than "steady".
	TrendQuietAfter = 30 * time.Minute

	// trendWindow is the snapshot roll cadence backing the count delta.
	trendWindow = 10 * time.Minute
)

// Trend values (frozen frontend contract).
const (
	TrendNew        = "new"
	TrendGrowing    = "growing"
	TrendSteady     = "steady"
	TrendRecovering = "recovering"
)

// ComputeTrend derives the trend badge for one signature. prevCount is the
// signature's total count as of the previous completed snapshot window;
// pass prevCount < 0 when no completed window exists yet (unknown delta).
func ComputeTrend(firstSeen, lastSeen time.Time, count, prevCount int64, now time.Time) string {
	if !firstSeen.IsZero() && now.Sub(firstSeen) <= TrendNewWindow {
		return TrendNew
	}
	if prevCount >= 0 {
		if count > prevCount {
			return TrendGrowing
		}
		if count < prevCount {
			return TrendRecovering
		}
	}
	if lastSeen.IsZero() || now.Sub(lastSeen) > TrendQuietAfter {
		return TrendRecovering
	}
	return TrendSteady
}

// rollSnapshot advances a signature's trend snapshot in place: on the first
// observation the window opens; each time the open window is older than
// trendWindow, its count becomes prev_count and a fresh window opens at the
// current total. Called by both feeders on every write so a signature that
// stops growing still rolls toward a flat delta.
func (s *Signature) rollSnapshot(now time.Time) {
	total := s.TotalCount()
	if s.SnapAt.IsZero() {
		s.SnapCount = total
		s.SnapAt = now
		return
	}
	if now.Sub(s.SnapAt) >= trendWindow {
		s.PrevCount = s.SnapCount
		s.SnapCount = total
		s.SnapAt = now
	}
}
