// Pure logic for the §4.3 bulk-op confirm flow: scope rendering, verb
// semantics, cost disclosure, and the execute-gate rules the modal enforces
// (preview_complete / typed confirmation / mandatory reason / partial-count
// acknowledgment). Pure functions so the gating is unit-testable without
// rendering the modal.

import { JobScope, JobVerb } from "../api";

export const THROTTLE_OPTIONS = [100, 200, 500, 1000] as const;
export const DEFAULT_THROTTLE = 200;

// Above this count, delete demands a typed confirmation (§4.3 step 4).
export const TYPED_CONFIRM_THRESHOLD = 1000;

// asynq trims every archive zset to 10k entries (§7.3).
export const ARCHIVE_TRIM_CAP = 10000;

// scopeLabel renders a scope the way the Ops table and the modal echo it:
// the console's filter vocabulary as query-ish tokens.
export function scopeLabel(scope: {
  queue?: string;
  state: string;
  q?: string;
  meta?: string[];
}): string {
  const parts = [`queue=${scope.queue || "all"}`, `state=${scope.state}`];
  for (const m of scope.meta ?? []) {
    const i = m.indexOf(":");
    if (i > 0) parts.push(`${m.slice(0, i)}=${m.slice(i + 1)}`);
  }
  if (scope.q) parts.push(`q~"${scope.q}"`);
  return parts.join(" ");
}

// verbSemantics is the §4.3 step-4 explicit-semantics line per verb.
export function verbSemantics(verb: JobVerb): string {
  switch (verb) {
    case "archive":
      return "Archive moves tasks to the dead-letter set — recoverable.";
    case "delete":
      return "Delete is gone forever — not recoverable.";
    case "cancel":
      return "Cancel is a cooperative signal — the handler must honor context cancellation, and a worker must be subscribed for it to have any effect.";
    case "run":
      return "Run enqueues each task for immediate processing (now-only; asynq cannot schedule to a specific time).";
  }
}

// archiveTrimWarning warns when the archive verb will itself trim (§7.3:
// "archiving 30k tasks into a 10k-capped set will discard the oldest 20k").
export function archiveTrimWarning(verb: JobVerb, count: number): string | null {
  if (verb !== "archive" || count <= ARCHIVE_TRIM_CAP) return null;
  const discarded = count - ARCHIVE_TRIM_CAP;
  return `Archiving ${count.toLocaleString()} tasks into a 10k-capped set will discard the oldest ${discarded.toLocaleString()}.`;
}

// typedConfirmPhrase returns the phrase the operator must type, or null when
// no typed confirmation is required. Only delete demands it, and only above
// the threshold; the count is the preview's exact candidate count (the gate
// already requires a completed preview for delete).
export function typedConfirmPhrase(verb: JobVerb, count: number): string | null {
  if (verb !== "delete" || count <= TYPED_CONFIRM_THRESHOLD) return null;
  return `DELETE ${count}`;
}

// costEstimateLabel turns matches × list-length into the disclosed
// single-threaded-Redis-time estimate. ~100ns per element scanned per LREM
// is an order-of-magnitude figure and is labeled as such by the caller.
export function costEstimateLabel(matches: number, listLen: number): string {
  const seconds = (matches * listLen) / 1e7;
  if (seconds < 1) return "under a second";
  if (seconds < 60) return `~${Math.ceil(seconds)}s`;
  if (seconds < 3600) return `~${Math.ceil(seconds / 60)} min`;
  return `~${(seconds / 3600).toFixed(1)} h`;
}

export interface GateInput {
  verb: JobVerb;
  previewComplete: boolean;
  candidates: number;
  reason: string;
  typedConfirm: string; // what the operator typed (empty when not asked)
  proceedOnPartial: boolean; // explicit partial-count acknowledgment
}

export interface GateResult {
  ok: boolean;
  // Why the confirm button is disabled (empty when ok).
  blockedBecause: string;
}

// gateExecute is the §4.3 execute gate, client-side mirror of the server's:
//   - a reason is always mandatory (§5.11);
//   - delete requires a COMPLETED preview — no partial-ack override;
//   - other verbs on a partial preview require the explicit acknowledgment;
//   - large deletes additionally require the typed phrase.
// The server re-checks everything; this only drives the button state.
export function gateExecute(input: GateInput): GateResult {
  if (input.reason.trim() === "") {
    return { ok: false, blockedBecause: "A reason is required — it goes to the audit log." };
  }
  if (!input.previewComplete) {
    if (input.verb === "delete") {
      return {
        ok: false,
        blockedBecause:
          "Delete requires a completed preview. The button unlocks when the count is exact.",
      };
    }
    if (!input.proceedOnPartial) {
      return {
        ok: false,
        blockedBecause:
          "Preview is still counting — acknowledge proceeding on a partial count, or wait.",
      };
    }
  }
  const phrase = input.previewComplete
    ? typedConfirmPhrase(input.verb, input.candidates)
    : null;
  if (phrase && input.typedConfirm.trim() !== phrase) {
    return { ok: false, blockedBecause: `Type "${phrase}" to confirm.` };
  }
  return { ok: true, blockedBecause: "" };
}

// jobProgress computes the acted/skipped/failed completion fraction (0..1)
// against the candidate count; null when it cannot be known yet.
export function jobProgress(counts: {
  candidates: number;
  acted: number;
  skipped: number;
  failed: number;
}): number | null {
  if (counts.candidates <= 0) return null;
  const done = counts.acted + counts.skipped + counts.failed;
  return Math.min(1, done / counts.candidates);
}

// isTerminalJobState mirrors jobs.State.IsTerminal.
export function isTerminalJobState(state: string): boolean {
  return state === "done" || state === "canceled" || state === "failed";
}

// verifyVerdict is the plain-language §3.9 verify-panel sentence: the scope
// re-counted live against what the job reported.
export function verifyVerdict(
  liveMatches: number,
  truncated: boolean,
  scope: JobScope,
  counts: { candidates: number; acted: number; skipped: number; failed: number }
): string {
  const scopeText = scopeLabel(scope);
  if (liveMatches === 0) {
    return `Scope now matches 0 tasks — drained. (${scopeText})`;
  }
  const n = truncated ? `${liveMatches}+` : `${liveMatches}`;
  return (
    `Scope still matches ${n} task(s) — was ${counts.candidates} at preview; ` +
    `${counts.acted} acted, ${counts.skipped} skipped, ${counts.failed} failed. ` +
    `New tasks may have entered the scope since. (${scopeText})`
  );
}
