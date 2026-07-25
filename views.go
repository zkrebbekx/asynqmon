package asynqmon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines the saved-view store (build contract §4.2, phase 11):
//
//	View = {id, name, target: tasks|queues, state JSON, created_by,
//	        created_at, system}
//
// Views are stored SERVER-SIDE in Redis — team assets for on-call handoff,
// not localStorage:
//
//	asynqmon:views:<id>   HASH  (one view)
//	asynqmon:views:index  ZSET  (score = created_at unix ms, member = id)
//
// `state` is the view's URL-serialized surface state exactly as the frontend
// urlstate module owns it (tasks: {"q", "mode", "size"}; queues: {"f",
// "sort", "dir", "limit"}). It is stored and served VERBATIM — §4.2 says
// relative-time → absolute conversion happens at share time in the frontend,
// so the server never rewrites it.
//
// System views are seeded at startup (idempotent — re-seeding preserves
// created_at), are undeletable and unmodifiable (400 with the reason), and
// keep their definitions self-healing: a hand-edited system view is restored
// to its shipped definition on the next boot.
// ****************************************************************************

const (
	viewsIndexKey = "asynqmon:views:index"
	viewKeyPrefix = "asynqmon:views:"

	viewMaxNameLen  = 120
	viewMaxStateLen = 4096
	// Hard cap on stored views so a curl loop cannot grow the set unbounded
	// (mirrors the markers cap policy).
	viewMaxCount = 500
)

// View targets — which surface the state JSON belongs to.
const (
	viewTargetTasks  = "tasks"
	viewTargetQueues = "queues"
)

// View is one saved view.
type View struct {
	ID        string
	Name      string
	Target    string // "tasks" | "queues"
	State     json.RawMessage
	CreatedBy string
	CreatedAt time.Time
	System    bool
}

// systemViews are the §4.2 shipped views. IDs are stable so seeding is
// idempotent; definition order is their display order. "Archive growth 24h"
// from the contract's list is deliberately absent: no current AQL predicate
// or queue-filter clause expresses it honestly (archived deltas live in the
// §5.8 series, not in a filterable field) — shipping a view whose query
// can't answer its name violates the honesty rule.
var systemViews = []View{
	{
		ID:     "sys_exhausted_retries",
		Name:   "Exhausted retries",
		Target: viewTargetTasks,
		State:  json.RawMessage(`{"q":"state=retry retries>=5"}`),
	},
	{
		ID:     "sys_orphaned_actives",
		Name:   "Orphaned actives",
		Target: viewTargetTasks,
		State:  json.RawMessage(`{"q":"state=active orphaned"}`),
	},
	{
		ID:     "sys_past_due_scheduled",
		Name:   "Past-due scheduled",
		Target: viewTargetTasks,
		State:  json.RawMessage(`{"q":"state=scheduled past_due"}`),
	},
	{
		ID:     "sys_zero_consumer_queues",
		Name:   "Zero-consumer queues",
		Target: viewTargetQueues,
		State:  json.RawMessage(`{"f":"consumers=0 pending>0"}`),
	},
	{
		ID:     "sys_paused_with_producers",
		Name:   "Paused with producers",
		Target: viewTargetQueues,
		State:  json.RawMessage(`{"f":"paused=true pending>0"}`),
	},
}

// systemViewRank maps system IDs to their display order (also the fast
// "is this a system id" test used before any Redis read).
var systemViewRank = func() map[string]int {
	m := make(map[string]int, len(systemViews))
	for i, v := range systemViews {
		m[v.ID] = i
	}
	return m
}()

// viewStore abstracts the Redis structures so handlers are unit-testable;
// redisViewStore is the production implementation.
type viewStore interface {
	List(ctx context.Context) ([]View, error)
	Get(ctx context.Context, id string) (View, bool, error)
	Put(ctx context.Context, v View) error
	Delete(ctx context.Context, id string) error
	Count(ctx context.Context) (int64, error)
}

type redisViewStore struct {
	rc redis.UniversalClient
}

func newRedisViewStore(rc redis.UniversalClient) redisViewStore {
	return redisViewStore{rc: rc}
}

func viewKey(id string) string { return viewKeyPrefix + id }

func (s redisViewStore) Put(ctx context.Context, v View) error {
	pipe := s.rc.Pipeline()
	pipe.HSet(ctx, viewKey(v.ID), map[string]interface{}{
		"id":         v.ID,
		"name":       v.Name,
		"target":     v.Target,
		"state":      string(v.State),
		"created_by": v.CreatedBy,
		"created_at": strconv.FormatInt(v.CreatedAt.UnixMilli(), 10),
		"system":     boolField(v.System),
	})
	pipe.ZAdd(ctx, viewsIndexKey, redis.Z{Score: float64(v.CreatedAt.UnixMilli()), Member: v.ID})
	_, err := pipe.Exec(ctx)
	return err
}

func boolField(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func viewFromHash(h map[string]string) (View, bool) {
	if len(h) == 0 || h["id"] == "" {
		return View{}, false
	}
	ms, _ := strconv.ParseInt(h["created_at"], 10, 64)
	return View{
		ID:        h["id"],
		Name:      h["name"],
		Target:    h["target"],
		State:     json.RawMessage(h["state"]),
		CreatedBy: h["created_by"],
		CreatedAt: time.UnixMilli(ms),
		System:    h["system"] == "1",
	}, true
}

func (s redisViewStore) Get(ctx context.Context, id string) (View, bool, error) {
	h, err := s.rc.HGetAll(ctx, viewKey(id)).Result()
	if err != nil {
		return View{}, false, err
	}
	v, ok := viewFromHash(h)
	return v, ok, nil
}

func (s redisViewStore) List(ctx context.Context) ([]View, error) {
	ids, err := s.rc.ZRange(ctx, viewsIndexKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	pipe := s.rc.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, viewKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([]View, 0, len(ids))
	for _, cmd := range cmds {
		// A dangling index member (hash expired/deleted out of band) is
		// skipped, not fatal.
		if v, ok := viewFromHash(cmd.Val()); ok {
			out = append(out, v)
		}
	}
	return out, nil
}

func (s redisViewStore) Delete(ctx context.Context, id string) error {
	pipe := s.rc.Pipeline()
	pipe.Del(ctx, viewKey(id))
	pipe.ZRem(ctx, viewsIndexKey, id)
	_, err := pipe.Exec(ctx)
	return err
}

func (s redisViewStore) Count(ctx context.Context) (int64, error) {
	return s.rc.ZCard(ctx, viewsIndexKey).Result()
}

// seedSystemViews writes the shipped system views. Idempotent: an existing
// system view keeps its created_at/created_by while name/target/state are
// restored to the shipped definition (self-healing). Called once at startup.
func seedSystemViews(ctx context.Context, store viewStore) error {
	for _, def := range systemViews {
		existing, ok, err := store.Get(ctx, def.ID)
		if err != nil {
			return err
		}
		v := def
		v.System = true
		v.CreatedBy = "system"
		if ok {
			v.CreatedAt = existing.CreatedAt
		} else {
			v.CreatedAt = time.Now()
		}
		if err := store.Put(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// sortViewsForDisplay orders system views first (shipped order), then user
// views newest-first.
func sortViewsForDisplay(views []View) {
	sort.SliceStable(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if a.System != b.System {
			return a.System
		}
		if a.System { // both system: shipped order
			return systemViewRank[a.ID] < systemViewRank[b.ID]
		}
		return a.CreatedAt.After(b.CreatedAt) // both user: newest first
	})
}

// validateViewState checks that raw is a JSON object within the size cap and
// returns it compacted (whitespace normalized, values verbatim).
func validateViewState(raw json.RawMessage) (json.RawMessage, string) {
	if len(raw) == 0 {
		return nil, "state is required (the view's URL-state JSON object)"
	}
	if len(raw) > viewMaxStateLen {
		return nil, "state is capped at 4096 bytes"
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, "state must be a JSON object of URL-state params"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, "state must be a JSON object of URL-state params"
	}
	return json.RawMessage(buf.Bytes()), ""
}

// newViewID returns a short random view id ("vw_" + 16 hex chars).
func newViewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "vw_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "vw_" + hex.EncodeToString(b)
}
