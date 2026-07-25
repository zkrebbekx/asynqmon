// Pure helpers for the Schedulers screen (build contract §3.8): outcome chip
// mapping, GONE row logic, target-queue/retention parsing from asynq option
// strings, and the Retention=0 guidance gating. Pure functions over fetched
// payloads so all of it is unit-testable without a router or a clock.

import { SchedulerOutcome, SchedulerOutcomeEvent, SchedulerRow } from "../api-fleet";
import { FcTone } from "../components/FleetBits";
import { humanizeMs } from "./fleet";

/**************************************************************
                    Outcome chips (§3.8)
 **************************************************************/

// Three-valued outcome tones: succeeded good / failed crit / unknown neutral;
// pending (still in flight) is informational, not a verdict.
export const OUTCOME_TONE: Record<SchedulerOutcome, FcTone> = {
  succeeded: "good",
  failed: "crit",
  pending: "info",
  unknown: "mut",
};

export function outcomeTone(outcome: string): FcTone {
  return OUTCOME_TONE[outcome as SchedulerOutcome] ?? "mut";
}

// outcomeStamp is the small explanation next to the chip (mockup: "completed-
// state task found"). For unknown events the server's reason is the stamp.
export function outcomeStamp(ev: SchedulerOutcomeEvent): string {
  if (ev.outcome === "unknown") return ev.reason ?? "task not found";
  if (ev.state) return `${ev.state}-state task found`;
  return "";
}

/**************************************************************
                    GONE rows (§3.8/§5.12)
 **************************************************************/

// A row is GONE when the server flagged it: no live counterpart and last_seen
// older than the gone threshold. The presence of gone_since IS the flag.
export function isGone(row: Pick<SchedulerRow, "gone_since">): boolean {
  return !!row.gone_since;
}

// goneLabel renders the chip's companion stamp: "last seen 2h ago".
export function goneLabel(goneSince: string | undefined, nowMs: number): string {
  if (!goneSince) return "";
  const t = Date.parse(goneSince);
  if (!Number.isFinite(t)) return "";
  return `last seen ${humanizeMs(Math.max(0, nowMs - t))} ago`;
}

// sortSchedulerRows puts GONE rows first (crit at the top, like the mockup),
// then live rows, both alphabetical by type then spec — stable across polls.
export function sortSchedulerRows(rows: SchedulerRow[]): SchedulerRow[] {
  return [...rows].sort((a, b) => {
    const ag = isGone(a) ? 0 : 1;
    const bg = isGone(b) ? 0 : 1;
    if (ag !== bg) return ag - bg;
    if (a.entry.task_type !== b.entry.task_type)
      return a.entry.task_type < b.entry.task_type ? -1 : 1;
    if (a.entry.spec !== b.entry.spec) return a.entry.spec < b.entry.spec ? -1 : 1;
    return a.stable_key < b.stable_key ? -1 : 1;
  });
}

// filterSchedulerRows narrows by type or spec substring (case-insensitive).
export function filterSchedulerRows(rows: SchedulerRow[], filter: string): SchedulerRow[] {
  const q = filter.trim().toLowerCase();
  if (q === "") return rows;
  return rows.filter(
    (r) =>
      r.entry.task_type.toLowerCase().includes(q) ||
      r.entry.spec.toLowerCase().includes(q)
  );
}

/**************************************************************
            Option-string parsing (queue & retention)
 **************************************************************/

// asynq renders options like Queue("critical"), MaxRetry(5), Retention(1h0m0s).
// The target queue defaults to "default" when no Queue option is present —
// handled explicitly so default-queue entries deep-link instead of dropping.
export function queueFromOptions(options: string[] | undefined | null): string {
  for (const o of options ?? []) {
    const m = o.match(/^Queue\("?([^")]+)"?\)$/);
    if (m) return m[1];
  }
  return "default";
}

// parseGoDuration parses Go's duration rendering ("1h0m0s", "90s", "1.5h",
// "500ms") into whole seconds; null when unparsable. Used for the retention
// badge on live rows, where only the option string is available.
export function parseGoDuration(s: string): number | null {
  const trimmed = s.trim();
  if (trimmed === "" || trimmed === "0" || trimmed === "0s") return 0;
  // Longest unit first: "500ms" must not lex as "500m" + dangling "s".
  const re = /(\d+(?:\.\d+)?)(ms|h|m|s)/g;
  let total = 0;
  let matchedLen = 0;
  const unit: Record<string, number> = { h: 3600, m: 60, s: 1, ms: 0.001 };
  for (const m of trimmed.matchAll(re)) {
    total += parseFloat(m[1]) * unit[m[2]];
    matchedLen += m[0].length;
  }
  if (matchedLen !== trimmed.length) return null; // leftovers = not a duration
  return Math.floor(total);
}

// retentionSecondsFromOptions extracts Retention(...) in whole seconds;
// 0 when absent (asynq's default: completions are deleted immediately).
export function retentionSecondsFromOptions(options: string[] | undefined | null): number {
  for (const o of options ?? []) {
    const m = o.match(/^Retention\(([^)]+)\)$/);
    if (m) return parseGoDuration(m[1]) ?? 0;
  }
  return 0;
}

// retentionLabel renders the badge: "Retention 1h" or "Retention 0".
export function retentionLabel(seconds: number): string {
  if (seconds <= 0) return "Retention 0";
  return `Retention ${humanizeMs(seconds * 1000)}`;
}

/**************************************************************
                Retention=0 guidance (§3.8, verbatim)
 **************************************************************/

// The §3.8 guidance callout, rendered VERBATIM when — and only when — the
// entry has Retention=0 and unknown outcomes exist. The screen never claims
// to close the "did last night's crons succeed" question for retention-less
// entries; it tells you how to make it closeable.
export const RETENTION_GUIDANCE =
  "Task no longer in Redis. This scheduler's tasks have Retention=0, so " +
  "successful completions are deleted immediately — success is unknowable. " +
  "Set asynq.Retention on the entry to make outcomes traceable.";

export function showRetentionGuidance(
  retentionSeconds: number,
  events: Array<Pick<SchedulerOutcomeEvent, "outcome">> | undefined | null
): boolean {
  if (retentionSeconds !== 0) return false;
  return (events ?? []).some((ev) => ev.outcome === "unknown");
}
