// useJobsEvents — ONE shared live bulk-job feed per tab, fed by the `jobs`
// events of the SSE stream GET /api/fleet/events.
//
// Event contract (frozen with the backend, fleet_events_handlers.go):
//   event: jobs — data = {"jobs": [<JobInfo JSON>, ...]}
// The on-connect burst carries the current NON-TERMINAL jobs snapshot; every
// later event carries the one job that just progressed or transitioned
// (progress ticks are throttled server-side to ≤1/job/sec; state transitions
// are always immediate).
//
// Connection model: a module-level refcounted singleton. Every enabled
// consumer used to open its own EventSource — the Ops page plus an open bulk
// modal plus the fleet stream held 3+ SSE connections per tab, and HTTP/1.1's
// 6-connections-per-host cap started starving normal API traffic. Now the
// first enabled consumer opens the stream, later ones share it, and the last
// one closing tears it down (and resets the cache, so remounts and tests
// start clean).
//
// Same resilience contract as useFleetEvents: the stream may 404/503 or drop
// mid-incident — consumers must keep working on their poll fallbacks, and
// the stream is retried with exponential backoff.
//
// Liveness: the server emits `event: ping` every 15 s. A watchdog counts
// every frame (jobs and ping) and, after 45 s of silence, publishes
// source "poll" and reconnects. Without it a half-open TCP connection (VPN
// drop, proxy idle kill) fires no `error` event for minutes and the
// consumers that stand their polls down while `source === "sse"` froze
// mid-progress. Unlike the fleet
// aggregates, this feed is NOT gated on the live-updates pause pill: it
// tracks operations the operator explicitly started (the same rule as the
// Ops screen's tight progress polling).

import { useEffect, useSyncExternalStore } from "react";
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
// Three missed server pings (15 s cadence) mean the connection is dead.
export const WATCHDOG_MS = 45_000;
const WATCHDOG_TICK_MS = 5_000;

const INITIAL_SNAPSHOT: JobsEventsSnapshot = { jobs: {}, source: "poll", lastEventAt: 0 };

// ---- module-level shared connection ----------------------------------------

let snapshot: JobsEventsSnapshot = INITIAL_SNAPSHOT;
let es: EventSource | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let backoff = INITIAL_BACKOFF_MS;
let refs = 0;
let watchdogTimer: ReturnType<typeof setInterval> | null = null;
// Epoch ms of the last frame of ANY kind on the open stream (jobs or ping);
// the connect time until the first frame.
let lastAnyEventAt = 0;
const listeners = new Set<() => void>();

function publish(next: JobsEventsSnapshot) {
  snapshot = next;
  for (const l of listeners) l();
}

function onPing() {
  lastAnyEventAt = Date.now();
}

function onJobs(e: MessageEvent) {
  backoff = INITIAL_BACKOFF_MS;
  lastAnyEventAt = Date.now();
  let incoming: JobInfo[];
  try {
    incoming = (JSON.parse(e.data) as { jobs?: JobInfo[] }).jobs ?? [];
  } catch {
    return; // malformed frame — keep the last good state
  }
  const jobs = { ...snapshot.jobs };
  for (const j of incoming) {
    if (!j?.id) continue;
    // Never regress a terminal job to a non-terminal state (a late
    // progress tick racing the terminal event).
    const cur = jobs[j.id];
    if (cur && isTerminalJobState(cur.state) && !isTerminalJobState(j.state)) {
      continue;
    }
    jobs[j.id] = j;
  }
  publish({ jobs, source: "sse", lastEventAt: Date.now() });
}

// closeStream tears the connection down. When `retry` is set and a consumer
// is still attached, it demotes the consumers to polling and schedules a
// reconnect with backoff.
function closeStream(retry: boolean) {
  es?.close();
  es = null;
  if (watchdogTimer !== null) {
    clearInterval(watchdogTimer);
    watchdogTimer = null;
  }
  if (!retry || refs === 0) return;
  publish({ ...snapshot, source: "poll" });
  if (reconnectTimer !== null) clearTimeout(reconnectTimer);
  reconnectTimer = setTimeout(openStream, backoff);
  backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
}

function watchdogCheck() {
  if (es === null) return;
  if (Date.now() - lastAnyEventAt > WATCHDOG_MS) closeStream(true);
}

function openStream() {
  if (refs === 0 || es !== null || typeof EventSource === "undefined") return;
  reconnectTimer = null;
  es = new EventSource(fleetEventsUrl());
  lastAnyEventAt = Date.now();
  es.addEventListener("jobs", onJobs);
  es.addEventListener("ping", onPing);
  es.onerror = () => closeStream(true);
  watchdogTimer = setInterval(watchdogCheck, WATCHDOG_TICK_MS);
}

function acquire(): () => void {
  refs++;
  openStream();
  let released = false;
  return () => {
    if (released) return;
    released = true;
    refs--;
    if (refs > 0) return;
    closeStream(false);
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    backoff = INITIAL_BACKOFF_MS;
    lastAnyEventAt = 0;
    snapshot = INITIAL_SNAPSHOT; // remounts/tests start clean
  };
}

function subscribeStore(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

function getSnapshot(): JobsEventsSnapshot {
  return snapshot;
}

// ---- hook ------------------------------------------------------------------

export function useJobsEvents(enabled: boolean): JobsEventsSnapshot {
  useEffect(() => {
    if (!enabled) return;
    return acquire();
  }, [enabled]);
  const snap = useSyncExternalStore(subscribeStore, getSnapshot);
  return enabled ? snap : INITIAL_SNAPSHOT;
}
