package asynqmon

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/errsig"
	"github.com/hibiken/asynqmon/hygiene"
	"github.com/hibiken/asynqmon/jobs"
	"github.com/hibiken/asynqmon/stats"
)

// Options are used to configure HTTPHandler.
type Options struct {
	// URL path the handler is responsible for.
	// The path is used for the homepage of asynqmon, and every other page is rooted in this subtree.
	//
	// This field is optional. Default is "/".
	RootPath string

	// RedisConnOpt specifies the connection to a redis-server or redis-cluster.
	//
	// This field is required.
	RedisConnOpt asynq.RedisConnOpt

	// PayloadFormatter is used to convert payload bytes to string shown in the UI.
	//
	// This field is optional.
	PayloadFormatter PayloadFormatter

	// ResultFormatter is used to convert result bytes to string shown in the UI.
	//
	// This field is optional.
	ResultFormatter ResultFormatter

	// PrometheusAddress specifies the address of the Prometheus to connect to.
	//
	// This field is optional. If this field is set, asynqmon will query the Prometheus server
	// to get the time series data about queue metrics and show them in the web UI.
	PrometheusAddress string

	// Set ReadOnly to true to restrict user to view-only mode.
	ReadOnly bool

	// StatsInterval is how often the background fleet-stats sweeper refreshes
	// the shared snapshot cache backing the /api/fleet endpoints.
	//
	// This field is optional. Default is 5s.
	StatsInterval time.Duration

	// StatsDisabled turns off the background fleet-stats sweeper. The
	// /api/fleet endpoints then respond 503. Stats collection is read-only,
	// so it stays enabled in ReadOnly mode unless disabled here explicitly.
	StatsDisabled bool

	// Attention-engine detector knobs (§3.1 detector table). All optional.

	// AttentionPendingAgeSLO raises a PENDING_AGE finding when a queue's
	// oldest pending task has waited longer than this. Default 5m.
	AttentionPendingAgeSLO time.Duration

	// AttentionRetryStormThreshold raises a RETRY_STORM finding when at
	// least this many retries fire within the next 5 minutes. Default 1000.
	AttentionRetryStormThreshold int

	// AttentionPausedLongAfter raises a PAUSED_LONG finding when a queue has
	// been paused longer than this. Default 7 days.
	AttentionPausedLongAfter time.Duration

	// AttentionGroupStallAfter raises a GROUP_STALL finding when the oldest
	// member of an examined group has aggregated longer than this. Default 5m.
	AttentionGroupStallAfter time.Duration

	// ------------------------------------------------------------------
	// Fleet Console phase 5 — identity & audit (§5.11) + bulk-job runner
	// (§5.4) options.
	// ------------------------------------------------------------------

	// AuthHeader is a reverse-proxy identity header (e.g.
	// "X-Auth-Request-User") resolved as the acting user on every request.
	// Only trusted when the peer is inside TrustedProxies (any peer when
	// TrustedProxies is empty). Optional.
	AuthHeader string

	// TrustedProxies are CIDRs (or bare IPs) the AuthHeader is trusted
	// from. Malformed entries panic at boot. Optional.
	TrustedProxies []string

	// RequireIdentity refuses mutating requests (403 JSON) that carry no
	// resolvable identity (trusted header or basic-auth user). Optional.
	RequireIdentity bool

	// JobConcurrency is the max number of bulk jobs this replica works at
	// once. Default 2.
	JobConcurrency int

	// JobsDisabled turns off this replica's bulk-job runner. The /api/jobs
	// endpoints still work — another replica's runner claims the jobs. The
	// runner never starts in ReadOnly mode regardless.
	JobsDisabled bool

	// ------------------------------------------------------------------
	// Fleet Console phase 9 — error-signature index (§3.6/§5.7) options.
	// ------------------------------------------------------------------

	// ErrorIndexDisabled turns off this replica's error-signature indexer.
	// The /api/errors endpoints still serve the shared store (another
	// replica's indexer feeds it). Like stats collection, indexing writes
	// only asynqmon-owned keys, so it stays enabled in ReadOnly mode unless
	// disabled here explicitly.
	ErrorIndexDisabled bool

	// errsigIndexer is the replica's indexer instance, set by New so the
	// §3.12 health readout can report "held by this replica"; nil when
	// disabled (or in direct-router tests).
	errsigIndexer *errsig.Indexer
	// --------------------------- end phase 9 ---------------------------

	// ------------------------------------------------------------------
	// Fleet Console — enqueue capability (§5.10) options.
	// ------------------------------------------------------------------

	// EnableEnqueue turns on POST /api/queues/{qname}/tasks (create a task
	// from the UI via an embedded asynq.Client; powers clone-and-edit,
	// §3.5). Default false. Always excluded in ReadOnly mode; when gated
	// off the route answers 403 JSON with the reason.
	EnableEnqueue bool

	// enqueueClient backs the enqueue route. Set by New (and closed via
	// HTTPHandler.Close) when EnableEnqueue && !ReadOnly; same-package
	// tests that build the router directly may inject their own.
	enqueueClient *asynq.Client
	// --------------------------- end §5.10 -----------------------------

	// ------------------------------------------------------------------
	// Fleet Console phase 16 — Flow view correlation keys (§3.5).
	// ------------------------------------------------------------------

	// CorrelationKeys is the ordered list of payload keys the task
	// drawer's Flow view recognizes as correlation ids (first present
	// wins). Producers opt in by putting one of these keys in their JSON
	// payloads; the drawer then reconstructs the de-facto chain of tasks
	// sharing the value via the metadata search path. Exposed to the
	// frontend through GET /api/features ("correlation_keys"). Entries
	// are trimmed, empties dropped, and duplicates removed; nil/empty
	// means the default list: trace_id, correlation_id, request_id.
	CorrelationKeys []string
	// --------------------------- end phase 16 --------------------------

	// ------------------------------------------------------------------
	// Fleet Console phase 15 — hygiene reports (§3.10) options.
	// ------------------------------------------------------------------

	// HygieneWebhookURL, if set, receives a POST of the full report JSON
	// every time a hygiene report is generated (scheduled or Run-now).
	// Delivery is best-effort: one attempt with a 10s timeout and NO
	// retries (v1) — a missed delivery is recoverable in-UI, where the
	// persisted report stays readable.
	HygieneWebhookURL string

	// HygieneDisabled turns off this replica's leased hygiene scheduler.
	// The /api/hygiene endpoints still serve persisted reports and Run-now
	// still generates on this replica. Like stats collection, hygiene
	// writes only asynqmon-owned keys, so it stays enabled in ReadOnly
	// mode unless disabled here explicitly.
	HygieneDisabled bool

	// hygieneEngine backs the hygiene routes. Set by New (and stopped via
	// HTTPHandler.Close); muxRouter builds an unstarted engine itself when
	// absent (direct-router tests).
	hygieneEngine *hygiene.Engine
	// --------------------------- end phase 15 --------------------------
}

// HTTPHandler is a http.Handler for asynqmon application.
type HTTPHandler struct {
	router   *mux.Router
	closers  []func() error
	rootPath string // the value should not have the trailing slash
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// New creates a HTTPHandler with the given options.
func New(opts Options) *HTTPHandler {
	if opts.RedisConnOpt == nil {
		panic("asynqmon.New: RedisConnOpt field is required")
	}
	rc, ok := opts.RedisConnOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		panic(fmt.Sprintf("asnyqmon.New: unsupported RedisConnOpt type %T", opts.RedisConnOpt))
	}
	i := asynq.NewInspector(opts.RedisConnOpt)

	// Make sure that RootPath starts with a slash if provided.
	if opts.RootPath != "" && !strings.HasPrefix(opts.RootPath, "/") {
		panic(fmt.Sprintf("asynqmon.New: RootPath must start with a slash"))
	}
	// Remove tailing slash from RootPath.
	opts.RootPath = strings.TrimSuffix(opts.RootPath, "/")

	closers := []func() error{rc.Close, i.Close}

	// The stats engine runs its sweeper only on the replica that wins the
	// Redis lease; every other replica's engine stands by and serves reads
	// from the shared cache, so starting it unconditionally is safe in
	// multi-replica deployments (§5.13).
	var statsEngine *stats.Engine
	var eventsBroker *fleetEventsBroker
	if !opts.StatsDisabled {
		statsEngine = stats.NewEngine(stats.Config{
			RedisClient:         rc,
			Inspector:           i,
			Interval:            opts.StatsInterval,
			PendingAgeSLO:       opts.AttentionPendingAgeSLO,
			RetryStormThreshold: int64(opts.AttentionRetryStormThreshold),
			PausedLongAfter:     opts.AttentionPausedLongAfter,
			GroupStallAfter:     opts.AttentionGroupStallAfter,
		})
		statsEngine.Start(context.Background())
		// One broker per process fans the SSE payloads out to all
		// /api/fleet/events subscribers (§4.4) — subscribers never multiply
		// sweeps or cache reads.
		eventsBroker = newFleetEventsBroker(statsEngine)
		eventsBroker.start(context.Background())
		// Stop broker then engine before the redis clients they use close.
		closers = append([]func() error{
			func() error { eventsBroker.stop(); return nil },
			func() error { statsEngine.Stop(); return nil },
		}, closers...)
	}

	// ------------------------------------------------------------------
	// Fleet Console phase 5 — bulk-job runner lifecycle (§5.4, §5.13).
	// Same pattern as the stats engine: any replica serves the /api/jobs
	// endpoints from the shared store; execution is claimed per job with a
	// fencing token, so running the runner on every writable replica is
	// safe. Never started in ReadOnly mode — a read-only replica must not
	// mutate queues even for jobs created elsewhere.
	// ------------------------------------------------------------------
	if !opts.ReadOnly && !opts.JobsDisabled {
		jobsRunner := jobs.NewRunner(jobs.Config{
			RedisClient: rc,
			Inspector:   i,
			Concurrency: opts.JobConcurrency,
		})
		jobsRunner.Start(context.Background())
		closers = append([]func() error{
			func() error { jobsRunner.Stop(); return nil },
		}, closers...)
	}

	// ------------------------------------------------------------------
	// Fleet Console phase 9 — error-signature indexer lifecycle (§5.7,
	// §5.13). Same pattern as the stats engine: only the replica holding
	// the errsig lease feeds the index; every replica serves the
	// /api/errors endpoints from the shared store, so starting it
	// unconditionally is safe. It runs in ReadOnly mode too — it writes
	// only asynqmon-owned index keys, never asynq's.
	// ------------------------------------------------------------------
	if !opts.ErrorIndexDisabled {
		errIndexer := errsig.NewIndexer(errsig.Config{
			RedisClient: rc,
			Inspector:   i,
		})
		errIndexer.Start(context.Background())
		opts.errsigIndexer = errIndexer
		closers = append([]func() error{
			func() error { errIndexer.Stop(); return nil },
		}, closers...)
	}
	// --------------------------- end phase 9 ---------------------------

	// ------------------------------------------------------------------
	// Fleet Console — embedded enqueue client lifecycle (§5.10). Only
	// constructed when the flag is on and this replica may mutate; the
	// §5.10 route block in muxRouter registers a 403 handler otherwise.
	// ------------------------------------------------------------------
	if opts.EnableEnqueue && !opts.ReadOnly {
		ec := asynq.NewClient(opts.RedisConnOpt)
		opts.enqueueClient = ec
		closers = append(closers, ec.Close)
	}
	// --------------------------- end §5.10 -----------------------------

	// ------------------------------------------------------------------
	// Fleet Console phase 15 — hygiene report engine lifecycle (§3.10,
	// §5.13). Same pattern as the stats engine: the engine is constructed
	// on every replica (it backs the Run-now endpoint and store reads);
	// its leased interval scheduler starts unless disabled, and only the
	// asynqmon:lock:hygiene holder generates scheduled reports. Runs in
	// ReadOnly mode too — generation reads asynq state and writes only
	// asynqmon-owned report keys.
	// ------------------------------------------------------------------
	var snapshotsFn func(ctx context.Context) (*stats.ReadResult, error)
	if statsEngine != nil {
		snapshotsFn = statsEngine.Read
	}
	hygieneEngine := hygiene.NewEngine(hygiene.Config{
		RedisClient: rc,
		Inspector:   i,
		Snapshots:   snapshotsFn,
		WebhookURL:  opts.HygieneWebhookURL,
	})
	opts.hygieneEngine = hygieneEngine
	if !opts.HygieneDisabled {
		hygieneEngine.Start(context.Background())
		closers = append([]func() error{
			func() error { hygieneEngine.Stop(); return nil },
		}, closers...)
	}
	// --------------------------- end phase 15 --------------------------

	return &HTTPHandler{
		router:   muxRouter(opts, rc, i, statsEngine, eventsBroker),
		closers:  closers,
		rootPath: opts.RootPath,
	}
}

// Close closes connections to redis.
func (h *HTTPHandler) Close() error {
	for _, f := range h.closers {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// RootPath returns the root URL path used for asynqmon application.
// Returned path string does not have the trailing slash.
func (h *HTTPHandler) RootPath() string {
	return h.rootPath
}

//go:embed ui/build/*
var staticContents embed.FS

func muxRouter(opts Options, rc redis.UniversalClient, inspector *asynq.Inspector, statsEngine *stats.Engine, eventsBroker *fleetEventsBroker) *mux.Router {
	router := mux.NewRouter().PathPrefix(opts.RootPath).Subrouter()

	var payloadFmt PayloadFormatter = DefaultPayloadFormatter
	if opts.PayloadFormatter != nil {
		payloadFmt = opts.PayloadFormatter
	}

	var resultFmt ResultFormatter = DefaultResultFormatter
	if opts.ResultFormatter != nil {
		resultFmt = opts.ResultFormatter
	}

	api := router.PathPrefix("/api").Subrouter()

	// ── Phase 12: "currently viewed" tracking (§5.1 hot set) ──
	// Whichever replica serves a directory name= filter or a workspace
	// endpoint records the queue in a small TTL'd Redis zset; the sweeping
	// replica folds recent views into the hot set on tiered fleets.
	// Best-effort by design (rotation press on regardless); markViewed wraps
	// the workspace routes so their handler signatures stay untouched.
	viewTracker := stats.NewViewTracker(rc, nil)
	markViewed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if qname := mux.Vars(r)["qname"]; qname != "" {
				viewTracker.MarkViewed(r.Context(), qname)
			}
			h(w, r)
		}
	}
	// ── end phase-12 view tracking ──

	// Fleet Console endpoints (stats cache; §5.1). These serve exclusively
	// from the sweeper's snapshot cache — never per-queue Redis reads per
	// request. The legacy GET /api/queues below stays untouched for the
	// shipping dashboard; the new directory reads /api/fleet/queues.
	api.HandleFunc("/fleet/overview", newFleetOverviewHandlerFunc(statsEngine)).Methods("GET")
	api.HandleFunc("/fleet/queues", newFleetQueuesHandlerFunc(statsEngine, viewTracker)).Methods("GET")
	api.HandleFunc("/fleet/attention", newFleetAttentionHandlerFunc(statsEngine)).Methods("GET")
	api.HandleFunc("/fleet/events", newFleetEventsHandlerFunc(eventsBroker)).Methods("GET")

	// Queue endpoints. The workspace GET marks the queue "currently viewed"
	// (§5.1 hot set, phase 12).
	api.HandleFunc("/queues", newListQueuesHandlerFunc(inspector)).Methods("GET")
	api.HandleFunc("/queues/{qname}", markViewed(newGetQueueHandlerFunc(inspector))).Methods("GET")
	api.HandleFunc("/queues/{qname}", newDeleteQueueHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}:pause", newPauseQueueHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}:resume", newResumeQueueHandlerFunc(inspector)).Methods("POST")

	// ── Queue Workspace routes (Fleet Console §3.3) — registration only ──
	// Right-rail histograms: retry-ETA (exact pipelined ZCOUNT buckets on the
	// retry zset) and pending-wait (head/tail LINDEX sampling, labeled
	// "sampled, head/tail-biased"). Handlers live in queue_workspace_handlers.go.
	api.HandleFunc("/queues/{qname}/retry_histogram", markViewed(newRetryHistogramHandlerFunc(rc))).Methods("GET")
	api.HandleFunc("/queues/{qname}/pending_wait_sample", markViewed(newPendingWaitSampleHandlerFunc(rc))).Methods("GET")
	// ── end Queue Workspace routes ──

	// Queue Historical Stats endpoint.
	api.HandleFunc("/queue_stats", newListQueueStatsHandlerFunc(inspector)).Methods("GET")

	// Task endpoints.
	api.HandleFunc("/queues/{qname}/active_tasks", newListActiveTasksHandlerFunc(inspector, payloadFmt)).Methods("GET")
	api.HandleFunc("/queues/{qname}/active_tasks/{task_id}:cancel", newCancelActiveTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/active_tasks:cancel_all", newCancelAllActiveTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/active_tasks:batch_cancel", newBatchCancelActiveTasksHandlerFunc(inspector)).Methods("POST")

	api.HandleFunc("/queues/{qname}/pending_tasks", newListPendingTasksHandlerFunc(inspector, rc, payloadFmt)).Methods("GET")
	api.HandleFunc("/queues/{qname}/pending_tasks/{task_id}", newDeleteTaskHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/pending_tasks:delete_all", newDeleteAllPendingTasksHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/pending_tasks:batch_delete", newBatchDeleteTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/pending_tasks/{task_id}:archive", newArchiveTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/pending_tasks:archive_all", newArchiveAllPendingTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/pending_tasks:batch_archive", newBatchArchiveTasksHandlerFunc(inspector)).Methods("POST")

	api.HandleFunc("/queues/{qname}/scheduled_tasks", newListScheduledTasksHandlerFunc(inspector, payloadFmt)).Methods("GET")
	api.HandleFunc("/queues/{qname}/scheduled_tasks/{task_id}", newDeleteTaskHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/scheduled_tasks:delete_all", newDeleteAllScheduledTasksHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/scheduled_tasks:batch_delete", newBatchDeleteTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/scheduled_tasks/{task_id}:run", newRunTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/scheduled_tasks:run_all", newRunAllScheduledTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/scheduled_tasks:batch_run", newBatchRunTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/scheduled_tasks/{task_id}:archive", newArchiveTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/scheduled_tasks:archive_all", newArchiveAllScheduledTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/scheduled_tasks:batch_archive", newBatchArchiveTasksHandlerFunc(inspector)).Methods("POST")

	api.HandleFunc("/queues/{qname}/retry_tasks", newListRetryTasksHandlerFunc(inspector, payloadFmt)).Methods("GET")
	api.HandleFunc("/queues/{qname}/retry_tasks/{task_id}", newDeleteTaskHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/retry_tasks:delete_all", newDeleteAllRetryTasksHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/retry_tasks:batch_delete", newBatchDeleteTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/retry_tasks/{task_id}:run", newRunTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/retry_tasks:run_all", newRunAllRetryTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/retry_tasks:batch_run", newBatchRunTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/retry_tasks/{task_id}:archive", newArchiveTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/retry_tasks:archive_all", newArchiveAllRetryTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/retry_tasks:batch_archive", newBatchArchiveTasksHandlerFunc(inspector)).Methods("POST")

	api.HandleFunc("/queues/{qname}/archived_tasks", newListArchivedTasksHandlerFunc(inspector, payloadFmt)).Methods("GET")
	api.HandleFunc("/queues/{qname}/archived_tasks/{task_id}", newDeleteTaskHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/archived_tasks:delete_all", newDeleteAllArchivedTasksHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/archived_tasks:batch_delete", newBatchDeleteTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/archived_tasks/{task_id}:run", newRunTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/archived_tasks:run_all", newRunAllArchivedTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/archived_tasks:batch_run", newBatchRunTasksHandlerFunc(inspector)).Methods("POST")

	api.HandleFunc("/queues/{qname}/completed_tasks", newListCompletedTasksHandlerFunc(inspector, payloadFmt, resultFmt)).Methods("GET")
	api.HandleFunc("/queues/{qname}/completed_tasks/{task_id}", newDeleteTaskHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/completed_tasks:delete_all", newDeleteAllCompletedTasksHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/completed_tasks:batch_delete", newBatchDeleteTasksHandlerFunc(inspector)).Methods("POST")

	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks", newListAggregatingTasksHandlerFunc(inspector, payloadFmt)).Methods("GET")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks/{task_id}", newDeleteTaskHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks:delete_all", newDeleteAllAggregatingTasksHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks:batch_delete", newBatchDeleteTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks/{task_id}:run", newRunTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks:run_all", newRunAllAggregatingTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks:batch_run", newBatchRunTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks/{task_id}:archive", newArchiveTaskHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks:archive_all", newArchiveAllAggregatingTasksHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}/groups/{gname}/aggregating_tasks:batch_archive", newBatchArchiveTasksHandlerFunc(inspector)).Methods("POST")

	api.HandleFunc("/queues/{qname}/tasks/{task_id}", newGetTaskHandlerFunc(inspector, payloadFmt, resultFmt)).Methods("GET")

	// Cross-queue server-side task search / filter / pagination. `q` accepts
	// AQL (phase 6, §3.4): cursorable plans return exact totals + (score,id)
	// cursor pages; scan plans return budgeted partial results + a resume
	// scan_cursor; parse rejections are 400 {error, position, hint}. The
	// stats engine feeds the phase-12 candidate estimator (cache when fresh,
	// live fallback).
	api.HandleFunc("/tasks", newSearchTasksHandlerFunc(inspector, rc, statsEngine, payloadFmt)).Methods("GET")
	// All seven per-state counts in one pipelined pass (state pills, §3.4);
	// fleet-wide answers come from the stats cache sums.
	api.HandleFunc("/tasks/state_counts", newStateCountsHandlerFunc(inspector, rc, statsEngine)).Methods("GET")
	// Global metadata facets (distinct key=value chips) for the filtered set.
	api.HandleFunc("/task_metadata", newTaskMetadataHandlerFunc(inspector, rc, payloadFmt)).Methods("GET")
	// Failure/usage analytics: group the filtered set by type/error/queue.
	api.HandleFunc("/task_aggregate", newTaskAggregateHandlerFunc(inspector, rc, payloadFmt)).Methods("GET")
	// Apply an action to every task matching a filter (not just the current page).
	api.HandleFunc("/tasks:batch_filtered", newBulkFilteredTasksHandlerFunc(inspector, rc, payloadFmt)).Methods("POST")

	// Groups endponts
	api.HandleFunc("/queues/{qname}/groups", newListGroupsHandlerFunc(inspector)).Methods("GET")

	// Servers endpoints.
	api.HandleFunc("/servers", newListServersHandlerFunc(inspector, payloadFmt)).Methods("GET")

	// ── Workers screen routes (Fleet Console §3.7) — registration only ──
	// Coverage matrix (§5.5) and cancel-deliverability widget (§5.14).
	// Handlers live in coverage_handlers.go; join logic in coverage.go.
	api.HandleFunc("/coverage", newCoverageHandlerFunc(inspector, statsEngine)).Methods("GET")
	api.HandleFunc("/cancel-listeners", newCancelListenersHandlerFunc(rc)).Methods("GET")
	// ── end Workers screen routes ──

	// Scheduler Entry endpoints.
	api.HandleFunc("/scheduler_entries", newListSchedulerEntriesHandlerFunc(inspector, payloadFmt)).Methods("GET")
	api.HandleFunc("/scheduler_entries/{entry_id}/enqueue_events", newListSchedulerEnqueueEventsHandlerFunc(inspector)).Methods("GET")

	// ── Schedulers screen routes (Fleet Console §3.8/§5.12) ──
	// Stable-key snapshot merge (live ∪ persisted; SCHEDULER GONE rows) and
	// per-entry outcome joins. Handlers live in scheduler_snapshot_handlers.go;
	// the snapshot substrate is stats/scheduler.go. The legacy
	// /scheduler_entries endpoints above stay for back-compat.
	api.HandleFunc("/schedulers", newListSchedulersHandlerFunc(inspector, rc, payloadFmt, stats.DefaultSchedulerGoneAfter)).Methods("GET")
	api.HandleFunc("/schedulers/{stable_key}/outcomes", newSchedulerOutcomesHandlerFunc(inspector, rc)).Methods("GET")
	// ── end Schedulers screen routes ──

	// ── Errors screen routes (Fleet Console §3.6/§5.7, phase 9) ──
	// Failure-signature explorer, served from the shared errsig store (any
	// replica; the leased indexer started in New feeds it). Handlers live in
	// errsig_handlers.go; the index substrate is the errsig package.
	api.HandleFunc("/errors/signatures", newListErrorSignaturesHandlerFunc(rc)).Methods("GET")
	api.HandleFunc("/errors/signatures/{sig}", newGetErrorSignatureHandlerFunc(rc)).Methods("GET")
	// ── end Errors screen routes ──

	// Redis info endpoint.
	switch c := rc.(type) {
	case *redis.ClusterClient:
		api.HandleFunc("/redis_info", newRedisClusterInfoHandlerFunc(c, inspector)).Methods("GET")
	case *redis.Client:
		api.HandleFunc("/redis_info", newRedisInfoHandlerFunc(c)).Methods("GET")
	}

	// Time series metrics endpoints.
	// Use a dedicated client with a timeout: a hanging Prometheus must not leak
	// goroutines on every metrics poll.
	metricsClient := &http.Client{Timeout: 10 * time.Second}
	api.HandleFunc("/metrics", newGetMetricsHandlerFunc(metricsClient, opts.PrometheusAddress)).Methods("GET")

	// ------------------------------------------------------------------
	// Fleet Console phase 5 — bulk jobs + audit + identity (§3.9, §4.3,
	// §5.4, §5.11; jobs_handlers.go / identity.go). Handlers are
	// store-backed so any replica serves them; the leased runner started
	// in New performs the work. All mutating routes here are POSTs, so
	// read-only mode's method filter below blocks them.
	// ------------------------------------------------------------------
	api.Use(newActorMiddleware(opts))
	jobsStore := jobs.NewStore(rc)
	api.HandleFunc("/jobs", newCreateJobHandlerFunc(jobsStore)).Methods("POST")
	api.HandleFunc("/jobs", newListJobsHandlerFunc(jobsStore)).Methods("GET")
	api.HandleFunc("/jobs/{job_id}", newGetJobHandlerFunc(jobsStore)).Methods("GET")
	api.HandleFunc("/jobs/{job_id}/execute", newExecuteJobHandlerFunc(jobsStore)).Methods("POST")
	api.HandleFunc("/jobs/{job_id}/cancel", newCancelJobHandlerFunc(jobsStore)).Methods("POST")
	api.HandleFunc("/jobs/{job_id}/pause", newPauseJobHandlerFunc(jobsStore)).Methods("POST")
	api.HandleFunc("/jobs/{job_id}/resume", newResumeJobHandlerFunc(jobsStore)).Methods("POST")
	api.HandleFunc("/audit", newListAuditHandlerFunc(jobsStore)).Methods("GET")
	// --------------------------- end phase 5 ---------------------------

	// ------------------------------------------------------------------
	// Fleet Console phase 10 — §5.8 ring-buffer series read API + §5.14
	// deploy markers. Series are written by the leased sweeper
	// (stats/series.go) and read from shared Redis strings, so any replica
	// serves them; markers are plain shared state (§5.13). POST /markers is
	// blocked in read-only mode by the method filter below and audit-logged
	// via the phase-5 store; the actor comes from the §5.11 middleware.
	// Handlers live in series_handlers.go / markers.go.
	// ------------------------------------------------------------------
	seriesRead := newStatsSeriesReader(rc)
	api.HandleFunc("/series", newGetSeriesHandlerFunc(seriesRead)).Methods("GET")
	api.HandleFunc("/series/batch", newGetSeriesBatchHandlerFunc(seriesRead)).Methods("GET")
	markerSt := newRedisMarkerStore(rc)
	api.HandleFunc("/markers", newCreateMarkerHandlerFunc(markerSt, jobsStore)).Methods("POST")
	api.HandleFunc("/markers", newListMarkersHandlerFunc(markerSt)).Methods("GET")
	// --------------------------- end phase 10 --------------------------

	// ------------------------------------------------------------------
	// Fleet Console phase 11 — §4.2 saved views (views.go /
	// views_handlers.go). Server-side team assets in shared Redis (§5.13:
	// plain shared structures, any replica serves/writes). System views
	// are seeded idempotently at boot — best-effort, since a transient
	// Redis error at boot must not take the whole handler down; the next
	// boot self-heals. They are undeletable/unmodifiable (400 + reason).
	// Mutations are actor-attributed (§5.11) and audit-logged via the
	// phase-5 store; the read-only method filter below blocks them. State
	// JSON is stored and served VERBATIM — relative→absolute time
	// conversion at share time is a frontend concern (§4.2).
	// ------------------------------------------------------------------
	viewSt := newRedisViewStore(rc)
	_ = seedSystemViews(context.Background(), viewSt)
	api.HandleFunc("/views", newListViewsHandlerFunc(viewSt)).Methods("GET")
	api.HandleFunc("/views", newCreateViewHandlerFunc(viewSt, jobsStore)).Methods("POST")
	api.HandleFunc("/views/{view_id}", newUpdateViewHandlerFunc(viewSt, jobsStore)).Methods("PUT")
	api.HandleFunc("/views/{view_id}", newDeleteViewHandlerFunc(viewSt, jobsStore)).Methods("DELETE")
	// --------------------------- end phase 11 --------------------------

	// ------------------------------------------------------------------
	// Fleet Console phase 15 — hygiene reports (§3.10; hygiene_handlers.go,
	// hygiene package). GETs serve the persisted store (any replica); the
	// PUT config route is read-only-blocked by the method filter below and
	// audit-logged. POST …/run is registered on the PARENT router so it
	// stays available in read-only mode — report generation only reads
	// asynq state and writes asynqmon-owned report keys — with the §5.11
	// actor middleware applied explicitly and the trigger audit-logged.
	// ------------------------------------------------------------------
	hygieneEng := opts.hygieneEngine
	if hygieneEng == nil {
		// muxRouter built directly (tests). In production New owns the
		// started, closer-managed engine and sets it before building the
		// router. NewEngine touches no Redis until Run/Start.
		var hygieneSnapshots func(ctx context.Context) (*stats.ReadResult, error)
		if statsEngine != nil {
			hygieneSnapshots = statsEngine.Read
		}
		hygieneEng = hygiene.NewEngine(hygiene.Config{
			RedisClient: rc,
			Inspector:   inspector,
			Snapshots:   hygieneSnapshots,
			WebhookURL:  opts.HygieneWebhookURL,
		})
	}
	// ------------------------------------------------------------------
	// Phase 12 — Settings › Health readout (§3.12): per-role lease holders,
	// tier status, budget, coverage, sweep cost, series-sampler status.
	// Read-only; served by any replica from the shared lease keys + the
	// stats cache. Handler lives in health_handlers.go.
	// ------------------------------------------------------------------
	api.HandleFunc("/health/roles",
		newHealthRolesHandlerFunc(rc, statsEngine, opts.errsigIndexer, hygieneEng)).Methods("GET")
	// --------------------------- end phase 12 --------------------------

	api.HandleFunc("/hygiene", newListHygieneHandlerFunc(rc)).Methods("GET")
	api.HandleFunc("/hygiene/{kind}", newGetHygieneReportHandlerFunc(rc)).Methods("GET")
	api.HandleFunc("/hygiene/{kind}/config", newPutHygieneConfigHandlerFunc(rc, jobsStore)).Methods("PUT")
	router.Handle("/api/hygiene/{kind}/run",
		newActorMiddleware(opts)(newRunHygieneHandlerFunc(hygieneEng, jobsStore))).Methods("POST")
	// --------------------------- end phase 15 --------------------------

	// ------------------------------------------------------------------
	// Fleet Console — enqueue route + capability discovery (§5.10;
	// enqueue.go / enqueue_handlers.go). The capability flag lives on its
	// own tiny GET /api/features rather than inside /api/fleet/overview:
	// overview 503s whenever the stats engine is disabled or has not
	// swept yet, and capability discovery must not depend on it (the
	// overview body is also rebuilt by the SSE broker — kept outside this
	// block's blast radius).
	// ------------------------------------------------------------------
	enqueueEnabled := opts.EnableEnqueue && !opts.ReadOnly
	api.HandleFunc("/features",
		newFeaturesHandlerFunc(enqueueEnabled, normalizeCorrelationKeys(opts.CorrelationKeys))).Methods("GET")
	if enqueueEnabled {
		ec := opts.enqueueClient
		if ec == nil {
			// muxRouter built directly (tests). In production New owns the
			// closer-managed client and sets it before building the router.
			ec = asynq.NewClient(opts.RedisConnOpt)
		}
		api.HandleFunc("/queues/{qname}/tasks", newEnqueueTaskHandlerFunc(ec, jobsStore, payloadFmt, resultFmt)).Methods("POST")
	} else {
		// Registered on the parent router, not the api subrouter: the
		// read-only method filter below would otherwise answer with a bare
		// text 405, and §5.10 wants an explanatory 403 JSON. An unmatched
		// POST falls out of the api subrouter (it has no NotFoundHandler
		// of its own) and reaches this route.
		router.HandleFunc("/api/queues/{qname}/tasks", newEnqueueDisabledHandlerFunc(opts.ReadOnly)).Methods("POST")
	}
	// --------------------------- end §5.10 -----------------------------

	// Restrict APIs when running in read-only mode.
	if opts.ReadOnly {
		api.Use(restrictToReadOnly)
	}

	// Everything else, route to uiAssetsHandler.
	router.NotFoundHandler = &uiAssetsHandler{
		rootPath:       opts.RootPath,
		contents:       staticContents,
		staticDirPath:  "ui/build",
		indexFileName:  "index.html",
		prometheusAddr: opts.PrometheusAddress,
		readOnly:       opts.ReadOnly,
	}

	return router
}

// restrictToReadOnly is a middleware function to restrict users to perform only GET requests.
func restrictToReadOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "" {
			http.Error(w, fmt.Sprintf("API Server is running in read-only mode: %s request is not allowed", r.Method), http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}
