// useJobsEvents — one live bulk-job feed per consumer, fed by the `jobs`
// events of the SSE stream GET /api/fleet/events.
//
// Event contract (frozen with the backend, fleet_events_handlers.go):
//   event: jobs — data = {"jobs": [<JobInfo JSON>, ...]}
// The on-connect burst carries the current NON-TERMINAL jobs snapshot; every
// later event carries the one job that just progressed or transitioned
// (progress ticks are throttled server-side to ≤1/job/sec; state transitions
// are always immediate).
//
// Same resilience contract as useFleetEvents: the stream may 404/503 or drop
// mid-incident — consumers must keep working on their poll fallbacks, and
// the stream is retried with exponential backoff. Unlike the fleet
// aggregates, this feed is NOT gated on the live-updates pause pill: it
// tracks operations the operator explicitly started (the same rule as the
// Ops screen's tight progress polling).

import { useEffect, useState } from "react";
import { JobInfo } from "../api";
import { fleetEventsUrl } from "../api-fleet";
import { isTerminalJobState } from "../lib/bulkjob";

export interface JobsEventsSnapshot {
  // Latest received state per job id (snapshot burst + incremental ticks).
  jobs: Record<string, JobInfo>;
  source: "sse" | "poll";
  // Epoch ms of the last received `jobs` event; 0 until the first one.
  lastEventAt: number;
}

const INITIAL_BACKOFF_MS = 5_000;
const MAX_BACKOFF_MS = 60_000;

export function useJobsEvents(enabled: boolean): JobsEventsSnapshot {
  const [jobs, setJobs] = useState<Record<string, JobInfo>>({});
  const [source, setSource] = useState<"sse" | "poll">("poll");
  const [lastEventAt, setLastEventAt] = useState(0);

  useEffect(() => {
    if (!enabled) return;
    let disposed = false;
    let es: EventSource | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let backoff = INITIAL_BACKOFF_MS;

    const onJobs = (e: MessageEvent) => {
      if (disposed) return;
      backoff = INITIAL_BACKOFF_MS;
      let incoming: JobInfo[];
      try {
        incoming = (JSON.parse(e.data) as { jobs?: JobInfo[] }).jobs ?? [];
      } catch {
        return; // malformed frame — keep the last good state
      }
      setSource("sse");
      setLastEventAt(Date.now());
      if (incoming.length === 0) return;
      setJobs((prev) => {
        const next = { ...prev };
        for (const j of incoming) {
          if (!j?.id) continue;
          // Never regress a terminal job to a non-terminal state (a late
          // progress tick racing the terminal event).
          const cur = next[j.id];
          if (cur && isTerminalJobState(cur.state) && !isTerminalJobState(j.state)) {
            continue;
          }
          next[j.id] = j;
        }
        return next;
      });
    };

    const openStream = () => {
      if (disposed || typeof EventSource === "undefined") return;
      es = new EventSource(fleetEventsUrl());
      es.addEventListener("jobs", onJobs);
      es.onerror = () => {
        es?.close();
        es = null;
        if (disposed) return;
        setSource("poll");
        reconnectTimer = setTimeout(openStream, backoff);
        backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
      };
    };

    openStream();
    return () => {
      disposed = true;
      if (es !== null) es.close();
      if (reconnectTimer !== null) clearTimeout(reconnectTimer);
    };
  }, [enabled]);

  return { jobs, source, lastEventAt };
}
