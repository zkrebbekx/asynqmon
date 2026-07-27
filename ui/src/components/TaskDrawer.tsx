import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSelector } from "react-redux";
import {
  AlertCircle, Archive, Check, ChevronLeft, ChevronRight, Copy, CopyPlus, Play, Trash2, X,
} from "lucide-react";
import { AppState, useAppDispatch } from "../store";
import * as api from "../api";
import { TaskInfo } from "../api";
import {
  runScheduledTaskAsync, runRetryTaskAsync, runArchivedTaskAsync,
  archiveScheduledTaskAsync, archiveRetryTaskAsync, archivePendingTaskAsync,
  deleteScheduledTaskAsync, deleteRetryTaskAsync, deleteArchivedTaskAsync,
  deletePendingTaskAsync, deleteCompletedTaskAsync,
  cancelActiveTaskAsync,
} from "../actions/tasksActions";
import { usePolling } from "../hooks";
import {
  useCorrelationKeys, useEnqueueEnabled, usePayloadDetailLimit,
} from "../hooks/useFeatures";
import {
  durationBefore, durationFromSeconds, durationSince, formatTimestamp,
  prettifyPayload, stringifyDuration, stringifyDurationMs, timeAgo, toErrorString,
} from "../utils";
import { MetaPair, metaId, parseMetadata } from "../lib/metadata";
import {
  distinctQueues, extractCorrelation, mergeFlowTasks, observedAt,
} from "../lib/correlation";
import { STATE_TONE, JourneyState } from "../lib/journey";
import { PeekTarget } from "../lib/urlstate";
import { cn } from "../lib/utils";
import SyntaxHighlighter from "./SyntaxHighlighter";
import ConfirmDialog from "./ConfirmDialog";
import JourneyGraph from "./JourneyGraph";
import CloneEnqueueModal from "./CloneEnqueueModal";

// TaskDrawer: the peek-param task detail overlay (Fleet Console build
// contract §3.5). Opens from any row in the Tasks console without a
// context-losing navigation; deep-linkable and refresh-safe because the
// target lives in the URL. Styled with the --fc-* design tokens.

type Dispatcher = (q: string, id: string) => any;

const runThunks: Record<string, Dispatcher> = {
  scheduled: runScheduledTaskAsync,
  retry: runRetryTaskAsync,
  archived: runArchivedTaskAsync,
};
const archiveThunks: Record<string, Dispatcher> = {
  scheduled: archiveScheduledTaskAsync,
  retry: archiveRetryTaskAsync,
  pending: archivePendingTaskAsync,
};
const deleteThunks: Record<string, Dispatcher> = {
  scheduled: deleteScheduledTaskAsync,
  retry: deleteRetryTaskAsync,
  archived: deleteArchivedTaskAsync,
  pending: deletePendingTaskAsync,
  completed: deleteCompletedTaskAsync,
};

// A console-filter pivot requested from a drawer field ("find all like
// this"). The view applies it to the URL console state; the drawer stays
// open if the task is still in the new results, else the view closes it.
export interface PivotRequest {
  q?: string; // replace the query text
  queue?: string; // scope to a queue
  meta?: MetaPair; // add a metadata chip
}

interface Props {
  peek: PeekTarget;
  // The current visible result list, in list order — prev/next walk it.
  resultList: TaskInfo[];
  onClose: () => void;
  onPeek: (target: PeekTarget) => void;
  onPivot: (pivot: PivotRequest) => void;
}

const FLOW_CAP = 50;
const FLOW_STATES = [
  "active", "pending", "aggregating", "scheduled", "retry", "archived", "completed",
] as const;

const TONE_CLASS: Record<string, string> = {
  good: "bg-[var(--fc-good-bg)] text-[var(--fc-good)]",
  warn: "bg-[var(--fc-warn-bg)] text-[var(--fc-warn)]",
  crit: "bg-[var(--fc-crit-bg)] text-[var(--fc-crit)]",
  info: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  mut: "bg-[var(--fc-raise)] text-[var(--fc-ink2)]",
};

function StateChip({ state, label }: { state: string; label?: string }) {
  const tone = STATE_TONE[state as JourneyState] ?? "mut";
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center rounded px-1.5 py-0.5 font-mono text-[10.5px] font-bold uppercase tracking-[0.04em]",
        TONE_CLASS[tone]
      )}
    >
      {label ?? state}
    </span>
  );
}

// Micro-label section header, per the mockup's .dsec h4.
function SectionTitle({ children }: { children: React.ReactNode }) {
  return (
    <h4 className="mb-2 text-[10.5px] font-semibold uppercase tracking-[0.08em] text-[var(--fc-ink3)]">
      {children}
    </h4>
  );
}

// Muted provenance/staleness stamp, per the mockup's .stamp.
function Stamp({ children, className }: { children: React.ReactNode; className?: string }) {
  return <span className={cn("text-[10.5px] text-[var(--fc-ink3)]", className)}>{children}</span>;
}

// Small drawer-chrome button (header prev/next/close) in fc styling.
function DrawerButton(props: React.ButtonHTMLAttributes<HTMLButtonElement>) {
  const { className, ...rest } = props;
  return (
    <button
      {...rest}
      className={cn(
        "inline-flex h-7 shrink-0 items-center gap-1 rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-2 text-[11px] font-semibold text-[var(--fc-ink)] transition-colors",
        "hover:border-[var(--fc-ink3)] disabled:cursor-not-allowed disabled:opacity-45",
        "focus-visible:outline focus-visible:outline-2 focus-visible:outline-[var(--fc-acc)]",
        className
      )}
    />
  );
}

// Accent pivot link for "find all like this" fields.
function PivotLink({ onClick, children, title }: { onClick: () => void; children: React.ReactNode; title?: string }) {
  return (
    <button
      onClick={onClick}
      title={title ?? "Find all like this"}
      className="font-mono text-xs text-[var(--fc-acc)] hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-[var(--fc-acc)]"
    >
      {children}
    </button>
  );
}

interface LifecycleEvent {
  key: string;
  hot?: boolean; // crit dot (the last-error entry)
  node: React.ReactNode;
}

interface FlowResult {
  tasks: TaskInfo[];
  total: number;
  queues: number; // distinct queues across the full merged (uncapped) chain
  truncated: boolean;
}

const FOCUSABLE =
  'a[href], button:not([disabled]), input, select, textarea, [tabindex]:not([tabindex="-1"])';

export default function TaskDrawer({ peek, resultList, onClose, onPeek, onPivot }: Props) {
  const dispatch = useAppDispatch();
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const peekKey = `${peek.queue}/${peek.id}`;

  const [taskInfo, setTaskInfo] = useState<TaskInfo | null>(null);
  const [fetchErr, setFetchErr] = useState("");
  const [copied, setCopied] = useState(false);
  const [payloadCopied, setPayloadCopied] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [cloneOpen, setCloneOpen] = useState(false);
  const [tab, setTab] = useState<"detail" | "flow">("detail");
  const [flow, setFlow] = useState<FlowResult | null>(null);
  const [flowLoading, setFlowLoading] = useState(false);
  const [flowErr, setFlowErr] = useState("");
  // Observed run history (opt-in observe middleware) — null means "no data",
  // which is the expected case until workers adopt the middleware; the drawer
  // then renders nothing for it (§7 honesty ledger).
  const [observed, setObserved] = useState<api.ObservedTaskResponse | null>(null);

  // 1s tick so countdowns (next retry, expires-in) stay live between polls.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  void now; // read for the re-render only; time helpers read the clock themselves

  // Fetch full detail via getTaskInfo; usePolling refreshes it on the poll
  // tick when polling is active and pauses in hidden tabs.
  const fetchInfo = useCallback(async () => {
    try {
      const info = await api.getTaskInfo(peek.queue, peek.id);
      setTaskInfo(info);
      setFetchErr("");
    } catch (e) {
      setFetchErr(toErrorString(e));
    }
    try {
      // Best-effort side read: getObservedTask resolves null on 404 (the
      // expected no-adoption case); a transient failure keeps the last
      // snapshot rather than surfacing an error for optional data.
      setObserved(await api.getObservedTask(peek.queue, peek.id));
    } catch {
      // ignore — observed data is optional
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [peekKey]);
  usePolling(fetchInfo, pollInterval, [peekKey]);

  // Reset per-task state when the drawer navigates to another task.
  useEffect(() => {
    setTaskInfo(null);
    setFetchErr("");
    setTab("detail");
    setFlow(null);
    setFlowErr("");
    setObserved(null);
    setConfirmDelete(false);
    setCloneOpen(false);
    setPayloadCopied(false);
  }, [peekKey]);

  // Route-match guard (kept from the details view): the state may still hold
  // the previously peeked task while this one loads. Render — and act on —
  // it only if it matches the peek param, so a fast prev/next followed by
  // Delete/Run can never hit the wrong task.
  const task =
    taskInfo && taskInfo.id === peek.id && taskInfo.queue === peek.queue ? taskInfo : null;

  const correlationKeys = useCorrelationKeys();
  const corr = useMemo(
    () => (task ? extractCorrelation(task.payload, correlationKeys) : null),
    [task, correlationKeys]
  );
  const metaPairs = useMemo(() => (task ? parseMetadata(task.payload) : []), [task]);

  // Prev/next through the current result list, in visible order.
  const idx = resultList.findIndex((t) => t.id === peek.id && t.queue === peek.queue);
  const prevTask = idx > 0 ? resultList[idx - 1] : undefined;
  const nextTask = idx >= 0 && idx < resultList.length - 1 ? resultList[idx + 1] : undefined;

  // Latest handlers in refs so the document-level key listener binds once.
  const handlersRef = useRef({ onClose, prevTask, nextTask, onPeek, confirmDelete, cloneOpen });
  handlersRef.current = { onClose, prevTask, nextTask, onPeek, confirmDelete, cloneOpen };

  // Keyboard: Esc closes, [ and ] move through the result list.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable)) return;
      const h = handlersRef.current;
      // The clone modal owns the keyboard while open (its dialog handles Esc).
      if (h.cloneOpen) return;
      if (e.key === "Escape") {
        // The delete ConfirmDialog handles its own Esc — don't double-close.
        if (h.confirmDelete) return;
        e.preventDefault();
        h.onClose();
      } else if (e.key === "[") {
        e.preventDefault();
        if (h.prevTask) h.onPeek({ queue: h.prevTask.queue, id: h.prevTask.id });
      } else if (e.key === "]") {
        e.preventDefault();
        if (h.nextTask) h.onPeek({ queue: h.nextTask.queue, id: h.nextTask.id });
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // Focus trap: focus the panel on open, keep Tab cycling inside it, and
  // restore focus to the opener on close.
  const panelRef = useRef<HTMLElement>(null);
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    panelRef.current?.focus();
    return () => opener?.focus?.();
  }, []);
  const trapTab = (e: React.KeyboardEvent) => {
    if (e.key !== "Tab") return;
    const root = panelRef.current;
    if (!root) return;
    const focusables = Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE));
    if (focusables.length === 0) {
      e.preventDefault();
      return;
    }
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;
    if (e.shiftKey) {
      if (active === first || active === root) {
        e.preventDefault();
        last.focus();
      }
    } else if (active === last) {
      e.preventDefault();
      first.focus();
    }
  };

  // Lazy-load the Flow tab: 7 per-state searches on the correlation key,
  // merged (deduped by id — a task can change state between the searches),
  // time-ordered, capped at FLOW_CAP nodes. A nested correlation id can't be
  // matched by the meta filter (client and server match top-level scalar
  // keys only), so those fall back to a free-text search on the value.
  useEffect(() => {
    if (tab !== "flow" || !corr || flow !== null || flowLoading) return;
    let cancelled = false;
    (async () => {
      setFlowLoading(true);
      try {
        const resps = await Promise.all(
          FLOW_STATES.map((st) =>
            api.searchTasks({
              state: st,
              ...(corr.nested ? { q: corr.value } : { meta: [`${corr.key}:${corr.value}`] }),
              size: FLOW_CAP,
            })
          )
        );
        if (cancelled) return;
        const merged = mergeFlowTasks(resps.map((r) => r.tasks ?? []));
        setFlow({
          tasks: merged.slice(0, FLOW_CAP),
          total: merged.length,
          queues: distinctQueues(merged),
          truncated: resps.some((r) => r.truncated),
        });
        setFlowErr("");
      } catch (e) {
        if (!cancelled) setFlowErr(toErrorString(e));
      }
      if (!cancelled) setFlowLoading(false);
    })();
    return () => {
      cancelled = true;
    };
  }, [tab, corr, flow, flowLoading]);

  const copyId = () => {
    navigator.clipboard?.writeText(peek.id);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  const copyPayload = () => {
    if (!task?.payload) return;
    navigator.clipboard?.writeText(task.payload);
    setPayloadCopied(true);
    setTimeout(() => setPayloadCopied(false), 1500);
  };

  // Server-side detail truncation note (upstream hibiken/asynqmon#301): the
  // backend's truncate yields exactly cap+1 code points ending in "…", so a
  // payload longer than the advertised cap was capped by the server.
  const payloadDetailLimit = usePayloadDetailLimit();
  const payloadCodePoints = useMemo(
    () => (task?.payload ? [...task.payload].length : 0),
    [task]
  );
  const payloadTruncated = payloadDetailLimit > 0 && payloadCodePoints > payloadDetailLimit;

  const state = task?.state ?? "";
  const canRun = !window.READ_ONLY && !!task && !!runThunks[state];
  const canArchive = !window.READ_ONLY && !!task && !!archiveThunks[state];
  const canDelete = !window.READ_ONLY && !!task && !!deleteThunks[state];
  const canCancel = !window.READ_ONLY && !!task && state === "active";
  // Clone-and-edit is offered in EVERY state (§3.5) — it creates a new task,
  // never mutates this one — but only when the deployment enables enqueue
  // (§5.10); the action hides entirely otherwise.
  const enqueueEnabled = useEnqueueEnabled();
  const canClone = enqueueEnabled && !window.READ_ONLY && !!task;

  // Actions re-check the guard (`task` is null unless it matches the peek
  // param) and refetch so the drawer reflects the task's new state.
  const doRun = async () => {
    if (!task) return;
    await dispatch(runThunks[state](task.queue, task.id));
    fetchInfo();
  };
  const doArchive = async () => {
    if (!task) return;
    await dispatch(archiveThunks[state](task.queue, task.id));
    fetchInfo();
  };
  const doCancel = async () => {
    if (!task) return;
    await dispatch(cancelActiveTaskAsync(task.queue, task.id));
    fetchInfo();
  };
  const doDelete = async () => {
    if (!task) return;
    await dispatch(deleteThunks[state](task.queue, task.id));
    onClose();
  };

  // Lifecycle strip, synthesized from stored fields only (no fabricated
  // per-attempt timeline — §7 honesty ledger).
  const events: LifecycleEvent[] = [];
  if (task) {
    events.push({
      key: "enqueued",
      node: (
        <>
          <b className="text-[var(--fc-ink)]">enqueued</b> into {task.queue} —{" "}
          <Stamp>timestamp not stored by asynq</Stamp>
        </>
      ),
    });
    if (task.group) {
      events.push({
        key: "group",
        node: (
          <>
            <b className="text-[var(--fc-ink)]">group</b>{" "}
            <span className="font-mono">{task.group}</span>
            {task.state === "aggregating" && <> — aggregating</>}
          </>
        ),
      });
    }
    if (task.state === "pending" && task.pending_since) {
      events.push({
        key: "pending",
        node: (
          <>
            <b className="text-[var(--fc-ink)]">waiting</b>{" "}
            <span className="font-mono tabular-nums">{durationSince(task.pending_since)}</span> · since{" "}
            {formatTimestamp(task.pending_since)}
          </>
        ),
      });
    }
    if (task.state === "active" && task.start_time) {
      events.push({
        key: "started",
        node: (
          <>
            <b className="text-[var(--fc-ink)]">started</b> {timeAgo(task.start_time)} ·{" "}
            {formatTimestamp(task.start_time)}
            {task.deadline && <> · deadline {formatTimestamp(task.deadline)}</>}
          </>
        ),
      });
    }
    if (task.state === "scheduled" && task.next_process_at) {
      events.push({
        key: "scheduled",
        node: (
          <>
            <b className="text-[var(--fc-ink)]">scheduled to run</b>{" "}
            {formatTimestamp(task.next_process_at)} —{" "}
            <span className="font-mono tabular-nums">{durationBefore(task.next_process_at)}</span>
          </>
        ),
      });
    }
    if (task.error_message) {
      events.push({
        key: "error",
        hot: true,
        node: (
          <>
            <b className="text-[var(--fc-ink)]">last error</b>{" "}
            <StateChip state="mut" label="asynq stores only the most recent" />
            <br />
            <code className="text-[11.5px] text-[var(--fc-crit)]">{task.error_message}</code>
            <br />
            <Stamp>
              failed {timeAgo(task.last_failed_at)} · attempt {task.retried} of {task.max_retry}
            </Stamp>
          </>
        ),
      });
    }
    if (task.state === "retry" && task.next_process_at) {
      events.push({
        key: "retry",
        node: (
          <>
            <b className="text-[var(--fc-ink)]">next retry</b>{" "}
            {formatTimestamp(task.next_process_at)} —{" "}
            <span className="font-mono tabular-nums">{durationBefore(task.next_process_at)}</span>
          </>
        ),
      });
    }
    if (task.state === "archived") {
      events.push({
        key: "archived",
        node: task.last_failed_at ? (
          <>
            <b className="text-[var(--fc-ink)]">died</b> {timeAgo(task.last_failed_at)} ·{" "}
            {formatTimestamp(task.last_failed_at)}
          </>
        ) : (
          <>
            <b className="text-[var(--fc-ink)]">archived</b> —{" "}
            <Stamp>archive time not stored for manual archives</Stamp>
          </>
        ),
      });
    }
    if (task.state === "completed" && task.completed_at) {
      events.push({
        key: "completed",
        node: (
          <>
            <b className="text-[var(--fc-ink)]">completed</b> {timeAgo(task.completed_at)} ·{" "}
            {formatTimestamp(task.completed_at)}
            {task.ttl_seconds > 0 ? (
              <>
                {" "}· result expires in{" "}
                <span className="font-mono tabular-nums">
                  {stringifyDuration(durationFromSeconds(task.ttl_seconds))}
                </span>
              </>
            ) : (
              <> · result retention expired</>
            )}
          </>
        ),
      });
    }
    // Run duration is only knowable when the worker adopted the observe
    // middleware — asynq stores no start/end for finished tasks. Absent
    // observed data adds nothing here.
    if (observed?.present && observed.summary) {
      events.push({
        key: "observed-run",
        node: (
          <>
            <b className="text-[var(--fc-ink)]">last run</b>{" "}
            <span className="font-mono tabular-nums">
              {stringifyDurationMs(observed.summary.last_duration_ms)}
            </span>{" "}
            · <Stamp>observed</Stamp>
          </>
        ),
      });
    }
  }

  const payloadBytes = task ? new TextEncoder().encode(task.payload).length : 0;

  return (
    <>
      {/* Scrim */}
      <div
        className="fixed inset-0 z-40 bg-[rgba(4,8,14,0.55)]"
        onClick={onClose}
        aria-hidden="true"
      />

      {/* Panel */}
      <aside
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={`Task ${peek.id} in queue ${peek.queue}`}
        tabIndex={-1}
        onKeyDown={trapTab}
        className="fc-slide-in fixed inset-y-0 right-0 z-50 flex w-[640px] max-w-[92vw] flex-col border-l border-[var(--fc-line)] bg-[var(--fc-panel)] text-[13px] text-[var(--fc-ink)] shadow-[var(--fc-shadow)] outline-none"
      >
        {/* Header */}
        <div className="flex items-center gap-2.5 border-b border-[var(--fc-line)] px-4 py-3">
          {task ? <StateChip state={task.state} /> : <StateChip state="mut" label="…" />}
          {task?.is_orphaned && <StateChip state="archived" label="orphaned" />}
          <span
            className="min-w-0 truncate font-mono text-[13px] font-semibold"
            title={peek.id}
          >
            {peek.id}
          </span>
          <button
            onClick={copyId}
            aria-label="Copy task ID"
            className="shrink-0 text-[var(--fc-ink3)] transition-colors hover:text-[var(--fc-ink)]"
          >
            {copied ? <Check size={13} className="text-[var(--fc-good)]" /> : <Copy size={13} />}
          </button>
          {task && <StateChip state="mut" label={task.type} />}
          <span className="flex-1" />
          <DrawerButton
            onClick={() => prevTask && onPeek({ queue: prevTask.queue, id: prevTask.id })}
            disabled={!prevTask}
            title="Previous in results ( [ )"
          >
            <ChevronLeft size={12} /> prev
          </DrawerButton>
          <DrawerButton
            onClick={() => nextTask && onPeek({ queue: nextTask.queue, id: nextTask.id })}
            disabled={!nextTask}
            title="Next in results ( ] )"
          >
            next <ChevronRight size={12} />
          </DrawerButton>
          <DrawerButton onClick={onClose} aria-label="Close drawer" className="border-0 bg-transparent">
            <X size={14} />
          </DrawerButton>
        </div>

        {/* Tabs (Flow only when the payload carries a correlation key) */}
        <div className="flex gap-0.5 border-b border-[var(--fc-line)] px-4">
          {(["detail", ...(corr ? (["flow"] as const) : [])] as Array<"detail" | "flow">).map(
            (t) => (
              <button
                key={t}
                onClick={() => setTab(t)}
                className={cn(
                  "flex items-center gap-1.5 border-b-2 px-3 py-2 text-xs transition-colors",
                  tab === t
                    ? "border-[var(--fc-acc)] font-semibold text-[var(--fc-acc)]"
                    : "border-transparent text-[var(--fc-ink2)] hover:text-[var(--fc-ink)]"
                )}
              >
                {t === "detail" ? (
                  "Detail"
                ) : (
                  <>
                    Flow · <span className="font-mono">{corr!.value}</span>
                    {flow && <span className="font-mono text-[11px]">{flow.tasks.length}</span>}
                  </>
                )}
              </button>
            )
          )}
        </div>

        {/* Body */}
        <div className="flex-1 overflow-y-auto px-4 py-4">
          {fetchErr && !task ? (
            <div className="flex items-start gap-2 rounded-md border border-[var(--fc-crit)] bg-[var(--fc-crit-bg)] p-3 text-xs text-[var(--fc-crit)]">
              <AlertCircle size={14} className="mt-0.5 shrink-0" />
              <div>
                <div className="font-semibold">Task not found</div>
                <div className="mt-0.5">{fetchErr}</div>
                <div className="mt-1 text-[var(--fc-ink2)]">
                  It may have completed and been deleted, or moved state since this link was made.
                </div>
              </div>
            </div>
          ) : !task ? (
            // Loading skeleton
            <div className="space-y-4" aria-busy="true">
              <div className="h-44 animate-pulse rounded-md bg-[var(--fc-raise)]" />
              <div className="h-24 animate-pulse rounded-md bg-[var(--fc-raise)]" />
              <div className="h-32 animate-pulse rounded-md bg-[var(--fc-raise)]" />
            </div>
          ) : tab === "detail" ? (
            <>
              {fetchErr && (
                <div className="mb-4 rounded-md border border-[var(--fc-warn)] bg-[var(--fc-warn-bg)] px-3 py-2 text-xs text-[var(--fc-warn)]">
                  Refresh failed — showing the last loaded snapshot. {fetchErr}
                </div>
              )}

              {/* Journey */}
              <section className="mb-5">
                <SectionTitle>Journey</SectionTitle>
                <JourneyGraph task={task} />
                <Stamp className="mt-1.5 block">
                  path reconstructed from stored fields · untraversed states dimmed · loop count =
                  retries
                </Stamp>
              </section>

              {/* Lifecycle */}
              <section className="mb-5">
                <SectionTitle>Lifecycle</SectionTitle>
                <div className="ml-1.5 flex flex-col border-l-2 border-[var(--fc-line)] pl-4">
                  {events.map((ev) => (
                    <div key={ev.key} className="relative pb-2.5 pt-1 text-xs text-[var(--fc-ink2)]">
                      <span
                        className={cn(
                          "absolute -left-[21.5px] top-[9px] h-[9px] w-[9px] rounded-full border-2 border-[var(--fc-panel)]",
                          ev.hot ? "bg-[var(--fc-crit)]" : "bg-[var(--fc-ink3)]"
                        )}
                        aria-hidden="true"
                      />
                      {ev.node}
                    </div>
                  ))}
                </div>
              </section>

              {/* Attempt history — only what the observe middleware recorded;
                  no section at all when absent (§7 honesty ledger). */}
              {observed?.present && observed.attempts.length > 0 && (
                <section className="mb-5">
                  <SectionTitle>
                    Attempt history · {observed.attempts.length}
                    {(observed.summary?.total_attempts ?? 0) > observed.attempts.length &&
                      ` of ${observed.summary!.total_attempts}`}
                  </SectionTitle>
                  <div className="flex flex-col gap-1">
                    {observed.attempts.map((a) => (
                      <div
                        key={`${a.n}:${a.start}`}
                        className="flex flex-wrap items-baseline gap-x-2.5 gap-y-0.5 rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-2.5 py-1.5 text-xs"
                      >
                        <span className="font-mono tabular-nums text-[var(--fc-ink3)]">
                          #{a.n}
                        </span>
                        <span className="text-[var(--fc-ink2)]">{timeAgo(a.start)}</span>
                        <span className="font-mono tabular-nums">
                          {stringifyDurationMs(a.duration_ms)}
                        </span>
                        <StateChip
                          state={a.outcome === "ok" ? "completed" : "archived"}
                          label={a.outcome}
                        />
                        <span className="flex-1" />
                        <span className="font-mono text-[11px] text-[var(--fc-ink3)]">
                          {a.worker}
                        </span>
                        {a.error && (
                          <code className="w-full text-[11px] text-[var(--fc-crit)]">
                            {a.error}
                          </code>
                        )}
                      </div>
                    ))}
                  </div>
                  <Stamp className="mt-1.5 block">
                    recorded by observe middleware — attempts before adoption are not shown
                  </Stamp>
                </section>
              )}

              {/* Task fields — every field is a pivot */}
              <section className="mb-5">
                <SectionTitle>Task</SectionTitle>
                <dl className="grid grid-cols-[130px_1fr] gap-x-3 gap-y-1 text-xs">
                  <dt className="text-[var(--fc-ink3)]">queue</dt>
                  <dd>
                    <PivotLink onClick={() => onPivot({ queue: task.queue })}>
                      {task.queue}
                    </PivotLink>
                  </dd>
                  <dt className="text-[var(--fc-ink3)]">type</dt>
                  <dd className="flex items-baseline gap-2">
                    <PivotLink onClick={() => onPivot({ q: task.type })}>{task.type}</PivotLink>
                    <Stamp>→ find all like this</Stamp>
                  </dd>
                  <dt className="text-[var(--fc-ink3)]">retried</dt>
                  <dd className="font-mono tabular-nums">
                    {task.retried} / {task.max_retry}
                  </dd>
                  {task.timeout_seconds > 0 && (
                    <>
                      <dt className="text-[var(--fc-ink3)]">timeout</dt>
                      <dd className="font-mono tabular-nums">{task.timeout_seconds}s</dd>
                    </>
                  )}
                  {task.deadline && (
                    <>
                      <dt className="text-[var(--fc-ink3)]">deadline</dt>
                      <dd className="font-mono tabular-nums">{formatTimestamp(task.deadline)}</dd>
                    </>
                  )}
                  <dt className="text-[var(--fc-ink3)]">group</dt>
                  <dd className={cn("font-mono", !task.group && "text-[var(--fc-ink3)]")}>
                    {task.group || "—"}
                  </dd>
                </dl>
                {metaPairs.length > 0 && (
                  <div className="mt-2.5">
                    <Stamp className="mr-1.5">metadata · pivots:</Stamp>
                    <span className="inline-flex flex-wrap gap-1.5 align-middle">
                      {metaPairs.map((p) => (
                        <button
                          key={metaId(p)}
                          onClick={() => onPivot({ meta: p })}
                          title={`Filter console by ${metaId(p)}`}
                          className="inline-flex items-center rounded-full border border-[var(--fc-line)] px-2 py-0.5 font-mono text-[11px] text-[var(--fc-ink2)] transition-colors hover:border-[var(--fc-acc)] hover:text-[var(--fc-acc)]"
                        >
                          {p.key}: {p.value}
                        </button>
                      ))}
                    </span>
                  </div>
                )}
              </section>

              {/* Payload */}
              <section className="mb-5">
                <SectionTitle>
                  Payload{task.payload ? ` · ${payloadBytes} B` : ""}
                  {task.payload && (
                    <button
                      onClick={copyPayload}
                      aria-label="Copy payload"
                      title="Copy payload"
                      className="ml-2 align-middle text-[var(--fc-ink3)] transition-colors hover:text-[var(--fc-ink)]"
                    >
                      {payloadCopied ? (
                        <Check size={12} className="text-[var(--fc-good)]" />
                      ) : (
                        <Copy size={12} />
                      )}
                    </button>
                  )}
                  {corr && (
                    <button
                      onClick={() => setTab("flow")}
                      className="ml-2 normal-case tracking-normal text-[var(--fc-acc)] hover:underline"
                    >
                      → flow via {corr.key}
                    </button>
                  )}
                </SectionTitle>
                {task.payload ? (
                  <>
                    <div className="overflow-x-auto rounded-md border border-[var(--fc-line)] bg-[var(--fc-bg)] p-2 text-xs">
                      <SyntaxHighlighter>{prettifyPayload(task.payload)}</SyntaxHighlighter>
                    </div>
                    {/* Honest server-cap note (upstream hibiken/asynqmon#301) */}
                    {payloadTruncated && (
                      <Stamp className="mt-1 block">
                        payload truncated at {payloadDetailLimit.toLocaleString("en-US")} chars
                        by the server (--max-detail-payload-length)
                      </Stamp>
                    )}
                  </>
                ) : (
                  <Stamp>No payload</Stamp>
                )}
              </section>

              {/* Result (completed only) */}
              {task.state === "completed" && (
                <section className="mb-5">
                  <SectionTitle>Result</SectionTitle>
                  {task.result ? (
                    <div className="overflow-x-auto rounded-md border border-[var(--fc-line)] bg-[var(--fc-bg)] p-2 text-xs">
                      <SyntaxHighlighter>{prettifyPayload(task.result)}</SyntaxHighlighter>
                    </div>
                  ) : (
                    <Stamp>No result stored</Stamp>
                  )}
                </section>
              )}

              {/* Actions */}
              {(canRun || canArchive || canDelete || canCancel || canClone) && (
                <section className="flex items-center gap-2 border-t border-[var(--fc-line2)] pt-4">
                  {canRun && (
                    <DrawerButton onClick={doRun} className="border-[var(--fc-acc)] bg-[var(--fc-acc)] text-[var(--fc-acc-fg)] hover:border-[var(--fc-acc)]">
                      <Play size={12} /> Run now
                    </DrawerButton>
                  )}
                  {canCancel && (
                    <DrawerButton onClick={doCancel}>
                      <X size={12} /> Cancel task
                    </DrawerButton>
                  )}
                  {canArchive && (
                    <DrawerButton onClick={doArchive}>
                      <Archive size={12} /> Archive
                    </DrawerButton>
                  )}
                  {canClone && (
                    <DrawerButton
                      onClick={() => setCloneOpen(true)}
                      title="Create a new task from this one (§5.10 enqueue)"
                    >
                      <CopyPlus size={12} /> Clone &amp; edit…
                    </DrawerButton>
                  )}
                  {canDelete && (
                    <DrawerButton
                      onClick={() => setConfirmDelete(true)}
                      className="ml-auto border-[var(--fc-crit)] text-[var(--fc-crit)] hover:border-[var(--fc-crit)]"
                    >
                      <Trash2 size={12} /> Delete
                    </DrawerButton>
                  )}
                  <ConfirmDialog
                    open={confirmDelete}
                    title="Delete Task"
                    description={
                      <>
                        Delete task <strong className="font-mono">{peek.id}</strong> from queue{" "}
                        <strong>{peek.queue}</strong>? This action cannot be undone.
                      </>
                    }
                    onConfirm={() => {
                      setConfirmDelete(false);
                      doDelete();
                    }}
                    onClose={() => setConfirmDelete(false)}
                  />
                  {canClone && task && (
                    <CloneEnqueueModal
                      open={cloneOpen}
                      sourceTask={task}
                      onClose={() => setCloneOpen(false)}
                      onEnqueued={(q, id) => {
                        setCloneOpen(false);
                        onPeek({ queue: q, id });
                      }}
                    />
                  )}
                </section>
              )}
            </>
          ) : (
            // Flow tab
            <section>
              <SectionTitle>
                Flow · correlation <span className="font-mono normal-case">{corr?.value}</span>
              </SectionTitle>
              {flowErr ? (
                <div className="rounded-md border border-[var(--fc-crit)] bg-[var(--fc-crit-bg)] p-3 text-xs text-[var(--fc-crit)]">
                  {flowErr}
                </div>
              ) : flowLoading || !flow ? (
                <div className="space-y-2" aria-busy="true">
                  {Array.from({ length: 4 }, (_, i) => (
                    <div key={i} className="h-9 animate-pulse rounded-md bg-[var(--fc-raise)]" />
                  ))}
                </div>
              ) : flow.tasks.length === 0 ? (
                <Stamp>No other tasks share this {corr?.key}.</Stamp>
              ) : (
                <div className="flex flex-col gap-1">
                  {flow.tasks.map((t, i) => {
                    const isThis = t.id === peek.id && t.queue === peek.queue;
                    const ts = observedAt(t);
                    return (
                      <div key={`${t.queue}:${t.id}`}>
                        {i > 0 && (
                          <div className="ml-5 py-0.5 text-[11px] text-[var(--fc-ink3)]" aria-hidden="true">
                            ↓
                          </div>
                        )}
                        <button
                          onClick={() => !isThis && onPeek({ queue: t.queue, id: t.id })}
                          disabled={isThis}
                          className={cn(
                            "flex w-full items-center gap-2.5 rounded-md border bg-[var(--fc-raise)] px-2.5 py-2 text-left text-xs",
                            isThis
                              ? "border-[var(--fc-acc)]"
                              : "border-[var(--fc-line)] transition-colors hover:border-[var(--fc-ink3)]"
                          )}
                        >
                          <StateChip state={t.state} />
                          <span className="min-w-0 truncate font-mono">{t.type}</span>
                          <span className="truncate font-mono text-[11px] text-[var(--fc-ink3)]">
                            {t.queue}
                          </span>
                          <span className="flex-1" />
                          {isThis && <Stamp>this task</Stamp>}
                          <span className="shrink-0 font-mono text-[11px] tabular-nums text-[var(--fc-ink3)]">
                            {ts ? timeAgo(ts) : "no stored time"}
                          </span>
                        </button>
                      </div>
                    );
                  })}
                  <Stamp className="mt-1 block">
                    {flow.total} task{flow.total === 1 ? "" : "s"} share
                    {flow.total === 1 ? "s" : ""}{" "}
                    <span className="font-mono">
                      {corr?.key}={corr?.value}
                    </span>{" "}
                    across {flow.queues} queue{flow.queues === 1 ? "" : "s"}
                  </Stamp>
                  {(flow.total > flow.tasks.length || flow.truncated) && (
                    <Stamp className="block">
                      showing {flow.tasks.length}
                      {flow.total > flow.tasks.length ? ` of ${flow.total}` : ""} · search caps
                      reached — the chain may be incomplete
                    </Stamp>
                  )}
                </div>
              )}
              <div className="mt-3 flex gap-2 rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-3 py-2 text-[11.5px] text-[var(--fc-ink2)]">
                <span className="font-bold text-[var(--fc-acc)]" aria-hidden="true">
                  ℹ
                </span>
                <span>
                  Flow reconstructed from shared{" "}
                  <code className="font-mono">{corr?.key}</code> — ordering inferred from
                  timestamps, not causal links; asynq stores no parent/child relationships.
                </span>
              </div>
            </section>
          )}
        </div>
      </aside>
    </>
  );
}
