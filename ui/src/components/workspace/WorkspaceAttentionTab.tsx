// Queue Workspace default tab (build contract §3.3): the synthesized anomaly
// view — summary chips, retry tasks sorted by next fire with live countdowns,
// orphaned actives, past-due scheduled. When navigated from a Fleet finding
// (?focus=<state>) the section for that state arrives highlighted and
// scrolled into view. Never lands on `active`.

import { useEffect, useMemo, useRef } from "react";
import { Link } from "react-router-dom";
import type { TaskInfo } from "../../api";
import { taskDetailsPath } from "../../paths";
import { humanizeMs } from "../../lib/fleet";
import {
  FocusState,
  RetryRow,
  WorkspaceAttentionItem,
  formatCountdown,
} from "../../lib/workspace";
import { FcChip, FcTone, MicroLabel } from "../FleetBits";
import { Button } from "../ui/button";
import { cn } from "../../lib/utils";

interface Props {
  qname: string;
  items: WorkspaceAttentionItem[];
  retryRows: RetryRow[];
  retryTotal: number;
  orphans: TaskInfo[];
  pastDue: TaskInfo[];
  focus: FocusState | null;
  loading: boolean;
  // §3.3 empty state sentence; shown when nothing is anomalous.
  healthySentence: string;
  nowMs: number;
  onOpenTab: (state: FocusState) => void;
  // Opens the bulk-job modal verb=run scope queue=X state=retry (§4.3 paced
  // re-drain — the only honest retry-storm flattening, §7.7).
  onPaceRetries: () => void;
}

const CHIP_TONE: Record<WorkspaceAttentionItem["tone"], FcTone> = {
  crit: "crit",
  warn: "warn",
  mut: "mut",
};

function Section({
  focused,
  children,
}: {
  focused: boolean;
  children: React.ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (focused && ref.current) {
      ref.current.scrollIntoView({ block: "nearest" });
    }
  }, [focused]);
  return (
    <div
      ref={ref}
      className={cn(
        "border-b border-[var(--fc-line2)] px-4 py-3 last:border-b-0",
        focused && "rounded-sm ring-2 ring-inset ring-[var(--fc-acc)]"
      )}
    >
      {focused && (
        <div className="mb-1.5 text-[10.5px] font-semibold uppercase tracking-[0.08em] text-[var(--fc-acc)]">
          focused from Fleet finding
        </div>
      )}
      {children}
    </div>
  );
}

export default function WorkspaceAttentionTab({
  qname,
  items,
  retryRows,
  retryTotal,
  orphans,
  pastDue,
  focus,
  loading,
  healthySentence,
  nowMs,
  onOpenTab,
  onPaceRetries,
}: Props) {
  const hasAnomalies =
    items.length > 0 || retryRows.length > 0 || orphans.length > 0 || pastDue.length > 0;

  // Which section carries the focus ring: the state the finding named.
  const itemStates = useMemo(() => new Set(items.map((i) => i.focusState)), [items]);
  const focusOnSummary =
    focus !== null &&
    itemStates.has(focus) &&
    !(focus === "retry" && retryRows.length > 0) &&
    !(focus === "active" && orphans.length > 0) &&
    !(focus === "scheduled" && pastDue.length > 0);

  if (loading && !hasAnomalies) {
    return (
      <div className="px-4 py-8 text-center text-sm text-[var(--fc-ink3)]">
        Synthesizing anomalies…
      </div>
    );
  }

  if (!hasAnomalies) {
    return (
      <div className="px-4 py-10 text-center">
        <div className="text-sm font-medium text-[var(--fc-good)]">{healthySentence}</div>
        <div className="mt-1 text-xs text-[var(--fc-ink3)]">
          Anomalies appear here the moment this queue has any — retries about to fire,
          orphaned actives, past-due scheduled work, archive pressure.
        </div>
      </div>
    );
  }

  return (
    <div>
      {/* Summary anomaly chips */}
      {items.length > 0 && (
        <Section focused={focusOnSummary}>
          <div className="flex flex-col gap-2">
            {items.map((item) => (
              <div key={item.kind} className="flex flex-wrap items-center gap-2 text-xs">
                <FcChip tone={CHIP_TONE[item.tone]}>{item.chip}</FcChip>
                <span className="text-[var(--fc-ink2)]">{item.sentence}</span>
                <button
                  onClick={() => onOpenTab(item.focusState)}
                  className="text-[var(--fc-acc)] hover:underline"
                >
                  open {item.focusState} →
                </button>
              </div>
            ))}
          </div>
        </Section>
      )}

      {/* Retry tasks by next fire, live countdowns (§3.3) */}
      {retryRows.length > 0 && (
        <Section focused={focus === "retry"}>
          <div className="mb-2 flex items-center gap-2">
            <MicroLabel>next retries · sorted by next fire</MicroLabel>
            <span className="flex-1" />
            {!window.READ_ONLY && (
              <Button size="sm" variant="outline" className="h-6 text-[11px]" onClick={onPaceRetries}>
                Pace as job…
              </Button>
            )}
            <button
              onClick={() => onOpenTab("retry")}
              className="text-[11px] text-[var(--fc-acc)] hover:underline"
            >
              open retry tab →
            </button>
          </div>
          <table className="w-full text-xs">
            <tbody>
              {retryRows.map(({ task, countdownMs }) => (
                <tr key={task.id} className="border-t border-[var(--fc-line2)] first:border-t-0">
                  <td className="py-1 pr-3">
                    <Link
                      to={taskDetailsPath(qname, task.id)}
                      className="font-mono text-[var(--fc-acc)] hover:underline"
                    >
                      {task.id.split("-")[0]}…
                    </Link>
                  </td>
                  <td className="py-1 pr-3 font-mono text-[var(--fc-ink2)]">{task.type}</td>
                  <td className="py-1 pr-3 whitespace-nowrap tabular-nums">
                    <span title="attempt / max retry">
                      {task.retried}/{task.max_retry}
                    </span>
                  </td>
                  <td
                    className="max-w-[320px] truncate py-1 pr-3 font-mono text-[11px] text-[var(--fc-ink3)]"
                    title={task.error_message}
                  >
                    {task.error_message || "—"}
                  </td>
                  <td
                    className={cn(
                      "py-1 text-right font-mono tabular-nums",
                      countdownMs !== null && countdownMs <= 0 && "text-[var(--fc-warn)]"
                    )}
                  >
                    {formatCountdown(countdownMs)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {retryTotal > retryRows.length && (
            <div className="mt-1.5 text-[10.5px] text-[var(--fc-ink3)]">
              showing the {retryRows.length} soonest of {retryTotal.toLocaleString()} retries
            </div>
          )}
        </Section>
      )}

      {/* Orphaned actives */}
      {orphans.length > 0 && (
        <Section focused={focus === "active"}>
          <div className="mb-1.5 flex items-center gap-2">
            <FcChip tone="warn">ORPHANS {orphans.length}</FcChip>
            <span className="text-xs text-[var(--fc-ink2)]">
              active tasks with an expired lease — worker crash suspected
            </span>
            <span className="flex-1" />
            <button
              onClick={() => onOpenTab("active")}
              className="text-[11px] text-[var(--fc-acc)] hover:underline"
            >
              open active tab →
            </button>
          </div>
          <div className="flex flex-wrap items-center gap-2 text-[11.5px]">
            {orphans.slice(0, 8).map((t) => (
              <Link
                key={t.id}
                to={taskDetailsPath(qname, t.id)}
                className="font-mono text-[var(--fc-acc)] hover:underline"
              >
                {t.id.split("-")[0]}…
              </Link>
            ))}
            <span className="text-[10.5px] text-[var(--fc-ink3)]">
              {orphans.length > 8 ? `+${orphans.length - 8} more · ` : ""}
              asynq's recoverer requeues them on lease expiry — asynqmon shows it, it
              doesn't fake it
            </span>
          </div>
        </Section>
      )}

      {/* Past-due scheduled */}
      {pastDue.length > 0 && (
        <Section focused={focus === "scheduled"}>
          <div className="mb-1.5 flex items-center gap-2">
            <FcChip tone="warn">PAST-DUE {pastDue.length}</FcChip>
            <span className="text-xs text-[var(--fc-ink2)]">
              scheduled tasks whose fire time is behind now — forwarder may be stalled
            </span>
            <span className="flex-1" />
            <button
              onClick={() => onOpenTab("scheduled")}
              className="text-[11px] text-[var(--fc-acc)] hover:underline"
            >
              open scheduled tab →
            </button>
          </div>
          <table className="w-full text-xs">
            <tbody>
              {pastDue.slice(0, 8).map((t) => {
                const at = Date.parse(t.next_process_at);
                return (
                  <tr key={t.id} className="border-t border-[var(--fc-line2)] first:border-t-0">
                    <td className="py-1 pr-3">
                      <Link
                        to={taskDetailsPath(qname, t.id)}
                        className="font-mono text-[var(--fc-acc)] hover:underline"
                      >
                        {t.id.split("-")[0]}…
                      </Link>
                    </td>
                    <td className="py-1 pr-3 font-mono text-[var(--fc-ink2)]">{t.type}</td>
                    <td className="py-1 text-right font-mono tabular-nums text-[var(--fc-warn)]">
                      overdue {Number.isFinite(at) ? humanizeMs(nowMs - at) : "—"}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </Section>
      )}
    </div>
  );
}
