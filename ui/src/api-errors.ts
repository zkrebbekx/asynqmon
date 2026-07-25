// Typed fetchers for the Errors screen endpoints (build contract §3.6/§5.7,
// phase 9). Response shapes are the frozen backend contracts
// (errsig_handlers.go). Served from the shared error-signature store: the
// archived tail is exact, retry contributions are sampled, and counts
// sourced from archive-trimmed queues are lower bounds — the honesty fields
// carry that, and the UI must render it, never hide it.

import axios from "axios";
import queryString from "query-string";

const getBaseUrl = () =>
  import.meta.env.PROD
    ? `${window.ROOT_PATH}/api`
    : `${window.location.origin}${window.ROOT_PATH}/api`;

export type SignatureTrend = "new" | "growing" | "steady" | "recovering";

export interface ErrorSignatureQueueCell {
  queue: string;
  type: string;
  count: number; // archived + retry_sampled
  archived: number; // exact (archived tail)
  retry_sampled: number; // last resample, replaced wholesale each run
  sampled: boolean;
}

export interface ErrorSignatureTaskRef {
  queue: string;
  id: string;
}

export interface ErrorSignatureRow {
  sig: string;
  template: string;
  count: number;
  // True when any source queue's archive sits at the 10k trim cap — render
  // the count with a "≥" prefix, never as exact.
  count_is_lower_bound: boolean;
  archived_count: number;
  retry_sampled_count: number;
  first_seen: string; // RFC3339; "" = unknown
  last_seen: string;
  trend: SignatureTrend;
  queues: ErrorSignatureQueueCell[];
  sample_task_refs: ErrorSignatureTaskRef[];
}

export interface ErrorSignatureHonesty {
  archived_exact: boolean;
  retry_sampled: boolean;
  trimmed_queues: string[];
}

export interface ListErrorSignaturesResponse {
  signatures: ErrorSignatureRow[];
  total_signatures: number;
  honesty: ErrorSignatureHonesty;
  // Counter-derived magnitude (fleet failed-counter sum at the last tail
  // sweep — never trimmed) beside the index-derived identity total.
  counter_failed_today: number;
  index_count_total: number;
  tail_indexed_at: string; // "" until the indexer's first sweep
  retry_sampled_at: string;
}

export interface ErrorSignatureCounterRow {
  queue: string;
  failed: number;
}

export interface GetErrorSignatureResponse {
  signature: ErrorSignatureRow;
  honesty: ErrorSignatureHonesty;
  // Live daily failed counters for the signature's source queues. These
  // count ALL failures in those queues, not only this signature's — the UI
  // labels them so.
  failed_today_by_queue: ErrorSignatureCounterRow[];
  failed_today_queues_total: number;
  counter_failed_today: number;
  index_count_total: number;
  tail_indexed_at: string;
  retry_sampled_at: string;
  // False until ring buffers land (phase 10) — the UI renders the counter
  // cross-check instead of a fabricated chart.
  count_over_time_available: boolean;
}

export async function listErrorSignatures(query: {
  limit?: number;
  sort?: "last_seen" | "count";
}): Promise<ListErrorSignaturesResponse> {
  const url = queryString.stringifyUrl(
    { url: `${getBaseUrl()}/errors/signatures`, query: { ...query } },
    { skipEmptyString: true }
  );
  const resp = await axios({ method: "get", url });
  return resp.data;
}

export async function getErrorSignature(
  sig: string
): Promise<GetErrorSignatureResponse> {
  const resp = await axios({
    method: "get",
    url: `${getBaseUrl()}/errors/signatures/${sig}`,
  });
  return resp.data;
}
