import { useEffect, useState } from "react";
import * as api from "../api";
import { stringifyDurationMs } from "../utils";

// ObservedRunStamp: "last run 3.4s · observed" for a finished task's table
// row, sourced from the OPT-IN observe middleware's summary. Renders NOTHING
// when no data exists — absence is the expected case until workers adopt the
// middleware, and the tables must not grow an empty column for it.
//
// Finished tasks' summaries are effectively immutable, so results are cached
// module-wide (short TTL) — table polling re-renders rows every few seconds
// and must not refetch per tick.

const cache = new Map<string, { at: number; value: api.ObservedTaskResponse | null }>();
const CACHE_TTL_MS = 60_000;

function fresh(key: string) {
  const hit = cache.get(key);
  return hit && Date.now() - hit.at < CACHE_TTL_MS ? hit : undefined;
}

// Test hook: lets vitest clear the module cache between cases.
export function resetObservedCache() {
  cache.clear();
}

export default function ObservedRunStamp({ queue, id }: { queue: string; id: string }) {
  const key = `${queue}/${id}`;
  const [obs, setObs] = useState<api.ObservedTaskResponse | null>(
    () => fresh(key)?.value ?? null
  );

  useEffect(() => {
    const hit = fresh(key);
    if (hit) {
      setObs(hit.value);
      return;
    }
    let cancelled = false;
    api
      .getObservedTask(queue, id)
      .then((value) => {
        cache.set(key, { at: Date.now(), value });
        if (!cancelled) setObs(value);
      })
      .catch(() => {
        // Best-effort optional data: negative-cache transient failures too,
        // so a down backend isn't hammered once per row per poll tick.
        cache.set(key, { at: Date.now(), value: null });
        if (!cancelled) setObs(null);
      });
    return () => {
      cancelled = true;
    };
  }, [key, queue, id]);

  if (!obs?.present || !obs.summary) return null;
  return (
    <div className="whitespace-nowrap text-[11px] text-[hsl(var(--muted-foreground))]">
      last run {stringifyDurationMs(obs.summary.last_duration_ms)} · observed
    </div>
  );
}
