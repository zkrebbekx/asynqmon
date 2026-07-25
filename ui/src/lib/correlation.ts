// Correlation-key extraction for the task drawer's Flow tab (build contract
// §3.5): when a payload carries a recognized correlation id, the drawer can
// reconstruct the de-facto chain of tasks sharing it via the metadata search
// path. Reuses the metadata parser so a correlation id is recognized exactly
// when the metadata filter can match on it.

import { parseMetadata } from "./metadata";

// Recognized keys, in priority order — the first present wins.
export const CORRELATION_KEYS = [
  "trace_id",
  "correlation_id",
  "request_id",
] as const;
export type CorrelationKey = (typeof CORRELATION_KEYS)[number];

export interface CorrelationRef {
  key: CorrelationKey;
  value: string;
}

export function extractCorrelation(payload: string): CorrelationRef | null {
  const pairs = parseMetadata(payload);
  for (const key of CORRELATION_KEYS) {
    const hit = pairs.find((p) => p.key === key && p.value !== "");
    if (hit) return { key, value: hit.value };
  }
  return null;
}
