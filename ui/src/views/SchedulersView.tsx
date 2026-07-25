// Schedulers screen (build contract §3.8): "did last night's crons run — and
// did they succeed?"
//
//   - Entries table: live entries ∪ persisted snapshots merged on the stable
//     entry key (§5.12), filterable by type/spec. A snapshot with no live
//     counterpart renders as SCHEDULER GONE (crit stripe + last-seen stamp) —
//     the dead scheduler is visible instead of silently vanishing.
//   - Target-queue chip parsed from the entry options (default queue handled
//     explicitly — no more regex-dropped links) and a retention badge.
//   - Row expand → outcome trace: enqueue events joined to current task
//     state, three-valued honestly (SUCCEEDED / FAILED / UNKNOWN, plus
//     PENDING for in-flight), with the Retention=0 guidance rendered
//     verbatim when success is unknowable, and asynq's 1,000-event history
//     cap labeled.

import React, { useCallback, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ChevronDown, ChevronRight, Loader2 } from "lucide-react";
import { useSelector } from "react-redux";
import { AppState } from "../store";
import { usePolling, useQuery } from "../hooks";
import {
  listSchedulers,
  getSchedulerOutcomes,
  SchedulerRow,
  SchedulerOutcomesResponse,
} from "../api-fleet";
import {
  filterSchedulerRows,
  goneLabel,
  isGone,
  outcomeStamp,
  outcomeTone,
  queueFromOptions,
  RETENTION_GUIDANCE,
  retentionLabel,
  retentionSecondsFromOptions,
  showRetentionGuidance,
  sortSchedulerRows,
} from "../lib/schedulers";
import { taskDetailsPath } from "../paths";
import { timeAgo, durationBefore, uuidPrefix, toErrorString } from "../utils";
import { FcChip, MicroLabel } from "../components/FleetBits";
import SyntaxHighlighter from "../components/SyntaxHighlighter";
import { clickableRowClass, clickableRowProps, cn } from "../lib/utils";

const panelClass = "rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]";
const theadClass =
  "whitespace-nowrap border-b border-[var(--fc-line)] px-2.5 py-[7px] text-left text-[10.5px] font-semibold uppercase tracking-[0.07em] text-[var(--fc-ink3)]";
const cellClass = "border-b border-[var(--fc-line2)] px-2.5 py-2 align-middle";

/**************************************************************
                    Outcome trace (row expand)
 **************************************************************/

function OutcomeTrace({ stableKey }: { stableKey: string }) {
  const navigate = useNavigate();
  const [resp, setResp] = useState<SchedulerOutcomesResponse | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setResp(null);
    setError("");
    getSchedulerOutcomes(stableKey)
      .then((r) => active && setResp(r))
      .catch((e) => active && setError(toErrorString(e)));
    return () => {
      active = false;
    };
  }, [stableKey]);

  if (error !== "") {
    return (
      <div className="py-2 text-xs text-[var(--fc-crit)]">
        Could not load the outcome trace: {error}
      </div>
    );
  }
  if (resp === null) {
    return (
      <div className="flex items-center gap-2 py-2 text-xs text-[var(--fc-ink3)]">
        <Loader2 size={13} className="animate-spin" /> Joining enqueue history to task state…
      </div>
    );
  }

  const events = resp.events ?? [];
  return (
    <div className="rounded-md border border-[var(--fc-line)] bg-[var(--fc-bg,transparent)]">
      <div className="flex flex-wrap items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-1.5">
        <MicroLabel>Outcome trace</MicroLabel>
        <FcChip tone="mut">queue {resp.queue}</FcChip>
        <FcChip tone={resp.retention_seconds === 0 ? "warn" : "mut"}>
          {retentionLabel(resp.retention_seconds)}
        </FcChip>
        <span className="flex-1" />
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          newest {events.length} of the entry's enqueue history — asynq caps history at{" "}
          {(resp.history_cap ?? 1000).toLocaleString("en-US")} events
        </span>
      </div>

      {showRetentionGuidance(resp.retention_seconds, events) && (
        <div
          role="note"
          className="mx-3 my-2 flex items-start gap-2 rounded-md border border-[var(--fc-warn)] bg-[var(--fc-warn-bg)] px-3 py-2 text-xs text-[var(--fc-ink)]"
        >
          <span aria-hidden="true">ℹ</span>
          <span>
            <b>Outcome is unknowable: </b>
            {RETENTION_GUIDANCE}
          </span>
        </div>
      )}

      {events.length === 0 ? (
        <div className="px-3 py-2 text-xs text-[var(--fc-ink3)]">
          No enqueue history. asynq clears an entry's history when its scheduler shuts down
          cleanly; history from a crashed scheduler survives.
        </div>
      ) : (
        <div className="divide-y divide-[var(--fc-line2)]">
          {events.map((ev) => (
            <div
              key={ev.task_id}
              className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-1.5 text-xs"
            >
              <FcChip tone={outcomeTone(ev.outcome)}>{ev.outcome}</FcChip>
              <button
                onClick={() => navigate(taskDetailsPath(resp.queue, ev.task_id))}
                className="font-mono text-[var(--fc-acc)] hover:underline"
                title={ev.task_id}
              >
                {uuidPrefix(ev.task_id)}
              </button>
              <span className="text-[var(--fc-ink3)]">{outcomeStamp(ev)}</span>
              <span className="flex-1" />
              <span className="text-[var(--fc-ink3)]">enqueued {timeAgo(ev.enqueued_at)}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/**************************************************************
                        Entries table
 **************************************************************/

function SchedulerEntryRow({
  row,
  isOpen,
  onToggle,
  nowMs,
}: {
  row: SchedulerRow;
  isOpen: boolean;
  onToggle: () => void;
  nowMs: number;
}) {
  const gone = isGone(row);
  const queue = queueFromOptions(row.entry.options);
  const retentionSec = retentionSecondsFromOptions(row.entry.options);
  return (
    <React.Fragment>
      <tr
        className={cn(clickableRowClass, "hover:bg-[var(--fc-raise)]")}
        aria-expanded={isOpen}
        {...clickableRowProps(onToggle)}
      >
        <td className={cn(cellClass, "relative w-8 pr-0 text-[var(--fc-ink3)]")}>
          {gone && (
            <span
              aria-hidden="true"
              className="absolute left-0 top-0 h-full w-[3px] bg-[var(--fc-crit)]"
            />
          )}
          {isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </td>
        <td className={cellClass}>
          <span
            className="font-mono text-xs font-medium text-[var(--fc-ink)]"
            title={row.entry.task_payload || "{}"}
          >
            {row.entry.task_type}
          </span>
        </td>
        <td className={cellClass}>
          <span className="rounded bg-[var(--fc-raise)] px-1.5 py-0.5 font-mono text-[11px] text-[var(--fc-ink2)]">
            {row.entry.spec}
          </span>
        </td>
        <td className={cellClass}>
          <FcChip tone="mut" className="font-mono normal-case">
            {queue}
          </FcChip>
        </td>
        <td className={cellClass}>
          <FcChip
            tone={retentionSec === 0 ? "warn" : "mut"}
            title={
              retentionSec === 0
                ? "Retention=0: completions are deleted immediately — outcomes are unknowable"
                : "Completed tasks are retained; outcomes stay traceable"
            }
          >
            {retentionLabel(retentionSec)}
          </FcChip>
        </td>
        <td className={cn(cellClass, "whitespace-nowrap text-xs text-[var(--fc-ink2)]")}>
          {gone ? "—" : durationBefore(row.entry.next_enqueue_at)}
        </td>
        <td className={cn(cellClass, "whitespace-nowrap text-xs text-[var(--fc-ink2)]")}>
          {row.entry.prev_enqueue_at ? timeAgo(row.entry.prev_enqueue_at) : "–"}
        </td>
        <td className={cn(cellClass, "whitespace-nowrap")}>
          {gone ? (
            <span className="inline-flex items-center gap-1.5">
              <FcChip tone="crit">SCHEDULER GONE</FcChip>
              <span className="text-[10.5px] text-[var(--fc-ink3)]">
                {goneLabel(row.gone_since, nowMs)}
              </span>
            </span>
          ) : row.live ? (
            <FcChip tone="good">LIVE</FcChip>
          ) : (
            <FcChip tone="mut" title="Not in the latest heartbeat; not yet past the gone threshold">
              NOT HEARTBEATING
            </FcChip>
          )}
        </td>
      </tr>
      {isOpen && (
        <tr>
          <td className={cellClass} />
          <td colSpan={7} className={cn(cellClass, "py-2")}>
            {row.entry.task_payload && row.entry.task_payload !== "{}" && (
              <div className="mb-2">
                <SyntaxHighlighter>{row.entry.task_payload}</SyntaxHighlighter>
              </div>
            )}
            <OutcomeTrace stableKey={row.stable_key} />
          </td>
        </tr>
      )}
    </React.Fragment>
  );
}

/**************************************************************
                        SchedulersView
 **************************************************************/

export default function SchedulersView() {
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const query = useQuery();

  const [rows, setRows] = useState<SchedulerRow[] | null>(null);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState(() => query.get("filter") ?? "");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [nowMs, setNowMs] = useState(Date.now());

  const fetchRows = useCallback(() => {
    listSchedulers()
      .then((r) => {
        setRows(r.entries ?? []);
        setError("");
        setNowMs(Date.now());
      })
      .catch((e) => setError(toErrorString(e)));
  }, []);
  usePolling(fetchRows, pollInterval);

  const toggle = (key: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const visible = rows === null ? [] : sortSchedulerRows(filterSchedulerRows(rows, filter));
  const goneCount = (rows ?? []).filter(isGone).length;

  return (
    <div className="mx-auto max-w-7xl px-4 py-8">
      {error !== "" && (
        <div
          role="alert"
          className="mb-3 rounded-md border border-[var(--fc-crit)] bg-[var(--fc-crit-bg)] px-3 py-2 text-sm text-[var(--fc-ink)]"
        >
          Could not retrieve scheduler data: {error}
        </div>
      )}
      <div className={cn(panelClass, "overflow-x-auto")}>
        <div className="flex flex-wrap items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2">
          <span className="text-xs font-semibold text-[var(--fc-ink)]">Entries</span>
          <span className="text-[10.5px] text-[var(--fc-ink3)]">
            live ∪ snapshots (stable-key) · history capped at 1,000 events by asynq
          </span>
          {goneCount > 0 && (
            <FcChip tone="crit">
              {goneCount} GONE
            </FcChip>
          )}
          <span className="flex-1" />
          <input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="filter by type or spec…"
            aria-label="Filter scheduler entries by type or spec"
            className="w-52 rounded-md border border-[var(--fc-line)] bg-transparent px-2 py-1 text-xs text-[var(--fc-ink)] placeholder:text-[var(--fc-ink3)] focus:outline-none focus:ring-1 focus:ring-[var(--fc-acc)]"
          />
        </div>
        {rows === null && error === "" ? (
          <div className="flex items-center gap-2 px-4 py-6 text-sm text-[var(--fc-ink3)]">
            <Loader2 size={15} className="animate-spin" /> Loading scheduler entries…
          </div>
        ) : visible.length === 0 ? (
          <div className="px-4 py-8 text-center text-sm text-[var(--fc-ink3)]">
            {filter.trim() !== ""
              ? "No entries match the filter"
              : "No scheduler entries observed — live or in snapshots"}
          </div>
        ) : (
          <table className="w-full border-collapse text-left">
            <thead>
              <tr>
                <th className={cn(theadClass, "w-8")} />
                <th className={theadClass}>Entry · type</th>
                <th className={theadClass}>Spec</th>
                <th className={theadClass}>Queue</th>
                <th className={theadClass}>Retention</th>
                <th className={theadClass}>Next</th>
                <th className={theadClass}>Prev</th>
                <th className={theadClass}>Status</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((row) => (
                <SchedulerEntryRow
                  key={row.stable_key}
                  row={row}
                  isOpen={expanded.has(row.stable_key)}
                  onToggle={() => toggle(row.stable_key)}
                  nowMs={nowMs}
                />
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
