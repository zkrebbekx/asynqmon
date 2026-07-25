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
	// --------------------------- end phase 9 ---------------------------
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
		closers = append([]func() error{
			func() error { errIndexer.Stop(); return nil },
		}, closers...)
	}
	// --------------------------- end phase 9 ---------------------------

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

	// Fleet Console endpoints (stats cache; §5.1). These serve exclusively
	// from the sweeper's snapshot cache — never per-queue Redis reads per
	// request. The legacy GET /api/queues below stays untouched for the
	// shipping dashboard; the new directory reads /api/fleet/queues.
	api.HandleFunc("/fleet/overview", newFleetOverviewHandlerFunc(statsEngine)).Methods("GET")
	api.HandleFunc("/fleet/queues", newFleetQueuesHandlerFunc(statsEngine)).Methods("GET")
	api.HandleFunc("/fleet/attention", newFleetAttentionHandlerFunc(statsEngine)).Methods("GET")
	api.HandleFunc("/fleet/events", newFleetEventsHandlerFunc(eventsBroker)).Methods("GET")

	// Queue endpoints.
	api.HandleFunc("/queues", newListQueuesHandlerFunc(inspector)).Methods("GET")
	api.HandleFunc("/queues/{qname}", newGetQueueHandlerFunc(inspector)).Methods("GET")
	api.HandleFunc("/queues/{qname}", newDeleteQueueHandlerFunc(inspector)).Methods("DELETE")
	api.HandleFunc("/queues/{qname}:pause", newPauseQueueHandlerFunc(inspector)).Methods("POST")
	api.HandleFunc("/queues/{qname}:resume", newResumeQueueHandlerFunc(inspector)).Methods("POST")

	// ── Queue Workspace routes (Fleet Console §3.3) — registration only ──
	// Right-rail histograms: retry-ETA (exact pipelined ZCOUNT buckets on the
	// retry zset) and pending-wait (head/tail LINDEX sampling, labeled
	// "sampled, head/tail-biased"). Handlers live in queue_workspace_handlers.go.
	api.HandleFunc("/queues/{qname}/retry_histogram", newRetryHistogramHandlerFunc(rc)).Methods("GET")
	api.HandleFunc("/queues/{qname}/pending_wait_sample", newPendingWaitSampleHandlerFunc(rc)).Methods("GET")
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
	// scan_cursor; parse rejections are 400 {error, position, hint}.
	api.HandleFunc("/tasks", newSearchTasksHandlerFunc(inspector, rc, payloadFmt)).Methods("GET")
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
