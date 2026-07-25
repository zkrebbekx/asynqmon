// Pure logic for the Queue Workspace (build contract §3.3): tab/focus URL
// routing, the Attention tab's anomaly synthesis, active-row budget bars, and
// histogram bucket rendering. Pure functions over already-fetched payloads so
// all of it is unit-testable without a router, a clock, or Redis.

import type { DailyStat, TaskInfo } from "../api";
import type {
  FleetQueueRow,
  PendingWaitSampleResponse,
  RetryHistogramResponse,
} from "../api-fleet";
import { humanizeMs } from "./fleet";

/**************************************************************
                  Tab & focus URL routing (§3.3)
 **************************************************************/

// The Attention tab is the default — the workspace NEVER lands on `active`.
export const WORKSPACE_TABS = [
  "attention",
  "pending",
  "active",
  "aggregating",
  "scheduled",
  "retry",
  "archived",
  "completed",
] as const;
export type WorkspaceTab = (typeof WORKSPACE_TABS)[number];

export const DEFAULT_WORKSPACE_TAB: WorkspaceTab = "attention";

// States a Fleet finding can pre-focus (?focus=<state>). Focus lands on the
// Attention tab with the named state's section highlighted.
export const FOCUS_STATES = [
  "pending",
  "active",
  "aggregating",
  "scheduled",
  "retry",
  "archived",
  "completed",
] as const;
export type FocusState = (typeof FOCUS_STATES)[number];

export interface WorkspaceParams {
  tab: WorkspaceTab;
  focus: FocusState | null;
}

// parseWorkspaceParams reads the workspace's URL state. The tab travels in
// the legacy `status` param (so existing queueDetailsPath(q, state) links
// keep working); invalid or absent values land on Attention, never active.
// `focus` (written by attentionTarget for Fleet findings) is only honored on
// the Attention tab — an explicit `status` wins over a stale focus.
export function parseWorkspaceParams(params: URLSearchParams): WorkspaceParams {
  const rawTab = params.get("status") ?? "";
  const tab = (WORKSPACE_TABS as readonly string[]).includes(rawTab)
    ? (rawTab as WorkspaceTab)
    : DEFAULT_WORKSPACE_TAB;
  const rawFocus = params.get("focus") ?? "";
  const focus =
    tab === "attention" && (FOCUS_STATES as readonly string[]).includes(rawFocus)
      ? (rawFocus as FocusState)
      : null;
  return { tab, focus };
}

// serializeWorkspaceTab writes a tab switch into search params, dropping any
// `focus` (focus is an arrival state, not a navigation state). Params the
// workspace doesn't own survive untouched.
export function serializeWorkspaceTab(
  tab: WorkspaceTab,
  base?: URLSearchParams
): URLSearchParams {
  const params = new URLSearchParams(base);
  params.delete("focus");
  if (tab === DEFAULT_WORKSPACE_TAB) params.delete("status");
  else params.set("status", tab);
  return params;
}

/**************************************************************
            Active-row budget (elapsed vs timeout, §3.3)
 **************************************************************/

export interface TaskBudget {
  elapsedMs: number | null;
  budgetMs: number | null;
  // elapsed / budget; may exceed 1 (past deadline) — callers clamp the BAR,
  // never the number. null when no honest budget exists.
  pct: number | null;
}

function parseWireTime(raw: string | undefined | null): number | null {
  if (!raw) return null;
  const t = Date.parse(raw);
  if (!Number.isFinite(t) || t <= 0) return null;
  return t;
}

// activeTaskBudget computes the elapsed-vs-deadline budget for an ACTIVE
// task row. The budget is deadline − start when the task carries a parsable
// deadline after its start; otherwise timeout_seconds (asynq's default
// deadline source) — otherwise no bar is drawn (absent data is not 100%).
export function activeTaskBudget(
  task: Pick<TaskInfo, "start_time" | "deadline" | "timeout_seconds">,
  nowMs: number
): TaskBudget {
  const start = parseWireTime(task.start_time);
  if (start === null) return { elapsedMs: null, budgetMs: null, pct: null };
  const elapsedMs = Math.max(0, nowMs - start);
  const end = parseWireTime(task.deadline);
  let budgetMs: number | null = null;
  if (end !== null && end > start) {
    budgetMs = end - start;
  } else if (task.timeout_seconds > 0) {
    budgetMs = task.timeout_seconds * 1000;
  }
  if (budgetMs === null) return { elapsedMs, budgetMs: null, pct: null };
  return { elapsedMs, budgetMs, pct: elapsedMs / budgetMs };
}

// sortByBudgetPct orders active tasks worst-first (the wedged worker is row
// one). Orphaned rows sink to the bottom (they have no live worker to watch);
// rows without a computable budget sort after budgeted ones. Ties break by
// longer elapsed, then id, for determinism. Pure — returns a new array.
export function sortByBudgetPct<T extends TaskInfo>(tasks: T[], nowMs: number): T[] {
  const rank = tasks.map((t) => ({ t, b: activeTaskBudget(t, nowMs) }));
  rank.sort((a, b) => {
    if (a.t.is_orphaned !== b.t.is_orphaned) return a.t.is_orphaned ? 1 : -1;
    const ap = a.b.pct;
    const bp = b.b.pct;
    if (ap === null && bp === null) {
      // fall through to elapsed
    } else if (ap === null) {
      return 1;
    } else if (bp === null) {
      return -1;
    } else if (ap !== bp) {
      return bp - ap;
    }
    const ae = a.b.elapsedMs ?? -1;
    const be = b.b.elapsedMs ?? -1;
    if (ae !== be) return be - ae;
    return a.t.id < b.t.id ? -1 : 1;
  });
  return rank.map((r) => r.t);
}

/**************************************************************
              Attention synthesis (§3.3 default tab)
 **************************************************************/

export type AttentionKind =
  | "orphans"
  | "retry_storm"
  | "pending_age"
  | "past_due"
  | "paused"
  | "archive_cap"
  | "archived_delta";

export interface WorkspaceAttentionItem {
  kind: AttentionKind;
  tone: "crit" | "warn" | "mut";
  chip: string; // e.g. "ORPHANS 14"
  sentence: string;
  // The state section/tab the item belongs to — focus targets match on this.
  focusState: FocusState;
}

export interface AttentionInput {
  // All-seven state counts (state_counts endpoint); null while loading.
  counts: Record<string, number> | null;
  // This queue's stats-cache row; null when the fleet endpoints are absent.
  fleetRow: FleetQueueRow | null;
  // Page 1 of active tasks (carries is_orphaned).
  activeTasks: TaskInfo[];
  // Page 1 of scheduled tasks (score-ascending: past-due first).
  scheduledTasks: TaskInfo[];
  // Retry-ETA histogram (exact); null while loading/absent.
  retryHistogram: RetryHistogramResponse | null;
  // Daily counters, oldest→newest (getQueue history); [] when unknown.
  history: DailyStat[];
  nowMs: number;
  // Oldest-pending SLO; default 5 minutes (§3.1 PENDING AGE).
  pendingAgeSloMs?: number;
}

export const DEFAULT_PENDING_AGE_SLO_MS = 5 * 60 * 1000;

// asynq trims every archive zset to 10k entries (§7.3).
export const ARCHIVE_CAP = 10_000;

// Retry mass firing within this window counts as "the storm lands now".
export const RETRY_STORM_WINDOW_S = 300;

// retryFireWithin sums the exact histogram mass (past-due included — those
// fire immediately) landing within the given horizon.
export function retryFireWithin(
  hist: RetryHistogramResponse | null,
  horizonSeconds: number
): number {
  if (!hist || hist.bucket_seconds <= 0) return 0;
  const n = Math.min(
    hist.buckets.length,
    Math.ceil(horizonSeconds / hist.bucket_seconds)
  );
  let sum = hist.past_due;
  for (let i = 0; i < n; i++) sum += hist.buckets[i];
  return sum;
}

// orphanedActives filters the active page down to expired-lease rows.
export function orphanedActives(activeTasks: TaskInfo[]): TaskInfo[] {
  return activeTasks.filter((t) => t.is_orphaned);
}

// pastDueScheduled: scheduled tasks whose next_process_at is already behind
// now (forwarder stall symptom). The list endpoint returns score-ascending
// pages, so page 1 catches the worst offenders.
export function pastDueScheduled(
  scheduledTasks: TaskInfo[],
  nowMs: number
): TaskInfo[] {
  return scheduledTasks.filter((t) => {
    const at = parseWireTime(t.next_process_at);
    return at !== null && at < nowMs;
  });
}

// upcomingRetries sorts retry tasks by next fire time ascending (defensive —
// the API already lists by score) and annotates each with its countdown.
export interface RetryRow {
  task: TaskInfo;
  // ms until next fire; ≤0 = due now.
  countdownMs: number | null;
}

export function upcomingRetries(retryTasks: TaskInfo[], nowMs: number): RetryRow[] {
  const rows = retryTasks.map((task) => {
    const at = parseWireTime(task.next_process_at);
    return { task, countdownMs: at === null ? null : at - nowMs };
  });
  rows.sort((a, b) => {
    if (a.countdownMs === null && b.countdownMs === null) return 0;
    if (a.countdownMs === null) return 1;
    if (b.countdownMs === null) return -1;
    return a.countdownMs - b.countdownMs;
  });
  return rows;
}

// formatCountdown renders a retry countdown: "in 4m 32s", "due now" at ≤0,
// "—" when unknown.
export function formatCountdown(countdownMs: number | null): string {
  if (countdownMs === null || !Number.isFinite(countdownMs)) return "—";
  if (countdownMs <= 0) return "due now";
  return `in ${humanizeMs(countdownMs)}`;
}

// archivedDelta compares today's failure counter against yesterday's (daily
// counters are never trimmed — magnitude truth, §3.1 Region D). Dates are
// asynq's UTC counter dates.
export function archivedDelta(
  history: DailyStat[],
  nowMs: number
): { today: number; yesterday: number } | null {
  if (history.length === 0) return null;
  const day = (offset: number) =>
    new Date(nowMs - offset * 86_400_000).toISOString().slice(0, 10);
  const byDate = new Map(history.map((h) => [h.date, h]));
  const today = byDate.get(day(0));
  const yesterday = byDate.get(day(1));
  if (!today && !yesterday) return null;
  return { today: today?.failed ?? 0, yesterday: yesterday?.failed ?? 0 };
}

// synthesizeAttention builds the summary anomaly items of the Attention tab.
// Every item is derived from verified-cheap reads already on hand; nothing
// here invents data — absent inputs simply produce no item.
export function synthesizeAttention(input: AttentionInput): WorkspaceAttentionItem[] {
  const items: WorkspaceAttentionItem[] = [];
  const sloMs = input.pendingAgeSloMs ?? DEFAULT_PENDING_AGE_SLO_MS;

  // Paused — work is accepted but will not run.
  if (input.fleetRow?.paused) {
    const since = parseWireTime(input.fleetRow.paused_since);
    const forMs = since === null ? null : input.nowMs - since;
    items.push({
      kind: "paused",
      tone: "warn",
      chip: forMs !== null ? `PAUSED ${humanizeMs(forMs)}` : "PAUSED",
      sentence:
        "queue is paused — producers can still enqueue, nothing is being consumed",
      focusState: "pending",
    });
  }

  // Orphans — expired leases beyond grace (sweeper-debounced count when the
  // cache row is present; the active page supplies the sample rows).
  const orphanPage = orphanedActives(input.activeTasks).length;
  const orphanCount = Math.max(input.fleetRow?.orphan_candidates ?? 0, orphanPage);
  if (orphanCount > 0) {
    items.push({
      kind: "orphans",
      tone: "warn",
      chip: `ORPHANS ${orphanCount.toLocaleString("en-US")}`,
      sentence:
        "active tasks with an expired lease — worker crash suspected; asynq's recoverer will requeue them on lease expiry",
      focusState: "active",
    });
  }

  // Pending age vs SLO.
  const ageMs = input.fleetRow?.oldest_pending_age_ms ?? 0;
  if (ageMs >= sloMs) {
    items.push({
      kind: "pending_age",
      tone: ageMs >= 6 * sloMs ? "crit" : "warn",
      chip: "PENDING AGE",
      sentence: `oldest pending task has waited ${humanizeMs(ageMs)} vs ${humanizeMs(sloMs)} SLO`,
      focusState: "pending",
    });
  }

  // Retry storm — exact mass firing in the next 5 minutes.
  const storm = retryFireWithin(input.retryHistogram, RETRY_STORM_WINDOW_S);
  if (storm > 0) {
    items.push({
      kind: "retry_storm",
      tone: storm >= 1000 ? "crit" : "warn",
      chip: `RETRY STORM`,
      sentence: `${storm.toLocaleString("en-US")} retries fire in the next 5m (exact, from retry scores)`,
      focusState: "retry",
    });
  }

  // Past-due scheduled — forwarder stall.
  const pastDueCount = Math.max(
    input.fleetRow?.past_due_scheduled ?? 0,
    pastDueScheduled(input.scheduledTasks, input.nowMs).length
  );
  if (pastDueCount > 0) {
    items.push({
      kind: "past_due",
      tone: "warn",
      chip: `PAST-DUE ${pastDueCount.toLocaleString("en-US")}`,
      sentence:
        "scheduled tasks whose fire time is behind now — asynq's forwarder may be stalled",
      focusState: "scheduled",
    });
  }

  // Archive at trim cap — forensic data is being discarded.
  const archived = input.counts?.archived ?? input.fleetRow?.archived ?? 0;
  if (archived >= ARCHIVE_CAP) {
    items.push({
      kind: "archive_cap",
      tone: "warn",
      chip: "ARCHIVE CAP",
      sentence:
        "archived set is at asynq's 10k trim cap — every further archive discards the oldest entry; archived-derived counts are lower bounds",
      focusState: "archived",
    });
  }

  // Archived/failure delta vs yesterday (daily counters, never trimmed).
  const delta = archivedDelta(input.history, input.nowMs);
  if (delta && delta.today > 0) {
    items.push({
      kind: "archived_delta",
      tone: delta.yesterday > 0 && delta.today >= 5 * delta.yesterday ? "warn" : "mut",
      chip: "FAILED Δ",
      sentence: `${delta.today.toLocaleString("en-US")} failures today vs ${delta.yesterday.toLocaleString("en-US")} yesterday (daily counters)`,
      focusState: "archived",
    });
  }

  const toneRank = { crit: 0, warn: 1, mut: 2 } as const;
  items.sort((a, b) => toneRank[a.tone] - toneRank[b.tone]);
  return items;
}

// healthySummary composes the §3.3 empty state: "Nothing anomalous. Oldest
// pending 4s. 0 failures today."
export function healthySummary(
  fleetRow: FleetQueueRow | null,
  history: DailyStat[],
  nowMs: number
): string {
  const parts = ["Nothing anomalous."];
  const age = fleetRow?.oldest_pending_age_ms ?? 0;
  parts.push(age > 0 ? `Oldest pending ${humanizeMs(age)}.` : "No pending backlog.");
  const delta = archivedDelta(history, nowMs);
  if (delta) {
    parts.push(
      delta.today === 0
        ? "No failures today."
        : `${delta.today.toLocaleString("en-US")} failures today.`
    );
  }
  return parts.join(" ");
}

/**************************************************************
                Histogram rendering (§3.3(c) rail)
 **************************************************************/

// histogramBars normalizes counts into 0–100 bar heights. All-zero input
// yields all-zero heights (an empty histogram renders flat, not full).
export function histogramBars(counts: number[]): number[] {
  const max = counts.reduce((m, c) => Math.max(m, c), 0);
  if (max <= 0) return counts.map(() => 0);
  return counts.map((c) => Math.round((c / max) * 100));
}

export interface RetryHistogramView {
  bars: number[]; // 0–100 heights
  hotIndex: number; // index of the peak bucket, -1 when empty
  next5m: number; // exact mass firing within 5 minutes (incl. past-due)
  pastDue: number;
  beyond: number;
  total: number;
  // "storm lands 14:07–14:12" for the peak bucket; "" when nothing fires.
  stormLabel: string;
  // Axis label for bucket i: "+0m", "+2.5m", …
  bucketLabel: (i: number) => string;
}

function clockLabel(ms: number): string {
  const d = new Date(ms);
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `${hh}:${mm}`;
}

// retryHistogramView turns the exact endpoint response into a render model.
export function retryHistogramView(
  hist: RetryHistogramResponse | null
): RetryHistogramView | null {
  if (!hist) return null;
  const bars = histogramBars(hist.buckets);
  let hotIndex = -1;
  let peak = 0;
  hist.buckets.forEach((c, i) => {
    if (c > peak) {
      peak = c;
      hotIndex = i;
    }
  });
  const fromMs = Date.parse(hist.from);
  let stormLabel = "";
  if (hotIndex >= 0 && Number.isFinite(fromMs)) {
    const lo = fromMs + hotIndex * hist.bucket_seconds * 1000;
    const hi = lo + hist.bucket_seconds * 1000;
    stormLabel = `storm lands ${clockLabel(lo)}–${clockLabel(hi)}`;
  }
  const bucketLabel = (i: number) => {
    const mins = (i * hist.bucket_seconds) / 60;
    return `+${Number.isInteger(mins) ? mins : mins.toFixed(1)}m`;
  };
  return {
    bars,
    hotIndex,
    next5m: retryFireWithin(hist, RETRY_STORM_WINDOW_S),
    pastDue: hist.past_due,
    beyond: hist.beyond,
    total: hist.total,
    stormLabel,
    bucketLabel,
  };
}

// Fixed wait-bucket boundaries (ms upper bounds; the last bucket is open).
export const WAIT_BUCKET_BOUNDS_MS = [
  1_000, 10_000, 60_000, 300_000, 900_000, 3_600_000, 21_600_000,
] as const;

export const WAIT_BUCKET_LABELS = [
  "<1s",
  "1–10s",
  "10s–1m",
  "1–5m",
  "5–15m",
  "15m–1h",
  "1–6h",
  "≥6h",
] as const;

export interface WaitBucket {
  label: string;
  count: number;
}

// bucketWaitsMs folds sampled waits into the fixed buckets. Negative or
// non-finite samples are dropped (never guessed into a bucket).
export function bucketWaitsMs(waitsMs: number[]): WaitBucket[] {
  const counts = new Array(WAIT_BUCKET_BOUNDS_MS.length + 1).fill(0);
  for (const w of waitsMs) {
    if (!Number.isFinite(w) || w < 0) continue;
    let i = WAIT_BUCKET_BOUNDS_MS.findIndex((b) => w < b);
    if (i === -1) i = counts.length - 1;
    counts[i]++;
  }
  return counts.map((count, i) => ({ label: WAIT_BUCKET_LABELS[i], count }));
}

// pendingWaitView: render model for the sampled pending-wait histogram.
export interface PendingWaitView {
  buckets: WaitBucket[];
  bars: number[];
  sampled: number;
  listLen: number;
  method: string; // rendered verbatim (§3.3(c) honesty label)
}

export function pendingWaitView(
  sample: PendingWaitSampleResponse | null
): PendingWaitView | null {
  if (!sample) return null;
  const buckets = bucketWaitsMs(sample.waits_ms);
  return {
    buckets,
    bars: histogramBars(buckets.map((b) => b.count)),
    sampled: sample.sampled,
    listLen: sample.list_len,
    method: sample.method,
  };
}

/**************************************************************
          This queue's error clusters (rail panel §3.3(c))
 **************************************************************/

// One merged cluster row from the retry + archived task_aggregate calls.
export interface ErrorCluster {
  label: string;
  retryCount: number;
  archivedCount: number;
  total: number;
}

// mergeErrorClusters joins the two per-state aggregate group lists by
// signature label, sorted by combined count desc then label, capped.
export function mergeErrorClusters(
  retryGroups: { label: string; count: number }[],
  archivedGroups: { label: string; count: number }[],
  limit = 8
): ErrorCluster[] {
  const map = new Map<string, ErrorCluster>();
  for (const g of retryGroups) {
    if (!g.label) continue;
    const c = map.get(g.label) ?? { label: g.label, retryCount: 0, archivedCount: 0, total: 0 };
    c.retryCount += g.count;
    c.total += g.count;
    map.set(g.label, c);
  }
  for (const g of archivedGroups) {
    if (!g.label) continue;
    const c = map.get(g.label) ?? { label: g.label, retryCount: 0, archivedCount: 0, total: 0 };
    c.archivedCount += g.count;
    c.total += g.count;
    map.set(g.label, c);
  }
  const out = [...map.values()];
  out.sort((a, b) => (a.total !== b.total ? b.total - a.total : a.label < b.label ? -1 : 1));
  return out.slice(0, limit);
}
