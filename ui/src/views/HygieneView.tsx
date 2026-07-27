// Hygiene screen (build contract §3.10): "what is rotting?" — four scheduled
// report cards (queue inventory, storage attribution, dead-letter digest,
// scheduler health), each viewable in-UI and deliverable by webhook:
//
//   - Card: summary chips from the last persisted report + generated-at
//     stamp, Run-now (spinner; allowed in read-only mode — reports are reads,
//     the trigger is audit-logged server-side), per-card schedule editor
//     (enabled + interval; read-only mode hides it — the PUT is blocked).
//   - Expand ("Open report") → the full latest report tables.
//   - Dead-letter rows carry the one-click "delete archived older than 30d"
//     action → standard BulkJobModal flow with the descriptor's AQL scope.
//   - Storage renders unsampled queues as "not sampled" — never 0 — and the
//     sampling method label verbatim (honest-numbers rule §2).
//   - Empty state (§3.10): "No reports scheduled — enable the weekly
//     inventory to start."
//
// The report body is server-generated and persisted; this view only reads
// and triggers.

import { useCallback, useState } from "react";
import { Loader2, Play } from "lucide-react";
import {
  DeadLetterAction,
  HygieneCard,
  HygieneKind,
  HygieneReport,
  getHygieneReport,
  listHygiene,
  putHygieneConfig,
  runHygieneReport,
} from "../api-hygiene";
import { usePolling } from "../hooks";
import {
  HYGIENE_KINDS,
  KIND_QUESTIONS,
  KIND_TITLES,
  activitySourceLabel,
  deadLetterActionScope,
  formatBytes,
  formatInterval,
  intervalOptions,
  nothingScheduled,
  summaryChips,
  validInterval,
} from "../lib/hygiene";
import { timeAgo, toErrorString } from "../utils";
import { FcChip, MicroLabel } from "../components/FleetBits";
import PageShell, { cellClass, panelClass, theadClass } from "../components/PageShell";
import BulkJobModal from "../components/BulkJobModal";
import { Button } from "../components/ui/button";
import { Switch } from "../components/ui/switch";
import { cn } from "../lib/utils";

const numCell = cn(cellClass, "text-right font-mono tabular-nums");

const fmt = (n: number | undefined | null) =>
  n === undefined || n === null || !Number.isFinite(n) ? "—" : n.toLocaleString("en-US");

/**************************************************************
                    Schedule editor
 **************************************************************/

function ScheduleEditor({
  card,
  onSaved,
}: {
  card: HygieneCard;
  onSaved: () => void;
}) {
  const [enabled, setEnabled] = useState(card.enabled);
  const [interval, setIntervalSec] = useState(card.interval_seconds);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const dirty = enabled !== card.enabled || interval !== card.interval_seconds;

  const save = async () => {
    if (!validInterval(interval) || saving) return;
    setSaving(true);
    setError("");
    try {
      await putHygieneConfig(card.kind, { enabled, interval_seconds: interval });
      onSaved();
    } catch (e) {
      setError(toErrorString(e as never));
    }
    setSaving(false);
  };

  return (
    <div className="flex flex-wrap items-center gap-2 border-t border-[var(--fc-line2)] pt-2">
      <label className="flex items-center gap-1.5 text-[11.5px] text-[var(--fc-ink2)]">
        <Switch
          checked={enabled}
          onCheckedChange={setEnabled}
          aria-label={`Schedule ${card.title}`}
        />
        scheduled
      </label>
      <select
        value={interval}
        onChange={(e) => setIntervalSec(Number(e.target.value))}
        aria-label={`${card.title} interval`}
        className="h-7 rounded-md border border-[var(--fc-line)] bg-[var(--fc-panel)] px-1.5 text-[11.5px] text-[var(--fc-ink)]"
      >
        {intervalOptions(card.interval_seconds).map((p) => (
          <option key={p.seconds} value={p.seconds}>
            {p.label}
          </option>
        ))}
      </select>
      {dirty && (
        <Button size="sm" variant="outline" className="h-7 px-2 text-[11.5px]" onClick={save} disabled={saving}>
          {saving ? "Saving…" : "Save schedule"}
        </Button>
      )}
      {error && <span className="text-[11px] text-[var(--fc-crit)]">{error}</span>}
    </div>
  );
}

/**************************************************************
                    Full-report tables
 **************************************************************/

function InventoryTable({ report }: { report: HygieneReport }) {
  const inv = report.inventory;
  if (!inv) return null;
  return (
    <div className="overflow-x-auto">
      {(inv.rows_truncated || inv.probes_capped) && (
        <div className="px-2.5 py-1.5 text-[11px] text-[var(--fc-warn)]">
          {inv.rows_truncated && `Showing the first rows of ${fmt(inv.queues_total)} queues. `}
          {inv.probes_capped &&
            `Dead-queue probing capped at ${fmt(inv.probe_cap)} candidates this run — unprobed queues stay unflagged.`}
        </div>
      )}
      <table className="w-full text-xs">
        <thead>
          <tr>
            <th className={theadClass}>Queue</th>
            <th className={cn(theadClass, "text-right")}>Size</th>
            <th className={cn(theadClass, "text-right")}>Consumers</th>
            <th className={theadClass}>Last activity</th>
            <th className={theadClass}>Flags</th>
          </tr>
        </thead>
        <tbody>
          {inv.rows.map((r) => (
            <tr key={r.queue}>
              <td className={cn(cellClass, "font-mono")}>{r.queue}</td>
              <td className={numCell}>{fmt(r.size)}</td>
              <td className={numCell}>{fmt(r.consumers)}</td>
              <td className={cellClass}>
                {r.last_activity_at ? timeAgo(r.last_activity_at) : "—"}
                <span className="ml-1.5 text-[10.5px] text-[var(--fc-ink3)]">
                  {activitySourceLabel(r.last_activity_source)}
                </span>
              </td>
              <td className={cellClass}>
                <span className="flex flex-wrap gap-1">
                  {r.dead_candidate && <FcChip tone="warn">dead candidate</FcChip>}
                  {r.zero_consumer_pending && <FcChip tone="crit">zero-consumer + pending</FcChip>}
                  {r.paused_over_7d && <FcChip tone="warn">paused &gt; 7d</FcChip>}
                </span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function StorageTable({ report }: { report: HygieneReport }) {
  const st = report.storage;
  if (!st) return null;
  return (
    <div className="space-y-3">
      <div className="px-2.5 text-[11px] leading-relaxed text-[var(--fc-ink3)]">{st.method}</div>
      <div className="overflow-x-auto">
        <MicroLabel className="px-2.5 pb-1">Memory by queue (estimate, sampled)</MicroLabel>
        <table className="w-full text-xs">
          <thead>
            <tr>
              <th className={theadClass}>Queue</th>
              <th className={cn(theadClass, "text-right")}>Tasks</th>
              <th className={cn(theadClass, "text-right")}>Estimated</th>
              <th className={cn(theadClass, "text-right")}>Avg task</th>
              <th className={theadClass}>Sampled</th>
            </tr>
          </thead>
          <tbody>
            {st.memory.map((m) => (
              <tr key={m.queue}>
                <td className={cn(cellClass, "font-mono")}>{m.queue}</td>
                <td className={numCell}>{fmt(m.tasks)}</td>
                <td className={numCell}>{formatBytes(m.estimated_bytes)}</td>
                <td className={numCell}>{formatBytes(m.avg_task_bytes)}</td>
                <td className={cellClass}>
                  {m.sampled_tasks} tasks · {timeAgo(m.sampled_at)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {st.not_sampled_queues > 0 && (
          <div className="px-2.5 py-1.5 text-[11px] text-[var(--fc-warn)]">
            {fmt(st.not_sampled_queues)} more queue{st.not_sampled_queues === 1 ? "" : "s"} with
            tasks were <b>not sampled</b> this run — no number is shown rather than a wrong one.
          </div>
        )}
      </div>

      <div className="overflow-x-auto">
        <MicroLabel className="px-2.5 pb-1">Top payload offenders (sampled)</MicroLabel>
        <table className="w-full text-xs">
          <thead>
            <tr>
              <th className={theadClass}>Queue</th>
              <th className={theadClass}>Task</th>
              <th className={theadClass}>State</th>
              <th className={cn(theadClass, "text-right")}>Encoded size</th>
            </tr>
          </thead>
          <tbody>
            {st.offenders.map((o) => (
              <tr key={`${o.queue}/${o.id}`}>
                <td className={cn(cellClass, "font-mono")}>{o.queue}</td>
                <td className={cn(cellClass, "font-mono")}>{o.id.slice(0, 8)}…</td>
                <td className={cellClass}>{o.state}</td>
                <td className={numCell}>{formatBytes(o.msg_bytes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="overflow-x-auto">
        <MicroLabel className="px-2.5 pb-1">
          Completed-retention pressure (exact — when the janitor reclaims)
        </MicroLabel>
        <table className="w-full text-xs">
          <thead>
            <tr>
              <th className={theadClass}>Queue</th>
              {st.retention.bucket_labels.map((b) => (
                <th key={b} className={cn(theadClass, "text-right")}>
                  {b}
                </th>
              ))}
              <th className={cn(theadClass, "text-right")}>Total</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td className={cn(cellClass, "font-semibold")}>fleet</td>
              {st.retention.fleet.counts.map((c, i) => (
                <td key={i} className={numCell}>
                  {fmt(c)}
                </td>
              ))}
              <td className={cn(numCell, "font-semibold")}>{fmt(st.retention.fleet.total)}</td>
            </tr>
            {st.retention.queues.map((r) => (
              <tr key={r.queue}>
                <td className={cn(cellClass, "font-mono")}>{r.queue}</td>
                {r.counts.map((c, i) => (
                  <td key={i} className={numCell}>
                    {fmt(c)}
                  </td>
                ))}
                <td className={numCell}>{fmt(r.total)}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {st.retention.queues_truncated && (
          <div className="px-2.5 py-1.5 text-[11px] text-[var(--fc-ink3)]">
            Per-queue rows are capped; the fleet row still sums every measured queue.
          </div>
        )}
      </div>
    </div>
  );
}

function DeadLetterTable({
  report,
  onAction,
}: {
  report: HygieneReport;
  onAction: (action: DeadLetterAction) => void;
}) {
  const dl = report.dead_letter;
  if (!dl) return null;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr>
            <th className={theadClass}>Queue</th>
            <th className={cn(theadClass, "text-right")}>Archived</th>
            {dl.bucket_labels.map((b) => (
              <th key={b} className={cn(theadClass, "text-right")}>
                {b}
              </th>
            ))}
            <th className={theadClass}>Top error clusters</th>
            <th className={theadClass}></th>
          </tr>
        </thead>
        <tbody>
          {dl.queues.map((q) => (
            <tr key={q.queue}>
              <td className={cn(cellClass, "font-mono")}>
                {q.queue}
                {q.at_trim_cap && (
                  <FcChip
                    tone="warn"
                    className="ml-1.5"
                    title="archive sits at asynq's 10k cap — data is being discarded; counts are lower bounds"
                  >
                    10k cap
                  </FcChip>
                )}
              </td>
              <td className={numCell}>{fmt(q.archived)}</td>
              {q.buckets.map((b, i) => (
                <td key={i} className={numCell}>
                  {fmt(b)}
                </td>
              ))}
              <td className={cellClass}>
                {q.clusters.length === 0 ? (
                  <span className="text-[var(--fc-ink3)]">no index data</span>
                ) : (
                  q.clusters.map((c) => (
                    <div key={c.sig} className="truncate font-mono text-[11px]" title={c.template}>
                      {c.lower_bound ? "≥" : ""}
                      {fmt(c.archived)} × {c.template}
                    </div>
                  ))
                )}
              </td>
              <td className={cellClass}>
                {q.action && (
                  <Button
                    size="sm"
                    variant="destructive"
                    className="h-6 px-2 text-[11px]"
                    onClick={() => onAction(q.action!)}
                  >
                    {q.action.label} ({fmt(q.action.estimate)}) as job…
                  </Button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {dl.queues_truncated && (
        <div className="px-2.5 py-1.5 text-[11px] text-[var(--fc-ink3)]">
          Showing the top {dl.queues.length} of {fmt(dl.queues_with_archived)} queues with archived
          tasks.
        </div>
      )}
    </div>
  );
}

function SchedulerHealthTable({ report }: { report: HygieneReport }) {
  const sh = report.scheduler_health;
  if (!sh) return null;
  const section = (
    title: string,
    tone: "crit" | "warn",
    rows: NonNullable<typeof sh>["gone"],
    extra: (r: (typeof rows)[number]) => string
  ) => (
    <div>
      <MicroLabel className="px-2.5 pb-1">{title}</MicroLabel>
      {rows.length === 0 ? (
        <div className="px-2.5 pb-2 text-[11.5px] text-[var(--fc-ink3)]">none</div>
      ) : (
        <table className="mb-2 w-full text-xs">
          <tbody>
            {rows.map((r) => (
              <tr key={`${title}-${r.stable_key}`}>
                <td className={cellClass}>
                  <FcChip tone={tone}>{r.task_type}</FcChip>
                </td>
                <td className={cn(cellClass, "font-mono")}>{r.spec}</td>
                <td className={cn(cellClass, "font-mono")}>→ {r.queue}</td>
                <td className={cn(cellClass, "text-[var(--fc-ink3)]")}>{extra(r)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
  return (
    <div className="space-y-1">
      {section("Scheduler gone", "crit", sh.gone, (r) =>
        r.last_seen_at ? `last seen ${timeAgo(r.last_seen_at)}` : ""
      )}
      {section(
        "Retention-less entries — outcome unknowable (§3.8)",
        "warn",
        sh.retention_less,
        () => "set asynq.Retention to make outcomes traceable"
      )}
      {section(
        "Entries targeting zero-consumer queues",
        "warn",
        sh.zero_consumer_targets,
        (r) => `${r.consumers} consumers on ${r.queue}`
      )}
      <div className="px-2.5 pb-1 text-[11px] text-[var(--fc-ink3)]">
        {fmt(sh.live_entries)} live entries · {fmt(sh.snapshots)} snapshots
      </div>
    </div>
  );
}

/**************************************************************
                       Report card
 **************************************************************/

function ReportCard({
  card,
  onChanged,
  onAction,
}: {
  card: HygieneCard;
  onChanged: () => void;
  onAction: (action: DeadLetterAction) => void;
}) {
  const [running, setRunning] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [report, setReport] = useState<HygieneReport | null>(null);
  const [error, setError] = useState("");
  const readOnly = window.READ_ONLY;

  const loadReport = useCallback(async () => {
    try {
      setReport(await getHygieneReport(card.kind));
      setError("");
    } catch (e) {
      setError(toErrorString(e as never));
    }
  }, [card.kind]);

  const runNow = async () => {
    if (running) return;
    setRunning(true);
    setError("");
    try {
      const rep = await runHygieneReport(card.kind);
      setReport(rep);
      setExpanded(true);
      onChanged();
    } catch (e) {
      setError(toErrorString(e as never));
    }
    setRunning(false);
  };

  const openReport = async () => {
    if (!expanded && !report) await loadReport();
    setExpanded(!expanded);
  };

  const chips = summaryChips(card.kind, card.summary);

  return (
    <section className={cn(panelClass, "flex flex-col gap-2 p-3")} aria-label={card.title}>
      <div>
        <h4 className="text-[13px] font-semibold text-[var(--fc-ink)]">{card.title}</h4>
        <div className="text-[11px] text-[var(--fc-ink3)]">
          {card.enabled ? formatInterval(card.interval_seconds) : "not scheduled"}
          {" · "}
          {card.last_generated_at ? (
            <span data-testid={`stamp-${card.kind}`}>generated {timeAgo(card.last_generated_at)}</span>
          ) : (
            "never generated"
          )}
        </div>
      </div>

      {chips.length === 0 ? (
        <div className="text-[11.5px] text-[var(--fc-ink3)]">
          No report yet — {KIND_QUESTIONS[card.kind]}
        </div>
      ) : (
        <div className="space-y-1">
          {chips.map((c) => (
            <div key={c.label} className="flex items-baseline justify-between gap-2 text-[11.5px]">
              <span className="text-[var(--fc-ink2)]">{c.label}</span>
              <b
                className={cn(
                  "font-mono tabular-nums",
                  c.tone === "crit" && "text-[var(--fc-crit)]",
                  c.tone === "warn" && c.value > 0 && "text-[var(--fc-warn)]"
                )}
              >
                {c.formatted ?? fmt(c.value)}
              </b>
            </div>
          ))}
        </div>
      )}

      <div className="flex items-center gap-2">
        <Button size="sm" variant="outline" className="h-7 px-2 text-[11.5px]" onClick={runNow} disabled={running}>
          {running ? (
            <Loader2 size={12} className="mr-1 animate-spin" data-testid={`spinner-${card.kind}`} />
          ) : (
            <Play size={12} className="mr-1" />
          )}
          Run now
        </Button>
        <Button size="sm" variant="ghost" className="h-7 px-2 text-[11.5px]" onClick={openReport}>
          {expanded ? "Close report" : "Open report"}
        </Button>
      </div>
      {error && <div className="text-[11px] text-[var(--fc-crit)]">{error}</div>}

      {!readOnly && <ScheduleEditor key={`${card.enabled}-${card.interval_seconds}`} card={card} onSaved={onChanged} />}

      {expanded && report && (
        <div className="border-t border-[var(--fc-line2)] pt-2">
          <div className="pb-2 text-[11px] text-[var(--fc-ink3)]">
            generated {timeAgo(report.generated_at)}
            {report.data_as_of && ` · data as of ${timeAgo(report.data_as_of)}`}
          </div>
          {card.kind === "inventory" && <InventoryTable report={report} />}
          {card.kind === "storage" && <StorageTable report={report} />}
          {card.kind === "dead-letter" && <DeadLetterTable report={report} onAction={onAction} />}
          {card.kind === "scheduler-health" && <SchedulerHealthTable report={report} />}
        </div>
      )}
    </section>
  );
}

/**************************************************************
                          View
 **************************************************************/

export default function HygieneView() {
  const [cards, setCards] = useState<HygieneCard[] | null>(null);
  const [loadError, setLoadError] = useState("");
  const [pendingAction, setPendingAction] = useState<DeadLetterAction | null>(null);
  const readOnly = window.READ_ONLY;

  const fetchCards = useCallback(async () => {
    try {
      const resp = await listHygiene();
      // Server order is the display order; keep a defensive stable sort.
      const byKind = new Map(resp.reports.map((r) => [r.kind, r]));
      setCards(HYGIENE_KINDS.map((k) => byKind.get(k)).filter((c): c is HygieneCard => !!c));
      setLoadError("");
    } catch (e) {
      setLoadError(toErrorString(e as never));
    }
  }, []);

  usePolling(fetchCards, 15);

  const enableWeeklyInventory = async () => {
    try {
      await putHygieneConfig("inventory", { enabled: true, interval_seconds: 7 * 24 * 3600 });
      fetchCards();
    } catch (e) {
      setLoadError(toErrorString(e as never));
    }
  };

  return (
    <PageShell>
      <div className="mb-3 text-[13px] text-[var(--fc-ink3)]">
        <b className="text-[var(--fc-ink)]">Hygiene</b> — what is rotting?
      </div>

      {loadError && (
        <div className={cn(panelClass, "mb-3 p-3 text-xs text-[var(--fc-crit)]")}>{loadError}</div>
      )}

      {cards && nothingScheduled(cards) && (
        <div className={cn(panelClass, "mb-3 flex items-center justify-between gap-3 p-4")}>
          <div className="text-[12.5px] text-[var(--fc-ink2)]">
            No reports scheduled — enable the weekly inventory to start.
          </div>
          {!readOnly && (
            <Button size="sm" variant="outline" className="h-7 text-[11.5px]" onClick={enableWeeklyInventory}>
              Enable weekly inventory
            </Button>
          )}
        </div>
      )}

      {!cards && !loadError && (
        <div className="flex h-40 items-center justify-center text-sm text-[var(--fc-ink3)]">
          Loading…
        </div>
      )}

      {cards && (
        <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
          {cards.map((card) => (
            <ReportCard
              key={card.kind}
              card={card}
              onChanged={fetchCards}
              onAction={setPendingAction}
            />
          ))}
        </div>
      )}

      {pendingAction && (
        <BulkJobModal
          open
          verb="delete"
          scope={deadLetterActionScope(pendingAction)}
          onClose={() => setPendingAction(null)}
        />
      )}
    </PageShell>
  );
}
