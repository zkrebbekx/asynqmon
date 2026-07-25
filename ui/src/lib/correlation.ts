// Correlation-key extraction for the task drawer's Flow tab (build contract
// §3.5): when a payload carries a recognized correlation id, the drawer can
// reconstruct the de-facto chain of tasks sharing it via the metadata search
// path. Reuses the metadata parser so a top-level correlation id is
// recognized exactly when the metadata filter can match on it.
//
// The recognized key list is deployment-configurable (--correlation-keys,
// served by GET /api/features); DEFAULT_CORRELATION_KEYS is the fallback when
// the endpoint is absent (older backends) and mirrors the server default.

import { parseMetadata } from "./metadata";

// Default recognized keys, in priority order — the first present wins.
export const DEFAULT_CORRELATION_KEYS: readonly string[] = [
  "trace_id",
  "correlation_id",
  "request_id",
];

export interface CorrelationRef {
  key: string;
  value: string;
  // True when the id was found one level deep inside a nested object (e.g.
  // {"meta":{"trace_id":...}}). The metadata filter — client and server
  // alike — matches top-level scalar keys only, so a nested ref cannot be
  // searched as key:value; the Flow tab falls back to a free-text search on
  // the value instead.
  nested: boolean;
}

// extractCorrelation finds the first configured key present in the payload.
// All top-level hits (in key priority order) beat nested ones; nested lookup
// goes exactly one level deep, matching scalars only.
export function extractCorrelation(
  payload: string,
  keys: readonly string[] = DEFAULT_CORRELATION_KEYS
): CorrelationRef | null {
  const top = parseMetadata(payload);
  for (const key of keys) {
    const hit = top.find((p) => p.key === key && p.value !== "");
    if (hit) return { key, value: hit.value, nested: false };
  }

  // One level deep: the metadata parser deliberately skips nested objects
  // (chips/filters are top-level only), so scan the raw object here.
  let parsed: unknown;
  try {
    parsed = JSON.parse(payload);
  } catch {
    return null;
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return null;
  }
  for (const key of keys) {
    for (const v of Object.values(parsed as Record<string, unknown>)) {
      if (v === null || typeof v !== "object" || Array.isArray(v)) continue;
      const nestedVal = (v as Record<string, unknown>)[key];
      if (nestedVal === null || nestedVal === undefined || typeof nestedVal === "object") continue;
      const value = String(nestedVal);
      if (value !== "") return { key, value, nested: true };
    }
  }
  return null;
}

// ----------------------------------------------------------------------------
// Flow-tab merge helpers: the tab fires one search per task state and merges
// the results into a single time-ordered chain.
// ----------------------------------------------------------------------------

// The fields the merge needs from a search-result task (structural subset of
// api.ts TaskInfo).
export interface FlowTaskLike {
  id: string;
  queue: string;
  completed_at?: string;
  last_failed_at?: string;
}

// Timestamps the flow can order by: asynq only stores completed_at and
// last_failed_at as observed events on search results. States without one
// sort last — the caption says ordering is inferred, not causal.
export function observedAt(t: FlowTaskLike): string {
  const s = t.completed_at || t.last_failed_at || "";
  return s.startsWith("0001-") ? "" : s;
}

export function observedAtMs(t: FlowTaskLike): number {
  const s = observedAt(t);
  const ms = s ? Date.parse(s) : NaN;
  return Number.isNaN(ms) ? Number.MAX_SAFE_INTEGER : ms;
}

// mergeFlowTasks flattens the per-state result lists, dedupes by task id (a
// task that changes state between the per-state searches can appear in two
// result sets; the first occurrence wins), and sorts by observed time with
// the id as tiebreak so the chain never reshuffles between polls.
export function mergeFlowTasks<T extends FlowTaskLike>(lists: T[][]): T[] {
  const seen = new Set<string>();
  const out: T[] = [];
  for (const list of lists) {
    for (const t of list) {
      if (seen.has(t.id)) continue;
      seen.add(t.id);
      out.push(t);
    }
  }
  return out.sort(
    (a, b) => observedAtMs(a) - observedAtMs(b) || a.id.localeCompare(b.id)
  );
}

// distinctQueues counts the queues a merged flow spans (the "N tasks share
// key=value across M queues" line).
export function distinctQueues(tasks: FlowTaskLike[]): number {
  return new Set(tasks.map((t) => t.queue)).size;
}
