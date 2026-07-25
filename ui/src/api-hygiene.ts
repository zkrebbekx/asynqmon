// Typed fetchers for the Hygiene report endpoints (build contract §3.10,
// phase 15). Response shapes are the frozen backend contracts
// (hygiene_handlers.go + the hygiene package's report types). Reports are
// generated server-side (leased scheduler or Run-now) and persisted; these
// calls only read/trigger — the browser never computes a report.

import axios from "axios";

// Same base-URL convention as api.ts / api-fleet.ts.
const getBaseUrl = () =>
  import.meta.env.PROD
    ? `${window.ROOT_PATH}/api`
    : `${window.location.origin}${window.ROOT_PATH}/api`;

export type HygieneKind =
  | "inventory"
  | "storage"
  | "dead-letter"
  | "scheduler-health";

/**************************************************************
                      GET /api/hygiene
 **************************************************************/

export interface HygieneCard {
  kind: HygieneKind;
  title: string;
  enabled: boolean;
  interval_seconds: number;
  // RFC3339; "" when the report was never generated.
  last_generated_at: string;
  // Card chips from the last report; null when never generated.
  summary: Record<string, number> | null;
}

export interface ListHygieneResponse {
  reports: HygieneCard[];
}

export async function listHygiene(): Promise<ListHygieneResponse> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/hygiene` });
  return resp.data;
}

/**************************************************************
                  GET /api/hygiene/{kind}
 **************************************************************/

// -- Queue inventory --------------------------------------------------------

export type ActivitySource =
  | "active_now"
  | "counters_today"
  | "oldest_pending"
  | "series"
  | "daily_counters"
  | "none"
  | "unknown";

export interface InventoryRow {
  queue: string;
  size: number;
  pending: number;
  consumers: number;
  paused: boolean;
  paused_since?: string;
  last_activity_at?: string;
  last_activity_source: ActivitySource;
  dead_candidate: boolean;
  dead_evidence?: string;
  zero_consumer_pending: boolean;
  paused_over_7d: boolean;
}

export interface InventoryReport {
  rows: InventoryRow[];
  queues_total: number;
  rows_truncated: boolean;
  probe_cap: number;
  probes_capped: boolean;
}

// -- Storage attribution ----------------------------------------------------

export interface QueueMemoryRow {
  queue: string;
  tasks: number;
  struct_bytes: number;
  sampled_tasks: number;
  avg_task_bytes: number;
  estimated_bytes: number;
  sampled_at: string;
}

export interface PayloadOffender {
  queue: string;
  id: string;
  state: string;
  msg_bytes: number;
}

export interface RetentionRow {
  queue: string;
  counts: number[];
  total: number;
}

export interface RetentionHistogram {
  bucket_labels: string[];
  fleet: RetentionRow;
  queues: RetentionRow[];
  queues_truncated: boolean;
}

export interface StorageReport {
  // Rows exist ONLY for sampled queues; a missing queue means "not sampled"
  // and the UI must say so, never render 0.
  memory: QueueMemoryRow[];
  not_sampled_queues: number;
  sample_per_queue: number;
  method: string; // honest sampling label — render verbatim
  offenders: PayloadOffender[];
  retention: RetentionHistogram;
}

// -- Dead-letter digest -----------------------------------------------------

export interface DeadLetterCluster {
  sig: string;
  template: string;
  archived: number;
  lower_bound: boolean;
}

export interface DeadLetterAction {
  label: string;
  verb: string; // "delete"
  queue: string;
  state: string; // "archived"
  aql: string; // "died>30d"
  estimate: number;
}

export interface DeadLetterQueueRow {
  queue: string;
  archived: number;
  buckets: number[];
  older_than_threshold: number;
  at_trim_cap: boolean;
  clusters: DeadLetterCluster[];
  action?: DeadLetterAction;
}

export interface DeadLetterReport {
  bucket_labels: string[];
  action_threshold_days: number;
  queues: DeadLetterQueueRow[];
  queues_with_archived: number;
  queues_truncated: boolean;
  clusters_indexed_at?: string;
}

// -- Scheduler health -------------------------------------------------------

export interface SchedIssue {
  stable_key: string;
  task_type: string;
  spec: string;
  queue: string;
  last_seen_at?: string;
  retention_seconds: number;
  consumers: number;
}

export interface SchedulerHealthReport {
  gone: SchedIssue[];
  retention_less: SchedIssue[];
  zero_consumer_targets: SchedIssue[];
  live_entries: number;
  snapshots: number;
}

// -- Envelope ---------------------------------------------------------------

export interface HygieneReport {
  kind: HygieneKind;
  generated_at: string;
  // Stamps the stats snapshot the report was computed from; absent for
  // scheduler-health (needs no snapshot).
  data_as_of?: string;
  summary: Record<string, number>;
  inventory?: InventoryReport;
  storage?: StorageReport;
  dead_letter?: DeadLetterReport;
  scheduler_health?: SchedulerHealthReport;
}

export async function getHygieneReport(kind: HygieneKind): Promise<HygieneReport> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/hygiene/${kind}` });
  return resp.data;
}

/**************************************************************
                POST /api/hygiene/{kind}/run
 **************************************************************/

// Generates the report NOW (synchronous, bounded reads) and returns it.
// Available in read-only mode — reports are reads; the trigger is
// audit-logged server-side.
export async function runHygieneReport(kind: HygieneKind): Promise<HygieneReport> {
  const resp = await axios({ method: "post", url: `${getBaseUrl()}/hygiene/${kind}/run` });
  return resp.data;
}

/**************************************************************
                PUT /api/hygiene/{kind}/config
 **************************************************************/

export interface HygieneConfig {
  enabled: boolean;
  interval_seconds: number;
}

export async function putHygieneConfig(
  kind: HygieneKind,
  config: HygieneConfig
): Promise<HygieneConfig & { kind: HygieneKind }> {
  const resp = await axios({
    method: "put",
    url: `${getBaseUrl()}/hygiene/${kind}/config`,
    data: config,
  });
  return resp.data;
}
