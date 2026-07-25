// Pure helpers for the Fleet landing + Queues Directory: humanized ages,
// staleness detection, and the attention-finding → URL wiring ("the system
// writes the 3am query", build contract §3.1). Pure functions so all of it is
// unit-testable without a router or a clock.

import {
  ConsoleTaskState,
  CONSOLE_STATES,
  DEFAULT_CONSOLE_STATE,
  serializeConsoleState,
  serializeDirectoryState,
  DEFAULT_DIRECTORY_STATE,
} from "./urlstate";

/**************************************************************
                        Ages & staleness
 **************************************************************/

// humanizeMs renders a millisecond duration the way the mockup does:
// "42m 18s", "1h 58m", "58s", "3d 4h". Zero/negative/absent → "—" (the
// directory uses it for oldest-pending age where 0 means "no pending").
export function humanizeMs(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms <= 0) return "—";
  const s = Math.floor(ms / 1000);
  if (s < 1) return "<1s";
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return sec > 0 ? `${m}m ${sec}s` : `${m}m`;
  return `${sec}s`;
}

// Age severity for oldest-pending, mirroring the mockup's ageStyle rule:
// crit at ≥30 minutes, warn at ≥5 minutes (the fleet SLO), quiet below.
export function ageTone(ms: number | undefined | null): "crit" | "warn" | null {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms <= 0) return null;
  if (ms >= 30 * 60 * 1000) return "crit";
  if (ms >= 5 * 60 * 1000) return "warn";
  return null;
}

// Staleness dot (§3.2): a directory row whose cache entry is older than the
// hot-tier cadence gets a gray dot + tooltip. Threshold 15s — three hot-tier
// sweep intervals — so a healthy tier-1 fleet never shows dots.
export const STALE_ROW_THRESHOLD_MS = 15_000;

export function isStale(
  refreshedAt: string | undefined | null,
  nowMs: number,
  thresholdMs: number = STALE_ROW_THRESHOLD_MS
): boolean {
  if (!refreshedAt) return true; // never refreshed = stale, not fresh
  const t = Date.parse(refreshedAt);
  if (Number.isNaN(t)) return true;
  return nowMs - t > thresholdMs;
}

/**************************************************************
                    Attention findings
 **************************************************************/

// severityTone maps the attention engine's 1–5 severity (5 = work is not
// running) onto the three visual tones of the rail's stripe + chips.
export function severityTone(severity: number): "crit" | "warn" | "info" {
  if (severity >= 5) return "crit";
  if (severity >= 3) return "warn";
  return "info";
}

// Age detectors report `value` in whole seconds (stats/attention.go); count
// detectors report a count. Render each as itself.
const AGE_DETECTORS = new Set(["PENDING_AGE", "PAUSED_LONG", "GROUP_STALL"]);

export function formatFindingValue(detector: string, value: number): string {
  if (!Number.isFinite(value)) return "—";
  if (AGE_DETECTORS.has(detector)) return humanizeMs(value * 1000);
  return value.toLocaleString("en-US");
}

// formatLatencyMs renders queue latency: sub-second values stay in
// milliseconds ("340ms"), larger ones humanize ("1m 4s").
export function formatLatencyMs(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms === 0) return "0ms";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return humanizeMs(ms);
}

// formatErrorRate renders the contract's error_rate. The backend sends a
// fraction (failed / processed+failed); a value > 1 is treated as an
// already-percent number so a contract drift degrades to a readable figure
// instead of "3400%".
export function formatErrorRate(rate: number | undefined | null): string {
  if (rate === undefined || rate === null || !Number.isFinite(rate)) return "—";
  const pct = rate <= 1 ? rate * 100 : rate;
  return `${pct.toFixed(1)}%`;
}

/**************************************************************
              Attention finding → pre-filtered URL
 **************************************************************/

// A finding's suggested_query is a space-separated clause list written by the
// attention engine, e.g. "queue=search:reindex state=pending" or
// "consumers=0 pending>0". Full AQL lands in phase 6; until then we route:
//
//   - clauses with a `state=` (task-scoped) → the Tasks console, mapping the
//     clauses today's console can answer (queue, state, error~/type~/payload~
//     as free text) and dropping the rest — the console shows its own filter
//     bar so nothing is hidden from the operator;
//   - everything else (queue-set predicates) → the Queues Directory with the
//     query passed through verbatim as `f=` (the phase-2 endpoint owns it).
//
// Pure: returns which surface plus a search string; the caller prefixes
// window.ROOT_PATH via paths().

export interface AttentionTarget {
  kind: "tasks" | "queues";
  search: string; // "" or "?..." ready to append to the path
}

interface Clause {
  key: string;
  op: string;
  value: string;
}

// key, then one of the comparison operators, then the (possibly quoted) value.
const CLAUSE_RE = /^([a-zA-Z_][a-zA-Z0-9_.]*)(>=|<=|=|~|>|<)(.*)$/;

function parseClauses(q: string): Clause[] {
  // Split on whitespace outside double quotes so error~"gateway timeout"
  // stays one clause.
  const tokens = q.match(/(?:[^\s"]+|"[^"]*")+/g) ?? [];
  const clauses: Clause[] = [];
  for (const token of tokens) {
    const m = CLAUSE_RE.exec(token);
    if (!m) continue; // bare words (e.g. `past_due`) carry no console mapping
    clauses.push({ key: m[1], op: m[2], value: m[3].replace(/^"|"$/g, "") });
  }
  return clauses;
}

export function attentionTarget(suggestedQuery: string): AttentionTarget {
  const raw = (suggestedQuery ?? "").trim();
  if (raw === "") return { kind: "queues", search: "" };

  const clauses = parseClauses(raw);
  const stateClause = clauses.find((c) => c.key === "state" && c.op === "=");

  if (stateClause) {
    const state = (CONSOLE_STATES as readonly string[]).includes(stateClause.value)
      ? (stateClause.value as ConsoleTaskState)
      : DEFAULT_CONSOLE_STATE.state;
    const queue =
      clauses.find((c) => c.key === "queue" && c.op === "=")?.value ??
      DEFAULT_CONSOLE_STATE.queue;
    // Substring-ish clauses map onto the console's free-text search.
    const q =
      clauses.find((c) => ["error", "type", "payload"].includes(c.key) && c.op === "~")
        ?.value ?? "";
    const params = serializeConsoleState({
      ...DEFAULT_CONSOLE_STATE,
      state,
      queue,
      q,
    });
    const s = params.toString();
    return { kind: "tasks", search: s ? `?${s}` : "" };
  }

  // Queue-set predicate: hand the whole expression to the directory.
  const params = serializeDirectoryState({ ...DEFAULT_DIRECTORY_STATE, f: raw });
  const s = params.toString();
  return { kind: "queues", search: s ? `?${s}` : "" };
}
