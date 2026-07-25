// Capability discovery for gated features (§5.10). The probe result is
// cached at module level: every drawer/modal shares one GET /api/features
// per page load instead of refetching on each mount.

import { useEffect, useState } from "react";
import * as api from "../api";

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
