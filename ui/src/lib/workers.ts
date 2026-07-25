// Pure helpers for the Workers screen (build contract §3.7): worker
// elapsed-vs-deadline budgets, the fleet-wide stuck-worker ranking, coverage
// chip mapping, and the /tasks drawer deep link for an active task. Pure
// functions over already-fetched payloads so all of it is unit-testable
// without a router or a clock.

import { ServerInfo, WorkerInfo } from "../api";
import { CoverageRow, CoverageStatus } from "../api-fleet";
import { FcTone } from "../components/FleetBits";
import {
  DEFAULT_CONSOLE_STATE,
  serializeConsoleState,
  serializePeek,
} from "./urlstate";

/**************************************************************
                Elapsed-vs-deadline budget (§3.7)
 **************************************************************/

export interface WorkerBudget {
  // Milliseconds the worker has been running; null when start_time is
  // missing/unparsable.
  elapsedMs: number | null;
  // Total budget (deadline − start); null when the deadline is missing,
  // unparsable, or not after the start (no honest bar can be drawn).
  budgetMs: number | null;
  // elapsed / budget. May exceed 1 (past deadline) — the caller clamps the
  // bar, not the number. null when either input is unusable.
  pct: number | null;
}

// parseWireTime parses an RFC3339 wire value, rejecting empty strings and
// the zero-time sentinel ("0001-01-01…" parses to a huge negative epoch).
function parseWireTime(raw: string | undefined | null): number | null {
  if (!raw) return null;
  const t = Date.parse(raw);
  if (!Number.isFinite(t) || t <= 0) return null;
  return t;
}

export function workerBudget(
  startTime: string | undefined | null,
  deadline: string | undefined | null,
  nowMs: number
): WorkerBudget {
  const start = parseWireTime(startTime);
  if (start === null) return { elapsedMs: null, budgetMs: null, pct: null };
  const elapsedMs = Math.max(0, nowMs - start);
  const end = parseWireTime(deadline);
  if (end === null || end <= start) return { elapsedMs, budgetMs: null, pct: null };
  const budgetMs = end - start;
  return { elapsedMs, budgetMs, pct: elapsedMs / budgetMs };
}

// budgetTone maps %-of-budget onto severity: ≥90% crit, ≥75% warn (the same
// thresholds as the Fleet landing's Redis gauge). null pct → no tone.
export function budgetTone(pct: number | null): "crit" | "warn" | null {
  if (pct === null || !Number.isFinite(pct)) return null;
  if (pct >= 0.9) return "crit";
  if (pct >= 0.75) return "warn";
  return null;
}

/**************************************************************
              Stuck-worker ranking (fleet-wide, §3.7)
 **************************************************************/

export interface StuckWorker {
  serverId: string;
  host: string;
  pid: number;
  worker: WorkerInfo;
  elapsedMs: number;
  budgetMs: number;
  pct: number;
}

// stuckWorkers flattens every active worker across the fleet, keeps those
// with a computable budget (a worker without a deadline cannot honestly rank
// on %-of-budget), and returns the top `limit` sorted worst-first.
// Ties break by longer elapsed time, then task id for determinism.
export function stuckWorkers(
  servers: ServerInfo[],
  nowMs: number,
  limit = 10
): StuckWorker[] {
  const out: StuckWorker[] = [];
  for (const s of servers) {
    for (const w of s.active_workers) {
      const b = workerBudget(w.start_time, w.deadline, nowMs);
      if (b.pct === null || b.budgetMs === null || b.elapsedMs === null) continue;
      out.push({
        serverId: s.id,
        host: s.host,
        pid: s.pid,
        worker: w,
        elapsedMs: b.elapsedMs,
        budgetMs: b.budgetMs,
        pct: b.pct,
      });
    }
  }
  out.sort((a, b) => {
    if (b.pct !== a.pct) return b.pct - a.pct;
    if (b.elapsedMs !== a.elapsedMs) return b.elapsedMs - a.elapsedMs;
    return a.worker.task_id < b.worker.task_id ? -1 : 1;
  });
  return out.slice(0, limit);
}

/**************************************************************
                  Coverage chips (severity = color + form)
 **************************************************************/

export const COVERAGE_TONE: Record<CoverageStatus, FcTone> = {
  uncovered: "crit",
  starved: "warn",
  ok: "good",
};

export function coverageChipLabel(status: CoverageStatus): string {
  switch (status) {
    case "uncovered":
      return "UNCOVERED";
    case "starved":
      return "STARVED";
    default:
      return "COVERED";
  }
}

// filterCoverageRows answers "who consumes Z": case-insensitive substring
// typeahead over queue names. Empty query returns everything.
export function filterCoverageRows(rows: CoverageRow[], query: string): CoverageRow[] {
  const q = query.trim().toLowerCase();
  if (!q) return rows;
  return rows.filter((r) => r.queue.toLowerCase().includes(q));
}

/**************************************************************
              Active-task drawer deep link (§3.5 peek param)
 **************************************************************/

// activeTaskDrawerSearch builds the /tasks search string that opens the
// Tasks console filtered to the worker's queue in the active state with the
// task peeked in the drawer: "?queue=X&state=active&peek=X/<id>".
export function activeTaskDrawerSearch(queue: string, taskId: string): string {
  const params = serializeConsoleState({
    ...DEFAULT_CONSOLE_STATE,
    state: "active",
    queue,
  });
  params.set("peek", serializePeek({ queue, id: taskId }));
  return `?${params.toString()}`;
}
