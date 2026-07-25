// Typed fetchers for the Fleet Console aggregate endpoints (build contract
// §5.1/§5.2). These are served from the server-side stats cache — the browser
// never enumerates queues itself. Response shapes are the frozen phase-2/4
// backend contracts; every consumer must tolerate the endpoints being absent
// (404/503) while the backend lands, so callers surface errors instead of
// assuming data.

import axios from "axios";
import queryString from "query-string";

import type { SchedulerEntry } from "./api";

// Same base-URL convention as api.ts: production serves the API on the same
// origin; development proxies /api via vite.
const getBaseUrl = () =>
  import.meta.env.PROD
    ? `${window.ROOT_PATH}/api`
    : `${window.location.origin}${window.ROOT_PATH}/api`;

/**************************************************************
                    GET /api/fleet/overview
 **************************************************************/

export interface FleetStats {
  queues_total: number;
  paused_queues: number;
  zero_consumer_queues: number;
  pending: number;
  active: number;
  scheduled: number;
  retry: number;
  archived: number;
  completed: number;
  groups: number;
  orphan_candidates: number;
  past_due_scheduled: number;
  processed_today: number;
  failed_today: number;
  error_rate: number;
  servers: number;
  workers_total: number;
  workers_busy: number;
  redis_memory_used_bytes: number;
  redis_memory_max_bytes: number;
}

// Coverage stamp (honest-numbers rule §2): which fraction of the fleet the
// totals actually cover, and when the cache was refreshed.
export interface FleetCoverage {
  tier: number;
  queues_total: number;
  refreshed_pct_5m: number;
  updated_at: string; // RFC3339
}

export interface FleetOverviewResponse {
  fleet: FleetStats;
  coverage: FleetCoverage;
}

export async function getFleetOverview(): Promise<FleetOverviewResponse> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/fleet/overview` });
  return resp.data;
}

/**************************************************************
                    GET /api/fleet/queues
 **************************************************************/

export interface FleetQueueRow {
  queue: string;
  pending: number;
  active: number;
  scheduled: number;
  retry: number;
  archived: number;
  completed: number;
  groups: number;
  paused: boolean;
  paused_since?: string;
  processed_today: number;
  failed_today: number;
  error_rate: number;
  orphan_candidates: number;
  next_retry_at?: string;
  past_due_scheduled: number;
  oldest_pending_since?: string;
  oldest_pending_age_ms: number;
  latency_ms: number;
  consumers: number;
  refreshed_at: string; // RFC3339 — drives the staleness dot
}

export interface FleetQueuesQuery {
  sort?: string;
  dir?: "asc" | "desc";
  cursor?: string;
  limit?: number;
  f?: string; // queue filter expression, passed through verbatim
}

export interface FleetQueuesResponse {
  queues: FleetQueueRow[];
  total_matched: number;
  next_cursor?: string;
  // Set when the server could not apply (part of) the filter; the UI renders
  // it as a dismissable note instead of silently showing unfiltered rows.
  filter_ignored?: string;
  coverage: FleetCoverage;
}

export async function listFleetQueues(
  query: FleetQueuesQuery
): Promise<FleetQueuesResponse> {
  const url = queryString.stringifyUrl(
    { url: `${getBaseUrl()}/fleet/queues`, query: { ...query } },
    { skipEmptyString: true }
  );
  const resp = await axios({ method: "get", url });
  return resp.data;
}

/**************************************************************
                    GET /api/fleet/attention
 **************************************************************/

export interface AttentionFinding {
  queue: string;
  detector: string;
  // 1 (hygiene) … 5 (work is not running) — see stats/attention.go.
  severity: number;
  sentence: string;
  // The finding's key number: a count for count detectors, whole seconds for
  // age detectors (PENDING_AGE, PAUSED_LONG, GROUP_STALL).
  value: number;
  chips: string[];
  since: string; // RFC3339
  // The query the system wrote for you — wired into /tasks or /queues.
  suggested_query: string;
}

export interface FleetAttentionResponse {
  findings: AttentionFinding[];
  updated_at: string;
  detectors_live: number;
  detectors_learning: number;
}

export async function getFleetAttention(): Promise<FleetAttentionResponse> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/fleet/attention` });
  return resp.data;
}

/**************************************************************
                      GET /api/coverage
 **************************************************************/

// Workers screen (§3.7): the queues × consuming-servers join (§5.5).
export type CoverageStatus = "ok" | "uncovered" | "starved";

export interface CoverageConsumer {
  server_id: string;
  host: string;
  pid: number;
  weight: number;
  strict_priority: boolean;
}

export interface CoverageRow {
  queue: string;
  // Pending depth from the stats cache; absent when the cache is
  // unavailable — absent data is not zero.
  pending?: number;
  consumers: CoverageConsumer[];
  // Upper bound of worker slots that could serve this queue (each server's
  // slots are shared across its whole queue map).
  total_concurrency: number;
  status: CoverageStatus;
}

export interface CoverageResponse {
  rows: CoverageRow[];
  updated_at: string; // RFC3339
}

export async function getCoverage(): Promise<CoverageResponse> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/coverage` });
  return resp.data;
}

/**************************************************************
                  GET /api/cancel-listeners
 **************************************************************/

// Cancel deliverability (§5.14): PUBSUB NUMSUB on asynq's cancelation
// channel ("asynq:cancel", asynq internal/base). approximate=true on Redis
// Cluster where the count is summed across reachable nodes.
export interface CancelListenersResponse {
  listeners: number;
  approximate: boolean;
  nodes: number;
}

export async function getCancelListeners(): Promise<CancelListenersResponse> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/cancel-listeners` });
  return resp.data;
}

/**************************************************************
              GET /api/schedulers (§3.8/§5.12)
 **************************************************************/

// One merged Schedulers row: live entries ∪ persisted snapshots, keyed by the
// stable entry identity hash(spec|type|payload|opts) so history survives
// scheduler restarts (asynq entry IDs are fresh UUIDs per process).
export interface SchedulerRow {
  stable_key: string;
  // The entry in the legacy /scheduler_entries shape.
  entry: SchedulerEntry;
  live: boolean;
  // Snapshot stamps (RFC3339); "" while the entry is live but the sweeper has
  // not persisted it yet — absent data is not "now".
  first_seen: string;
  last_seen: string;
  // Present only when the entry is GONE: no live counterpart and last_seen
  // older than the server's gone threshold. Equals last_seen.
  gone_since?: string;
}

export interface SchedulersResponse {
  entries: SchedulerRow[];
}

export async function listSchedulers(): Promise<SchedulersResponse> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/schedulers` });
  return resp.data;
}

/**************************************************************
        GET /api/schedulers/:stable_key/outcomes (§3.8)
 **************************************************************/

// Three-valued honestly (§3.8): succeeded = completed-state task found;
// failed = archived, or retry-state with retries > 0; pending = still in
// flight; unknown = task no longer in Redis (deleted, or Retention=0 erased
// the completion).
export type SchedulerOutcome = "succeeded" | "failed" | "pending" | "unknown";

export interface SchedulerOutcomeEvent {
  task_id: string;
  enqueued_at: string; // RFC3339
  outcome: SchedulerOutcome;
  // Raw asynq task state when the task was found.
  state?: string;
  // Why an outcome is unknown ("not_found — task deleted or Retention=0").
  reason?: string;
}

export interface SchedulerOutcomesResponse {
  stable_key: string;
  queue: string;
  // The entry's asynq.Retention in whole seconds; 0 = completions are deleted
  // immediately and success is unknowable — drives the §3.8 guidance callout.
  retention_seconds: number;
  // asynq's hard cap on per-entry enqueue history (1,000) — label it.
  history_cap: number;
  events: SchedulerOutcomeEvent[];
}

export async function getSchedulerOutcomes(
  stableKey: string
): Promise<SchedulerOutcomesResponse> {
  const resp = await axios({
    method: "get",
    url: `${getBaseUrl()}/schedulers/${stableKey}/outcomes`,
  });
  return resp.data;
}

/**************************************************************
        GET /api/queues/:qname/retry_histogram (§3.3(c))
 **************************************************************/

// Retry-ETA histogram — "when does the storm land". EXACT: pipelined ZCOUNT
// windows over the retry zset's next_process_at scores, never a sample.
export interface RetryHistogramResponse {
  queue: string;
  from: string; // RFC3339; bucket i covers [from + i*w, from + (i+1)*w)
  bucket_seconds: number;
  past_due: number; // next_process_at already behind now
  buckets: number[];
  beyond: number; // fires after the last bucket's end
  total: number;
  exact: boolean;
}

export async function getRetryHistogram(
  qname: string,
  opts?: { window?: number; buckets?: number }
): Promise<RetryHistogramResponse> {
  const usp = new URLSearchParams();
  if (opts?.window) usp.set("window", String(opts.window));
  if (opts?.buckets) usp.set("buckets", String(opts.buckets));
  const qs = usp.toString();
  const resp = await axios({
    method: "get",
    url: `${getBaseUrl()}/queues/${qname}/retry_histogram${qs ? `?${qs}` : ""}`,
  });
  return resp.data;
}

/**************************************************************
      GET /api/queues/:qname/pending_wait_sample (§3.3(c))
 **************************************************************/

// Pending-wait sample — NOT uniform and labeled so: pending is a Redis LIST
// (LINDEX is O(index) per probe), so probes come from the head/tail bands and
// `method` carries the "sampled, head/tail-biased" caption verbatim.
export interface PendingWaitSampleResponse {
  queue: string;
  list_len: number;
  head_probes: number;
  tail_probes: number;
  sampled: number;
  waits_ms: number[];
  method: string; // "sampled, head/tail-biased" — render verbatim
  sampled_at: string; // RFC3339
}

export async function getPendingWaitSample(
  qname: string,
  probes?: number
): Promise<PendingWaitSampleResponse> {
  const qs = probes ? `?probes=${probes}` : "";
  const resp = await axios({
    method: "get",
    url: `${getBaseUrl()}/queues/${qname}/pending_wait_sample${qs}`,
  });
  return resp.data;
}

/**************************************************************
                    SSE GET /api/fleet/events
 **************************************************************/

// Event stream carrying `overview` and `attention` events whose data payloads
// are the same JSON bodies as the GET endpoints above. Consumed by
// useFleetEvents, which falls back to polling the GETs when the stream errors.
export function fleetEventsUrl(): string {
  return `${getBaseUrl()}/fleet/events`;
}
