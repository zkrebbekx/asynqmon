package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - Store: all Redis persistence for jobs (§5.4) and the audit stream (§5.11)
//   - the per-job claim lease + fencing token, and the fence-guarded batch
//     write every runner mutation goes through (§5.13)
//
// Keys (all asynqmon-owned; asynqmon never writes under "asynq:"):
//   asynqmon:jobs                 ZSET   job index, score = created unix ms
//   asynqmon:jobs:<id>            HASH   job fields (incl. fence + cursor)
//   asynqmon:jobs:<id>:candidates LIST   "queue\x1fid" refs from enumeration
//   asynqmon:jobs:<id>:sample     LIST   ≤10 JSON sample rows
//   asynqmon:jobs:<id>:failures   LIST   ≤10k JSON per-item failures
//   asynqmon:jobs:<id>:lease      STRING claim lease (SET NX PX)
//   asynqmon:audit                STREAM capped audit log (MAXLEN ~10000)
// ****************************************************************************

const (
	jobsIndexKey   = "asynqmon:jobs"
	jobKeyPrefix   = "asynqmon:jobs:"
	auditStreamKey = "asynqmon:audit"

	// auditMaxLen caps the audit stream (§5.11: "capped Redis stream").
	auditMaxLen = 10000

	// maxFailureReport caps the per-item failure report; overflow is counted
	// (§3.9: "capped at 10k items with counts beyond").
	maxFailureReport = 10000

	// sampleSize is the preview sample size (§5.4).
	sampleSize = 10

	// jobsIndexCap bounds how many finished jobs the index retains.
	jobsIndexCap = 1000

	// jobTTL expires job artifacts so abandoned/ancient jobs don't accrete.
	jobTTL = 30 * 24 * time.Hour
)

// Errors returned by Store's state-transition methods; the HTTP layer maps
// them onto status codes.
var (
	ErrNotFound          = errors.New("job not found")
	ErrWrongState        = errors.New("job is not in a state that allows this")
	ErrPreviewIncomplete = errors.New("delete requires a completed preview; wait for enumeration to finish")
	ErrPartialNotAcked   = errors.New("preview is incomplete; set proceed_on_partial to execute on a partial count")
)

func jobKey(id string) string        { return jobKeyPrefix + id }
func candidatesKey(id string) string { return jobKey(id) + ":candidates" }
func sampleKey(id string) string     { return jobKey(id) + ":sample" }
func failuresKey(id string) string   { return jobKey(id) + ":failures" }
func leaseKey(id string) string      { return jobKey(id) + ":lease" }

// candidateSep joins queue and task id in a candidate ref. Task IDs are
// UUIDs and queue names cannot contain control bytes, so 0x1f is safe.
const candidateSep = "\x1f"

// Store is the shared persistence layer: every replica's HTTP handlers and
// the claiming runner read and write jobs through it.
type Store struct {
	rc redis.UniversalClient
}

// NewStore creates a Store. It is stateless and cheap.
func NewStore(rc redis.UniversalClient) *Store {
	if rc == nil {
		panic("jobs.NewStore: redis client is required")
	}
	return &Store{rc: rc}
}

// NewJobID returns a time-ordered, collision-resistant job id ("jb_<unixms>_<rand>").
// Time-ordered ids make the index zset and log lines line up chronologically.
func NewJobID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("jb_%d_%d", time.Now().UnixMilli(), time.Now().UnixNano()%100000)
	}
	return fmt.Sprintf("jb_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// ----------------------------------------------------------------------------
// Hash (de)serialization
// ----------------------------------------------------------------------------

func fmtBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.UnixMilli(), 10)
}

func parseTimeMs(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// jobToHash flattens the full job into hash fields (used only at Create;
// later mutations write just the fields they own).
func jobToHash(j *Job) map[string]interface{} {
	scopeJSON, _ := json.Marshal(j.Scope)
	queuesJSON, _ := json.Marshal(j.cursorQueues)
	groupsJSON, _ := json.Marshal(j.cursorGroups)
	return map[string]interface{}{
		"id":                 j.ID,
		"verb":               string(j.Verb),
		"scope":              string(scopeJSON),
		"phase":              string(j.Phase),
		"state":              string(j.State),
		"throttle":           strconv.Itoa(j.Throttle),
		"reason":             j.Reason,
		"actor":              j.Actor,
		"candidates":         strconv.FormatInt(j.Counts.Candidates, 10),
		"acted":              strconv.FormatInt(j.Counts.Acted, 10),
		"skipped":            strconv.FormatInt(j.Counts.Skipped, 10),
		"failed":             strconv.FormatInt(j.Counts.Failed, 10),
		"cost_class":         string(j.CostClass),
		"cost_list_len":      strconv.FormatInt(j.CostListLen, 10),
		"preview_complete":   fmtBool(j.PreviewComplete),
		"proceed_on_partial": fmtBool(j.ProceedOnPartial),
		"created_at":         fmtTime(j.CreatedAt),
		"started_at":         fmtTime(j.StartedAt),
		"finished_at":        fmtTime(j.FinishedAt),
		"fence":              strconv.FormatInt(j.Fence, 10),
		"error":              j.Error,
		"failures_overflow":  strconv.FormatInt(j.FailuresOverflow, 10),
		"ctl":                j.Ctl,
		"cursor_queues":      string(queuesJSON),
		"cursor_groups":      string(groupsJSON),
		"cursor_qidx":        strconv.Itoa(j.cursorQIdx),
		"cursor_gidx":        strconv.Itoa(j.cursorGIdx),
		"cursor_page":        strconv.Itoa(j.cursorPage),
		"act_cursor":         strconv.FormatInt(j.actCursor, 10),
	}
}

func jobFromHash(m map[string]string) (*Job, error) {
	if len(m) == 0 || m["id"] == "" {
		return nil, ErrNotFound
	}
	j := &Job{
		ID:               m["id"],
		Verb:             Verb(m["verb"]),
		Phase:            Phase(m["phase"]),
		State:            State(m["state"]),
		Reason:           m["reason"],
		Actor:            m["actor"],
		CostClass:        CostClass(m["cost_class"]),
		CostListLen:      parseInt64(m["cost_list_len"]),
		PreviewComplete:  m["preview_complete"] == "1",
		ProceedOnPartial: m["proceed_on_partial"] == "1",
		CreatedAt:        parseTimeMs(m["created_at"]),
		StartedAt:        parseTimeMs(m["started_at"]),
		FinishedAt:       parseTimeMs(m["finished_at"]),
		Fence:            parseInt64(m["fence"]),
		Error:            m["error"],
		FailuresOverflow: parseInt64(m["failures_overflow"]),
		Ctl:              m["ctl"],
	}
	j.Throttle, _ = strconv.Atoi(m["throttle"])
	j.Counts = Counts{
		Candidates: parseInt64(m["candidates"]),
		Acted:      parseInt64(m["acted"]),
		Skipped:    parseInt64(m["skipped"]),
		Failed:     parseInt64(m["failed"]),
	}
	if err := json.Unmarshal([]byte(m["scope"]), &j.Scope); err != nil {
		return nil, fmt.Errorf("decoding job %s scope: %w", j.ID, err)
	}
	// Cursor fields tolerate absence (fresh job) but not corruption.
	if v := m["cursor_queues"]; v != "" {
		_ = json.Unmarshal([]byte(v), &j.cursorQueues)
	}
	if v := m["cursor_groups"]; v != "" {
		_ = json.Unmarshal([]byte(v), &j.cursorGroups)
	}
	j.cursorQIdx, _ = strconv.Atoi(m["cursor_qidx"])
	j.cursorGIdx, _ = strconv.Atoi(m["cursor_gidx"])
	j.cursorPage, _ = strconv.Atoi(m["cursor_page"])
	j.actCursor = parseInt64(m["act_cursor"])
	return j, nil
}

// ----------------------------------------------------------------------------
// CRUD
// ----------------------------------------------------------------------------

// Create persists a new job and indexes it, newest-first. The job must
// already be validated (verb capability, reason present).
func (s *Store) Create(ctx context.Context, j *Job) error {
	pipe := s.rc.TxPipeline()
	pipe.HSet(ctx, jobKey(j.ID), jobToHash(j))
	pipe.Expire(ctx, jobKey(j.ID), jobTTL)
	pipe.ZAdd(ctx, jobsIndexKey, redis.Z{Score: float64(j.CreatedAt.UnixMilli()), Member: j.ID})
	// Bound the index; artifact keys of evicted jobs age out via their TTL.
	pipe.ZRemRangeByRank(ctx, jobsIndexKey, 0, int64(-jobsIndexCap-1))
	_, err := pipe.Exec(ctx)
	return err
}

// Get loads one job.
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	m, err := s.rc.HGetAll(ctx, jobKey(id)).Result()
	if err != nil {
		return nil, err
	}
	return jobFromHash(m)
}

// List returns up to limit jobs, newest first. Jobs whose hash has expired
// out from under the index are silently skipped.
func (s *Store) List(ctx context.Context, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 50
	}
	ids, err := s.rc.ZRevRange(ctx, jobsIndexKey, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	pipe := s.rc.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, jobKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	out := make([]*Job, 0, len(ids))
	for _, c := range cmds {
		m, err := c.Result()
		if err != nil {
			continue
		}
		if j, err := jobFromHash(m); err == nil {
			out = append(out, j)
		}
	}
	return out, nil
}

// Sample returns the job's preview sample rows.
func (s *Store) Sample(ctx context.Context, id string) ([]SampleRow, error) {
	raw, err := s.rc.LRange(ctx, sampleKey(id), 0, sampleSize-1).Result()
	if err != nil {
		return nil, err
	}
	rows := make([]SampleRow, 0, len(raw))
	for _, r := range raw {
		var row SampleRow
		if json.Unmarshal([]byte(r), &row) == nil {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// Failures returns one page of the per-item failure report plus the total
// recorded (the report itself is capped; Job.FailuresOverflow counts beyond).
func (s *Store) Failures(ctx context.Context, id string, offset, limit int64) ([]ItemFailure, int64, error) {
	if limit <= 0 {
		limit = 100
	}
	pipe := s.rc.Pipeline()
	lenCmd := pipe.LLen(ctx, failuresKey(id))
	pageCmd := pipe.LRange(ctx, failuresKey(id), offset, offset+limit-1)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, 0, err
	}
	total, _ := lenCmd.Result()
	raw, _ := pageCmd.Result()
	out := make([]ItemFailure, 0, len(raw))
	for _, r := range raw {
		var f ItemFailure
		if json.Unmarshal([]byte(r), &f) == nil {
			out = append(out, f)
		}
	}
	return out, total, nil
}

// CandidateCount returns how many candidate refs enumeration has persisted.
func (s *Store) CandidateCount(ctx context.Context, id string) (int64, error) {
	return s.rc.LLen(ctx, candidatesKey(id)).Result()
}

// Candidates returns one page of candidate refs as (queue, taskID) pairs.
func (s *Store) Candidates(ctx context.Context, id string, offset, limit int64) ([][2]string, error) {
	raw, err := s.rc.LRange(ctx, candidatesKey(id), offset, offset+limit-1).Result()
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(raw))
	for _, r := range raw {
		if i := indexByte(r, 0x1f); i > 0 {
			out = append(out, [2]string{r[:i], r[i+1:]})
		}
	}
	return out, nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// ----------------------------------------------------------------------------
// Control-plane transitions (HTTP-handler writes; not fence-guarded — these
// are operator intents, the fence protects runner progress writes)
// ----------------------------------------------------------------------------

// RequestExecute applies the §4.3 execute gate and, when it passes, moves the
// job into the execute phase. Delete strictly requires a completed preview;
// run/archive/cancel may proceed on a partial count only with an explicit
// proceed_on_partial acknowledgment (execution still completes its own
// enumeration server-side either way). throttle > 0 and reason != "" override
// the job's values — both are execution parameters the operator finalizes at
// confirm time (the preview job is created before the confirm form is filled
// in, so its reason may be a placeholder until here).
func (s *Store) RequestExecute(ctx context.Context, id string, proceedOnPartial bool, throttle int, reason string) (*Job, error) {
	j, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.Phase != PhasePreview || (j.State != StatePreviewing && j.State != StatePreviewReady) {
		return nil, fmt.Errorf("%w: phase=%s state=%s", ErrWrongState, j.Phase, j.State)
	}
	if !j.PreviewComplete {
		if j.Verb == VerbDelete {
			return nil, ErrPreviewIncomplete
		}
		if !proceedOnPartial {
			return nil, ErrPartialNotAcked
		}
	}
	now := time.Now()
	fields := map[string]interface{}{
		"phase":              string(PhaseExecute),
		"state":              string(StateRunning),
		"proceed_on_partial": fmtBool(proceedOnPartial),
		"started_at":         fmtTime(now),
	}
	if throttle > 0 {
		fields["throttle"] = strconv.Itoa(throttle)
	}
	if reason != "" {
		fields["reason"] = reason
	}
	if err := s.rc.HSet(ctx, jobKey(id), fields).Err(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// RequestCancel asks the claiming runner to stop after the current batch. A
// dormant job (preview_ready, unclaimed) is finalized immediately.
func (s *Store) RequestCancel(ctx context.Context, id string) (*Job, error) {
	j, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.State.IsTerminal() {
		return nil, fmt.Errorf("%w: state=%s", ErrWrongState, j.State)
	}
	if j.State == StatePreviewReady && j.Phase == PhasePreview {
		// No runner is working a preview_ready job — finalize here.
		if err := s.rc.HSet(ctx, jobKey(id),
			"state", string(StateCanceled), "finished_at", fmtTime(time.Now())).Err(); err != nil {
			return nil, err
		}
		return s.Get(ctx, id)
	}
	if err := s.rc.HSet(ctx, jobKey(id), "ctl", CtlCancel).Err(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// RequestPause asks the claiming runner to pause between batches.
func (s *Store) RequestPause(ctx context.Context, id string) (*Job, error) {
	j, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.State != StateRunning && j.State != StatePreviewing {
		return nil, fmt.Errorf("%w: state=%s", ErrWrongState, j.State)
	}
	if err := s.rc.HSet(ctx, jobKey(id), "ctl", CtlPause).Err(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// RequestResume clears a pause (requested or already honored).
func (s *Store) RequestResume(ctx context.Context, id string) (*Job, error) {
	j, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.State != StatePaused && !(j.Ctl == CtlPause && (j.State == StateRunning || j.State == StatePreviewing)) {
		return nil, fmt.Errorf("%w: state=%s", ErrWrongState, j.State)
	}
	fields := map[string]interface{}{"ctl": ""}
	if j.State == StatePaused {
		// The claiming runner polls the state while paused; flipping it back
		// here resumes work even though the pause was honored already.
		if j.Phase == PhaseExecute {
			fields["state"] = string(StateRunning)
		} else {
			fields["state"] = string(StatePreviewing)
		}
	}
	if err := s.rc.HSet(ctx, jobKey(id), fields).Err(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ----------------------------------------------------------------------------
// Claim lease + fencing token (§5.13)
// ----------------------------------------------------------------------------

// claimScript atomically takes the per-job lease and mints the next fencing
// token. Returns the token, or 0 when the lease is held by someone else.
// Minting inside the same script closes the SET-then-INCR race: token order
// always equals claim order, so a stale claimer always holds a lower token.
var claimScript = redis.NewScript(`
if redis.call("SET", KEYS[1], ARGV[1], "NX", "PX", ARGV[2]) then
	return redis.call("HINCRBY", KEYS[2], "fence", 1)
end
return 0`)

// renewClaimScript extends the lease only while we still hold it.
var renewClaimScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

// releaseClaimScript drops the lease only if we hold it.
var releaseClaimScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

// Claim attempts to take the job's execution lease. On success it returns
// the freshly-minted fencing token (>0) that every subsequent progress write
// must present.
func (s *Store) Claim(ctx context.Context, id, instanceID string, ttl time.Duration) (int64, error) {
	n, err := claimScript.Run(ctx, s.rc,
		[]string{leaseKey(id), jobKey(id)}, instanceID, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RenewClaim extends the lease; false (nil error) means the lease was lost.
func (s *Store) RenewClaim(ctx context.Context, id, instanceID string, ttl time.Duration) (bool, error) {
	n, err := renewClaimScript.Run(ctx, s.rc,
		[]string{leaseKey(id)}, instanceID, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ReleaseClaim gives the lease up (best-effort; TTL is the backstop).
func (s *Store) ReleaseClaim(ctx context.Context, id, instanceID string) error {
	return releaseClaimScript.Run(ctx, s.rc, []string{leaseKey(id)}, instanceID).Err()
}

// progressScript is the fence-guarded batch write (§5.13: "every batch write
// is preceded by a token check"). It applies hash-field updates and appends
// candidates / sample rows / failures atomically — but only while the
// caller's fencing token is still the job's current one, so a paused or
// partitioned runner can never write after another replica reclaimed the job.
//
// KEYS: 1=job hash  2=candidates list  3=sample list  4=failures list
// ARGV: 1=token
//       2=nfields, then nfields × (field, value)
//       next=ncand, then ncand candidate refs
//       next=nsample, then nsample sample rows
//       next=nfail, then nfail failure rows
var progressScript = redis.NewScript(`
if redis.call("HGET", KEYS[1], "fence") ~= ARGV[1] then
	return 0
end
local i = 2
local nfields = tonumber(ARGV[i]); i = i + 1
for f = 1, nfields do
	redis.call("HSET", KEYS[1], ARGV[i], ARGV[i+1])
	i = i + 2
end
local ncand = tonumber(ARGV[i]); i = i + 1
for c = 1, ncand do
	redis.call("RPUSH", KEYS[2], ARGV[i])
	i = i + 1
end
local nsample = tonumber(ARGV[i]); i = i + 1
for m = 1, nsample do
	if redis.call("LLEN", KEYS[3]) < ` + strconv.Itoa(sampleSize) + ` then
		redis.call("RPUSH", KEYS[3], ARGV[i])
	end
	i = i + 1
end
local nfail = tonumber(ARGV[i]); i = i + 1
for f = 1, nfail do
	if redis.call("LLEN", KEYS[4]) < ` + strconv.Itoa(maxFailureReport) + ` then
		redis.call("RPUSH", KEYS[4], ARGV[i])
	else
		redis.call("HINCRBY", KEYS[1], "failures_overflow", 1)
	end
	i = i + 1
end
return 1`)

// ProgressWrite is one fence-guarded batch mutation.
type ProgressWrite struct {
	Fields     map[string]string
	Candidates []string // "queue\x1fid" refs
	Sample     []SampleRow
	Failures   []ItemFailure
}

// WriteProgress applies a batch's mutations if (and only if) token is still
// the job's current fencing token. Returns false when the fence moved on —
// the caller must stop working the job immediately.
func (s *Store) WriteProgress(ctx context.Context, id string, token int64, w ProgressWrite) (bool, error) {
	args := make([]interface{}, 0, 4+2*len(w.Fields)+len(w.Candidates)+len(w.Sample)+len(w.Failures))
	args = append(args, strconv.FormatInt(token, 10))
	args = append(args, strconv.Itoa(len(w.Fields)))
	for k, v := range w.Fields {
		args = append(args, k, v)
	}
	args = append(args, strconv.Itoa(len(w.Candidates)))
	for _, c := range w.Candidates {
		args = append(args, c)
	}
	args = append(args, strconv.Itoa(len(w.Sample)))
	for _, row := range w.Sample {
		b, _ := json.Marshal(row)
		args = append(args, string(b))
	}
	args = append(args, strconv.Itoa(len(w.Failures)))
	for _, f := range w.Failures {
		b, _ := json.Marshal(f)
		args = append(args, string(b))
	}
	n, err := progressScript.Run(ctx, s.rc,
		[]string{jobKey(id), candidatesKey(id), sampleKey(id), failuresKey(id)}, args...).Int()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, nil
	}
	// Keep artifact keys on the same clock as the job hash. Best-effort.
	pipe := s.rc.Pipeline()
	for _, k := range []string{candidatesKey(id), sampleKey(id), failuresKey(id)} {
		pipe.Expire(ctx, k, jobTTL)
	}
	_, _ = pipe.Exec(ctx)
	return true, nil
}

// completePreviewScript flips preview_complete and — only if the job is
// still in the preview phase — parks it in preview_ready. Doing the phase
// check inside the script closes the race where an execute request lands
// between the runner's read and this write (the job must stay "running").
var completePreviewScript = redis.NewScript(`
if redis.call("HGET", KEYS[1], "fence") ~= ARGV[1] then
	return 0
end
redis.call("HSET", KEYS[1], "preview_complete", "1")
if redis.call("HGET", KEYS[1], "phase") == "preview" then
	redis.call("HSET", KEYS[1], "state", "preview_ready")
end
return 1`)

// CompletePreview marks enumeration finished (fence-guarded).
func (s *Store) CompletePreview(ctx context.Context, id string, token int64) (bool, error) {
	n, err := completePreviewScript.Run(ctx, s.rc, []string{jobKey(id)},
		strconv.FormatInt(token, 10)).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ----------------------------------------------------------------------------
// Audit stream (§5.11)
// ----------------------------------------------------------------------------

// AuditEntry is one row of the capped asynqmon:audit stream.
type AuditEntry struct {
	ID           string `json:"id"` // stream entry id (also the timestamp)
	Event        string `json:"event"`
	Actor        string `json:"actor"`
	Verb         string `json:"verb"`
	Scope        Scope  `json:"scope"`
	Reason       string `json:"reason"`
	JobID        string `json:"job_id"`
	// TaskID is the subject task for single-task events (§5.10 enqueue);
	// empty on job-level entries.
	TaskID       string `json:"task_id"`
	PreviewCount int64  `json:"preview_count"`
	Acted        int64  `json:"acted"`
	Skipped      int64  `json:"skipped"`
	Failed       int64  `json:"failed"`
	At           string `json:"at"` // RFC3339
}

// Audit events.
const (
	AuditJobCreated   = "job_created"
	AuditJobFinished  = "job_finished" // done, failed, or canceled — final counts attached
	AuditTaskEnqueued = "task_enqueued" // §5.10 single-task enqueue; TaskID set, Acted = 1

	// Hygiene events (§3.10). Verb carries the report kind. Run triggers are
	// logged even in read-only mode (report generation is a read of asynq
	// state); config changes are read-only-blocked upstream.
	AuditHygieneRun    = "hygiene_report_run"
	AuditHygieneConfig = "hygiene_config_changed"
)

// AppendAudit writes one audit entry (XADD, MAXLEN ~10000).
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	scopeJSON, _ := json.Marshal(e.Scope)
	at := e.At
	if at == "" {
		at = time.Now().Format(time.RFC3339)
	}
	return s.rc.XAdd(ctx, &redis.XAddArgs{
		Stream: auditStreamKey,
		MaxLen: auditMaxLen,
		Approx: true,
		Values: map[string]interface{}{
			"event":   e.Event,
			"actor":   e.Actor,
			"verb":    e.Verb,
			"scope":   string(scopeJSON),
			"reason":  e.Reason,
			"job_id":  e.JobID,
			"task_id": e.TaskID,
			"preview": strconv.FormatInt(e.PreviewCount, 10),
			"acted":   strconv.FormatInt(e.Acted, 10),
			"skipped": strconv.FormatInt(e.Skipped, 10),
			"failed":  strconv.FormatInt(e.Failed, 10),
			"at":      at,
		},
	}).Err()
}

// ReadAudit returns up to limit entries, newest first.
func (s *Store) ReadAudit(ctx context.Context, limit int64) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	msgs, err := s.rc.XRevRangeN(ctx, auditStreamKey, "+", "-", limit).Result()
	if err != nil {
		return nil, err
	}
	out := make([]AuditEntry, 0, len(msgs))
	for _, m := range msgs {
		e := AuditEntry{ID: m.ID}
		get := func(k string) string {
			if v, ok := m.Values[k].(string); ok {
				return v
			}
			return ""
		}
		e.Event = get("event")
		e.Actor = get("actor")
		e.Verb = get("verb")
		e.Reason = get("reason")
		e.JobID = get("job_id")
		e.TaskID = get("task_id")
		e.At = get("at")
		e.PreviewCount = parseInt64(get("preview"))
		e.Acted = parseInt64(get("acted"))
		e.Skipped = parseInt64(get("skipped"))
		e.Failed = parseInt64(get("failed"))
		_ = json.Unmarshal([]byte(get("scope")), &e.Scope)
		out = append(out, e)
	}
	return out, nil
}
