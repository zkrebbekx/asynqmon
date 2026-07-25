// Pure logic for the Hygiene screen (§3.10): kind catalog, summary-chip
// derivation, schedule-editor logic, one-click job-scope construction and
// honest formatting helpers. No React, no I/O — fully unit-tested.

import type { ActivitySource, DeadLetterAction, HygieneCard, HygieneKind } from "../api-hygiene";
import type { BulkJobScope } from "../components/BulkJobModal";
import type { FcTone } from "../components/FleetBits";

/**************************************************************
                       Kind catalog
 **************************************************************/

export const HYGIENE_KINDS: HygieneKind[] = [
  "inventory",
  "storage",
  "dead-letter",
  "scheduler-health",
];

export const KIND_TITLES: Record<HygieneKind, string> = {
  inventory: "Queue inventory",
  storage: "Storage attribution",
  "dead-letter": "Dead-letter digest",
  "scheduler-health": "Scheduler health",
};

export const KIND_QUESTIONS: Record<HygieneKind, string> = {
  inventory: "which queues are dead, unconsumed, or forgotten-paused?",
  storage: "where does the Redis memory actually go, and when does it come back?",
  "dead-letter": "what is rotting in the archives, and what can go?",
  "scheduler-health": "which cron entries are dead, untraceable, or aimed at nobody?",
};

/**************************************************************
                     Schedule editor logic
 **************************************************************/

export interface IntervalPreset {
  label: string;
  seconds: number;
}

export const INTERVAL_PRESETS: IntervalPreset[] = [
  { label: "hourly", seconds: 3600 },
  { label: "daily", seconds: 24 * 3600 },
  { label: "weekly", seconds: 7 * 24 * 3600 },
  { label: "monthly", seconds: 30 * 24 * 3600 },
];

// Mirrors the backend's ValidateConfig bounds (1m..90d).
export const MIN_INTERVAL_SECONDS = 60;
export const MAX_INTERVAL_SECONDS = 90 * 24 * 3600;

export function validInterval(seconds: number): boolean {
  return (
    Number.isFinite(seconds) &&
    Number.isInteger(seconds) &&
    seconds >= MIN_INTERVAL_SECONDS &&
    seconds <= MAX_INTERVAL_SECONDS
  );
}

// formatInterval renders an interval as its preset name when it matches one,
// else a compact duration ("every 12h").
export function formatInterval(seconds: number): string {
  const preset = INTERVAL_PRESETS.find((p) => p.seconds === seconds);
  if (preset) return preset.label;
  if (seconds % (24 * 3600) === 0) return `every ${seconds / (24 * 3600)}d`;
  if (seconds % 3600 === 0) return `every ${seconds / 3600}h`;
  if (seconds % 60 === 0) return `every ${seconds / 60}m`;
  return `every ${seconds}s`;
}

// intervalOptions returns the preset list, with the current value appended as
// a custom option when it matches no preset (so the select never lies about
// the stored config).
export function intervalOptions(currentSeconds: number): IntervalPreset[] {
  if (INTERVAL_PRESETS.some((p) => p.seconds === currentSeconds)) {
    return INTERVAL_PRESETS;
  }
  return [...INTERVAL_PRESETS, { label: formatInterval(currentSeconds), seconds: currentSeconds }];
}

/**************************************************************
                   One-click job scope (§3.10)
 **************************************************************/

// deadLetterActionScope turns the report's action descriptor into the
// BulkJobModal scope verbatim — the job flow (preview, typed confirmation,
// audit) stays the standard §4.3 path.
export function deadLetterActionScope(action: DeadLetterAction): BulkJobScope {
  return {
    queue: action.queue,
    state: action.state,
    q: "",
    meta: [],
    aql: action.aql,
  };
}

/**************************************************************
                       Summary chips
 **************************************************************/

export interface SummaryChip {
  label: string;
  value: number;
  tone: FcTone;
  // Formatted value override (bytes); plain locale number otherwise.
  formatted?: string;
}

// summaryChips derives each card's stat rows from the report summary, with
// tones mirroring the mockup: work-not-running is crit, decay is warn.
export function summaryChips(
  kind: HygieneKind,
  summary: Record<string, number> | null
): SummaryChip[] {
  if (!summary) return [];
  const n = (key: string) => summary[key] ?? 0;
  switch (kind) {
    case "inventory":
      return [
        { label: "dead-queue candidates (30d silent)", value: n("dead_queue_candidates"), tone: n("dead_queue_candidates") > 0 ? "warn" : "mut" },
        { label: "zero-consumer with pending", value: n("zero_consumer_pending"), tone: n("zero_consumer_pending") > 0 ? "crit" : "mut" },
        { label: "paused > 7d", value: n("paused_over_7d"), tone: n("paused_over_7d") > 0 ? "warn" : "mut" },
        { label: "queues total", value: n("queues_total"), tone: "mut" },
      ];
    case "storage":
      return [
        { label: "estimated memory (sampled queues)", value: n("estimated_bytes"), tone: "mut", formatted: formatBytes(n("estimated_bytes")) },
        { label: "queues not sampled", value: n("not_sampled_queues"), tone: n("not_sampled_queues") > 0 ? "warn" : "mut" },
        { label: "completed reclaim in 24h", value: n("reclaim_24h"), tone: "mut" },
        { label: "reclaim past due", value: n("reclaim_past_due"), tone: n("reclaim_past_due") > 0 ? "warn" : "mut" },
      ];
    case "dead-letter":
      return [
        { label: "archived total", value: n("archived_total"), tone: "mut" },
        { label: "older than 30d", value: n("older_than_30d"), tone: n("older_than_30d") > 0 ? "warn" : "mut" },
        { label: "queues at 10k archive cap", value: n("trim_cap_queues"), tone: n("trim_cap_queues") > 0 ? "warn" : "mut" },
      ];
    case "scheduler-health":
      return [
        { label: "SCHEDULER GONE", value: n("gone"), tone: n("gone") > 0 ? "crit" : "mut" },
        { label: "retention-less entries (outcome unknowable)", value: n("retention_less"), tone: n("retention_less") > 0 ? "warn" : "mut" },
        { label: "entries targeting zero-consumer queues", value: n("zero_consumer_targets"), tone: n("zero_consumer_targets") > 0 ? "warn" : "mut" },
      ];
    default:
      return [];
  }
}

/**************************************************************
                     Honest formatting
 **************************************************************/

export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n;
  let u = -1;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[u]}`;
}

// activitySourceLabel spells out what each evidence source actually proves —
// the honest-numbers rule: a lower bound says so.
export function activitySourceLabel(source: ActivitySource): string {
  switch (source) {
    case "active_now":
      return "tasks in flight at snapshot";
    case "counters_today":
      return "counter movement today";
    case "oldest_pending":
      return "oldest pending enqueue (lower bound)";
    case "series":
      return "series rollup (30d)";
    case "daily_counters":
      return "daily counters (30d)";
    case "none":
      return "no activity observed in 30d";
    case "unknown":
    default:
      return "not probed — no cheap evidence";
  }
}

/**************************************************************
                        Empty state
 **************************************************************/

// §3.10 empty state: nothing scheduled AND nothing ever generated.
export function nothingScheduled(cards: HygieneCard[]): boolean {
  return cards.length > 0 && cards.every((c) => !c.enabled && !c.last_generated_at);
}
