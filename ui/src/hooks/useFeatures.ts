// Capability discovery for gated features (§5.10). The probe result is
// cached at module level: every drawer/modal shares one GET /api/features
// per page load instead of refetching on each mount.

import { useEffect, useState } from "react";
import * as api from "../api";
import { DEFAULT_CORRELATION_KEYS } from "../lib/correlation";

let cached: Promise<api.FeaturesResponse> | null = null;

// resetFeaturesCache exists for tests (the module-level cache would leak
// between cases otherwise).
export function resetFeaturesCache() {
  cached = null;
}

// useEnqueueEnabled reports whether this deployment allows creating tasks
// from the UI. False while loading, on probe failure, and always in a
// read-only build — the clone-and-edit action hides entirely (§5.10).
export function useEnqueueEnabled(): boolean {
  const [enabled, setEnabled] = useState(false);
  useEffect(() => {
    if (window.READ_ONLY) return;
    let alive = true;
    if (!cached) cached = api.getFeatures();
    cached
      .then((f) => {
        if (alive) setEnabled(!!f.features?.enqueue);
      })
      .catch(() => {
        // Older backends without /api/features: feature stays hidden.
        cached = null;
        if (alive) setEnabled(false);
      });
    return () => {
      alive = false;
    };
  }, []);
  return enabled;
}

// usePayloadDetailLimit returns the character cap the backend's task DETAIL
// endpoint applies to formatted payloads/results (--max-detail-payload-length,
// upstream hibiken/asynqmon#301), or 0 when unlimited/unknown (older
// backends, probe failure). Probes in read-only mode too — reading a payload
// is a read-only feature.
export function usePayloadDetailLimit(): number {
  const [limit, setLimit] = useState(0);
  useEffect(() => {
    let alive = true;
    if (!cached) cached = api.getFeatures();
    cached
      .then((f) => {
        if (alive && typeof f.payload_detail_limit === "number" && f.payload_detail_limit > 0) {
          setLimit(f.payload_detail_limit);
        }
      })
      .catch(() => {
        // Older backends without /api/features: no cap knowledge — no note.
        cached = null;
      });
    return () => {
      alive = false;
    };
  }, []);
  return limit;
}

// useCorrelationKeys returns the payload keys this deployment's Flow view
// recognizes as correlation ids (--correlation-keys, §3.5), in priority
// order. Defaults apply while loading, on probe failure, and against older
// backends whose /api/features omits the field. Unlike useEnqueueEnabled it
// probes in read-only mode too — the Flow view is a read-only feature.
export function useCorrelationKeys(): readonly string[] {
  const [keys, setKeys] = useState<readonly string[]>(DEFAULT_CORRELATION_KEYS);
  useEffect(() => {
    let alive = true;
    if (!cached) cached = api.getFeatures();
    cached
      .then((f) => {
        if (alive && Array.isArray(f.correlation_keys) && f.correlation_keys.length > 0) {
          setKeys(f.correlation_keys);
        }
      })
      .catch(() => {
        // Older backends without /api/features: keep the defaults.
        cached = null;
      });
    return () => {
      alive = false;
    };
  }, []);
  return keys;
}
