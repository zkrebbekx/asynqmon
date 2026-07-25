// Workers screen (build contract §3.7): "is anyone actually listening?"
// Route stays /servers; nav label is Workers.
//
//   - Coverage matrix (top): queues × consuming servers join from
//     GET /api/coverage — UNCOVERED (crit) / STARVED (warn) rows, consumer
//     chips that jump to the server row, "who consumes Z" typeahead filter.
//   - Cancel-deliverability widget: GET /api/cancel-listeners with the
//     Redis Cluster approximation caveat spelled out (§5.14).
//   - Stuck-worker strip: all active workers fleet-wide by %-of-budget
//     descending, top 10 — computed client-side from the servers payload.
//   - Servers table: expandable rows; active workers link into the Tasks
//     console drawer (peek param) with elapsed-vs-deadline budget bars.
//
// Deliberately absent (honesty over decoration): a "last heartbeat" column —
// asynq's Inspector.Servers() does not expose heartbeat timestamps, so the
// mockup's Heartbeat column cannot be filled truthfully.

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSelector } from "react-redux";
import { ChevronDown, ChevronRight } from "lucide-react";
import { AppState, useAppDispatch } from "../store";
import { listServersAsync } from "../actions/serversActions";
import { usePolling } from "../hooks";
import { ServerInfo, WorkerInfo } from "../api";
import {
  getCoverage,
  getCancelListeners,
  CoverageResponse,
  CoverageRow,
  CancelListenersResponse,
} from "../api-fleet";
import { paths, queueDetailsPath } from "../paths";
import { timeAgo, toErrorString } from "../utils";
import { humanizeMs } from "../lib/fleet";
import {
  workerBudget,
  budgetTone,
  stuckWorkers,
  COVERAGE_TONE,
  coverageChipLabel,
  filterCoverageRows,
  activeTaskDrawerSearch,
} from "../lib/workers";
import { FcChip, MicroLabel } from "../components/FleetBits";
import { cn } from "../lib/utils";

const fmt = (n: number | undefined | null) =>
  n === undefined || n === null || !Number.isFinite(n) ? "—" : n.toLocaleString("en-US");

const shortId = (id: string) => (id.length > 10 ? `${id.slice(0, 8)}…` : id);

/**************************************************************
                        Shared bits
 **************************************************************/

const panelClass = "rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]";
const theadClass =
  "whitespace-nowrap border-b border-[var(--fc-line)] px-2.5 py-[7px] text-left text-[10.5px] font-semibold uppercase tracking-[0.07em] text-[var(--fc-ink3)]";

// BudgetBar renders %-of-budget as width + tone; callers pair it with text
// (severity is never color alone).
function BudgetBar({ pct }: { pct: number | null }) {
  const tone = budgetTone(pct);
  return (
    <span className="inline-block h-[6px] w-[72px] overflow-hidden rounded-[3px] bg-[var(--fc-raise)] align-middle">
      {pct !== null && (
        <i
          className={cn(
            "block h-full",
            tone === "crit"
              ? "bg-[var(--fc-crit)]"
              : tone === "warn"
                ? "bg-[var(--fc-warn)]"
                : "bg-[var(--fc-acc)]"
          )}
          style={{ width: `${Math.min(100, Math.max(2, pct * 100))}%` }}
        />
      )}
    </span>
  );
}

/**************************************************************
                      Coverage matrix
 **************************************************************/

function coverageNote(row: CoverageRow): string {
  if (row.status === "uncovered") {
    return row.pending !== undefined
      ? `${fmt(row.pending)} pending will not run`
      : "no live consumer";
  }
  if (row.status === "starved") {
    return "strict priority; higher-priority queues non-empty";
  }
  return "";
}

function CoverageMatrix({
  coverage,
  error,
  filter,
  onFilterChange,
  onJumpToServer,
  nowMs,
}: {
  coverage: CoverageResponse | null;
  error: string;
  filter: string;
  onFilterChange: (v: string) => void;
  onJumpToServer: (serverId: string) => void;
  nowMs: number;
}) {
  const rows = filterCoverageRows(coverage?.rows ?? [], filter);
  return (
    <div className={panelClass}>
      <div className="flex flex-wrap items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2">
        <span className="text-xs font-semibold text-[var(--fc-ink)]">
          Coverage — queues × consuming servers
        </span>
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          O(servers+configs) · exact
          {coverage &&
            ` · updated ${Math.max(0, Math.round((nowMs - Date.parse(coverage.updated_at)) / 1000))}s ago`}
        </span>
        <span className="flex-1" />
        <input
          value={filter}
          onChange={(e) => onFilterChange(e.target.value)}
          placeholder="who consumes… (queue name)"
          spellCheck={false}
          className="w-56 rounded-md border border-[var(--fc-line)] bg-transparent px-2 py-1 font-mono text-xs text-[var(--fc-ink)] outline-none placeholder:text-[var(--fc-ink3)] focus:border-[var(--fc-acc)]"
        />
      </div>

      {!coverage && error ? (
        <div className="px-3 py-6 text-center text-xs text-[var(--fc-ink2)]">
          Coverage unavailable ({error}).
        </div>
      ) : !coverage ? (
        <div className="px-3 py-6 text-center text-xs text-[var(--fc-ink3)]">
          Loading coverage…
        </div>
      ) : rows.length === 0 ? (
        <div className="px-3 py-6 text-center text-xs text-[var(--fc-ink2)]">
          {filter ? (
            <>
              No queue matches <code className="rounded bg-[var(--fc-raise)] px-1.5 py-0.5 font-mono">{filter}</code>.{" "}
              <button onClick={() => onFilterChange("")} className="text-[var(--fc-acc)] hover:underline">
                Clear
              </button>
            </>
          ) : (
            "No queues known yet — nothing has enqueued and no server declares a queue."
          )}
        </div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-[12.5px]">
            <thead>
              <tr>
                <th className={theadClass}>Queue</th>
                <th className={cn(theadClass, "text-right")}>Pending</th>
                <th className={theadClass}>Consuming servers</th>
                <th className={cn(theadClass, "text-right")} title="Upper bound: each server's slots are shared across its whole queue map">
                  Slots ≤
                </th>
                <th className={theadClass}>Status</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr
                  key={row.queue}
                  className={cn(
                    "border-b border-[var(--fc-line2)] last:border-b-0 hover:bg-[var(--fc-raise)]",
                    row.status === "uncovered" && "shadow-[inset_2px_0_0_var(--fc-crit)]",
                    row.status === "starved" && "shadow-[inset_2px_0_0_var(--fc-warn)]"
                  )}
                >
                  <td className="whitespace-nowrap px-2.5 py-1.5">
                    <Link
                      to={queueDetailsPath(row.queue)}
                      className="font-mono text-xs font-semibold text-[var(--fc-acc)] hover:underline"
                    >
                      {row.queue}
                    </Link>
                  </td>
                  <td
                    className="whitespace-nowrap px-2.5 py-1.5 text-right font-mono tabular-nums"
                    title={row.pending === undefined ? "stats cache unavailable — depth unknown" : undefined}
                  >
                    {row.pending === undefined ? "—" : fmt(row.pending)}
                  </td>
                  <td className="px-2.5 py-1.5">
                    {row.consumers.length === 0 ? (
                      <span className="text-xs text-[var(--fc-ink3)]">— none —</span>
                    ) : (
                      <span className="flex flex-wrap gap-1">
                        {row.consumers.map((c) => (
                          <button
                            key={c.server_id}
                            onClick={() => onJumpToServer(c.server_id)}
                            title={`jump to ${c.host}:${c.pid}${c.strict_priority ? " (strict priority)" : ""}`}
                            className="rounded bg-[var(--fc-raise)] px-[7px] py-[2px] font-mono text-[10.5px] font-semibold text-[var(--fc-ink2)] hover:bg-[var(--fc-acc-bg)] hover:text-[var(--fc-acc)]"
                          >
                            {c.host}:{c.pid} ×{c.weight}
                            {c.strict_priority && " strict"}
                          </button>
                        ))}
                      </span>
                    )}
                  </td>
                  <td className="whitespace-nowrap px-2.5 py-1.5 text-right font-mono tabular-nums">
                    {row.consumers.length === 0 ? "—" : fmt(row.total_concurrency)}
                  </td>
                  <td className="whitespace-nowrap px-2.5 py-1.5">
                    <FcChip tone={COVERAGE_TONE[row.status]}>{coverageChipLabel(row.status)}</FcChip>
                    {coverageNote(row) && (
                      <span className="ml-1.5 text-[10.5px] text-[var(--fc-ink3)]">
                        {coverageNote(row)}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/**************************************************************
              Cancel deliverability + stuck workers
 **************************************************************/

function CancelPanel({
  cancel,
  error,
  hasServers,
}: {
  cancel: CancelListenersResponse | null;
  error: string;
  hasServers: boolean;
}) {
  return (
    <div className={panelClass}>
      <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
        Cancel deliverability
      </div>
      <div className="px-3 py-2.5">
        {!cancel && error ? (
          <div className="text-xs text-[var(--fc-ink2)]">Unavailable ({error}).</div>
        ) : !cancel ? (
          <div className="text-xs text-[var(--fc-ink3)]">Loading…</div>
        ) : (
          <>
            <div className="font-mono text-[19px] font-semibold tabular-nums text-[var(--fc-ink)]">
              {fmt(cancel.listeners)}{" "}
              <span className="text-xs font-normal text-[var(--fc-ink2)]">
                listeners on asynq:cancel
              </span>
            </div>
            {cancel.listeners === 0 && hasServers && (
              <FcChip tone="crit" className="mt-1.5">
                no listeners — cancel signals go nowhere
              </FcChip>
            )}
            <div className="mt-1 text-[10.5px] text-[var(--fc-ink3)]">
              {cancel.approximate
                ? `summed across ${cancel.nodes} reachable node${cancel.nodes === 1 ? "" : "s"}; per-node counts on Redis Cluster — treat as approximate`
                : "exact · single Redis node · cancel is cooperative pub/sub with no delivery guarantee"}
            </div>
          </>
        )}
      </div>
    </div>
  );
}

function StuckPanel({
  servers,
  nowMs,
  appTasksPath,
}: {
  servers: ServerInfo[];
  nowMs: number;
  appTasksPath: string;
}) {
  const stuck = stuckWorkers(servers, nowMs);
  return (
    <div className={panelClass}>
      <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
        Stuck workers <span className="font-normal text-[var(--fc-ink3)]">· % of deadline · top 10</span>
      </div>
      {stuck.length === 0 ? (
        <div className="px-3 py-4 text-xs text-[var(--fc-ink3)]">
          No active workers with deadlines.
        </div>
      ) : (
        <div className="flex flex-col gap-1.5 px-3 py-2">
          {stuck.map((s) => (
            <div key={`${s.serverId}:${s.worker.task_id}`} className="flex items-center gap-2 text-xs">
              <span className="min-w-0 flex-1 truncate font-mono text-[var(--fc-ink2)]" title={`${s.host}:${s.pid}`}>
                {s.host}:{s.pid}
              </span>
              <Link
                to={`${appTasksPath}${activeTaskDrawerSearch(s.worker.queue, s.worker.task_id)}`}
                className="font-mono text-[var(--fc-acc)] hover:underline"
                title={`open ${s.worker.task_id} in the task drawer`}
              >
                {shortId(s.worker.task_id)}
              </Link>
              <BudgetBar pct={s.pct} />
              <span
                className={cn(
                  "w-10 text-right font-mono tabular-nums",
                  budgetTone(s.pct) === "crit" && "font-semibold text-[var(--fc-crit)]",
                  budgetTone(s.pct) === "warn" && "font-semibold text-[var(--fc-warn)]"
                )}
              >
                {Math.round(s.pct * 100)}%
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/**************************************************************
                        Servers table
 **************************************************************/

function WorkerRow({ w, nowMs, appTasksPath }: { w: WorkerInfo; nowMs: number; appTasksPath: string }) {
  const b = workerBudget(w.start_time, w.deadline, nowMs);
  const tone = budgetTone(b.pct);
  return (
    <div className="flex items-center gap-2.5 py-1 text-xs">
      <Link
        to={`${appTasksPath}${activeTaskDrawerSearch(w.queue, w.task_id)}`}
        className="font-mono text-[var(--fc-acc)] hover:underline"
        title={`open ${w.task_id} in the task drawer`}
      >
        {shortId(w.task_id)}
      </Link>
      <span className="font-mono text-[var(--fc-ink2)]">{w.task_type}</span>
      <Link to={queueDetailsPath(w.queue)} className="font-mono text-[var(--fc-ink3)] hover:underline">
        {w.queue}
      </Link>
      <span className="flex-1" />
      <BudgetBar pct={b.pct} />
      <span className="w-28 whitespace-nowrap text-right font-mono tabular-nums text-[var(--fc-ink2)]">
        {b.elapsedMs === null ? "—" : b.elapsedMs > 0 ? humanizeMs(b.elapsedMs) : "0s"}
        {b.budgetMs !== null ? ` / ${humanizeMs(b.budgetMs)}` : " · no deadline"}
      </span>
      {tone === "crit" && b.pct !== null && (
        <FcChip tone="crit">{Math.round(b.pct * 100)}% of deadline</FcChip>
      )}
    </div>
  );
}

function serverStatusTone(status: string): "good" | "warn" | "mut" {
  if (status === "active") return "good";
  if (status === "closed" || status === "stopped") return "mut";
  return "warn";
}

function ServerRow({
  server,
  expanded,
  onToggle,
  flash,
  nowMs,
  appTasksPath,
}: {
  server: ServerInfo;
  expanded: boolean;
  onToggle: () => void;
  flash: boolean;
  nowMs: number;
  appTasksPath: string;
}) {
  const busy = server.active_workers.length;
  const busyPct = server.concurrency > 0 ? busy / server.concurrency : 0;
  const weights = Object.values(server.queue_priorities);
  const minW = Math.min(...weights);
  const maxW = Math.max(...weights);
  const queueChips = Object.entries(server.queue_priorities).sort((a, b) => b[1] - a[1]);
  return (
    <>
      <tr
        id={`srv-${server.id}`}
        onClick={onToggle}
        className={cn(
          "cursor-pointer border-b border-[var(--fc-line2)] last:border-b-0 hover:bg-[var(--fc-raise)]",
          flash && "bg-[var(--fc-acc-bg)]"
        )}
      >
        <td className="w-8 px-2.5 py-1.5 text-[var(--fc-ink3)]">
          {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </td>
        <td className="whitespace-nowrap px-2.5 py-1.5 font-mono text-xs font-semibold text-[var(--fc-ink)]">
          {server.host}:{server.pid}
        </td>
        <td className="whitespace-nowrap px-2.5 py-1.5">
          <FcChip tone={serverStatusTone(server.status)}>{server.status}</FcChip>
        </td>
        <td className="whitespace-nowrap px-2.5 py-1.5 text-right font-mono tabular-nums">
          {busy}/{server.concurrency}
        </td>
        <td className="whitespace-nowrap px-2.5 py-1.5">
          <span className="inline-block h-[6px] w-[88px] overflow-hidden rounded-[3px] bg-[var(--fc-raise)] align-middle">
            <i
              className={cn(
                "block h-full",
                busyPct >= 1 ? "bg-[var(--fc-warn)]" : "bg-[var(--fc-acc)]"
              )}
              style={{ width: `${Math.min(100, Math.round(busyPct * 100))}%` }}
            />
          </span>
          <span className="ml-1.5 align-middle font-mono text-[10.5px] tabular-nums text-[var(--fc-ink3)]">
            {Math.round(busyPct * 100)}%
          </span>
        </td>
        <td className="px-2.5 py-1.5">
          <span className="flex flex-wrap gap-1">
            {queueChips.map(([q, w]) => {
              const starvable =
                server.strict_priority_enabled && w === minW && maxW > minW;
              return (
                <Link
                  key={q}
                  to={queueDetailsPath(q)}
                  onClick={(e) => e.stopPropagation()}
                  title={
                    starvable
                      ? `${q} sits at this strict-priority server's lowest weight — starvable when higher classes are busy`
                      : `open queue ${q}`
                  }
                  className={cn(
                    "rounded px-[7px] py-[2px] font-mono text-[10.5px] font-semibold no-underline",
                    starvable
                      ? "bg-[var(--fc-warn-bg)] text-[var(--fc-warn)]"
                      : "bg-[var(--fc-raise)] text-[var(--fc-ink2)] hover:text-[var(--fc-acc)]"
                  )}
                >
                  {q}·{w}
                  {starvable && " strict"}
                </Link>
              );
            })}
            {server.strict_priority_enabled && (
              <FcChip tone="mut" title="queues drain strictly by weight on this server">
                strict
              </FcChip>
            )}
          </span>
        </td>
        <td className="whitespace-nowrap px-2.5 py-1.5 text-right text-xs text-[var(--fc-ink3)]">
          {timeAgo(server.start_time)}
        </td>
      </tr>
      {expanded && (
        <tr className="border-b border-[var(--fc-line2)] last:border-b-0">
          <td colSpan={7} className="bg-[var(--fc-bg)] px-4 py-2">
            {server.active_workers.length === 0 ? (
              <div className="py-1 text-xs text-[var(--fc-ink3)]">
                No active workers — all {server.concurrency} slots idle.
              </div>
            ) : (
              <>
                <MicroLabel className="mb-1">
                  active workers · {server.active_workers.length}
                </MicroLabel>
                {server.active_workers.map((w) => (
                  <WorkerRow
                    key={w.task_id}
                    w={w}
                    nowMs={nowMs}
                    appTasksPath={appTasksPath}
                  />
                ))}
              </>
            )}
          </td>
        </tr>
      )}
    </>
  );
}

/**************************************************************
                        Workers screen
 **************************************************************/

export default function ServersView() {
  const dispatch = useAppDispatch();
  const {
    loading: serversLoading,
    error: serversError,
    data: servers,
  } = useSelector((s: AppState) => s.servers);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const appPaths = paths();

  usePolling(() => dispatch(listServersAsync()), pollInterval);

  // Coverage + cancel widgets poll together on the same cadence.
  const [coverage, setCoverage] = useState<CoverageResponse | null>(null);
  const [coverageError, setCoverageError] = useState("");
  const [cancel, setCancel] = useState<CancelListenersResponse | null>(null);
  const [cancelError, setCancelError] = useState("");
  const fetchWidgets = useCallback(async () => {
    try {
      setCoverage(await getCoverage());
      setCoverageError("");
    } catch (e) {
      setCoverageError(toErrorString(e as Parameters<typeof toErrorString>[0]));
    }
    try {
      setCancel(await getCancelListeners());
      setCancelError("");
    } catch (e) {
      setCancelError(toErrorString(e as Parameters<typeof toErrorString>[0]));
    }
  }, []);
  usePolling(fetchWidgets, pollInterval);

  // Budget bars and "updated Ns ago" track wall-clock time. Plain interval,
  // not usePolling — this is a clock, not a fetch, and must not feed the
  // header's poll-tick indicator.
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  const [filter, setFilter] = useState("");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [flash, setFlash] = useState<string | null>(null);

  // Consumer-chip click: expand + scroll to the server's row, with a brief
  // highlight so the eye lands on it.
  const jumpToServer = useCallback((serverId: string) => {
    setExpanded((prev) => ({ ...prev, [serverId]: true }));
    setFlash(serverId);
    window.setTimeout(() => setFlash((f) => (f === serverId ? null : f)), 1600);
    // After the expand renders:
    window.setTimeout(() => {
      document
        .getElementById(`srv-${serverId}`)
        ?.scrollIntoView({ behavior: "smooth", block: "center" });
    }, 0);
  }, []);

  const totalSlots = servers.reduce((acc, s) => acc + s.concurrency, 0);
  const totalBusy = servers.reduce((acc, s) => acc + s.active_workers.length, 0);
  const noServers = !serversLoading && !serversError && servers.length === 0;

  return (
    <div className="px-4 py-4">
      {/* §3.7 empty state: this IS the answer, not an error. */}
      {noServers && (
        <div className="mb-2.5 rounded-lg border border-[var(--fc-crit)]/50 bg-[var(--fc-crit-bg)] px-4 py-4">
          <div className="text-sm font-semibold text-[var(--fc-crit)]">
            No live servers. All queues are unconsumed. Pending work will not run.
          </div>
          <div className="mt-1 text-xs text-[var(--fc-ink2)]">
            Start an asynq server (asynq.NewServer + Run) pointed at this Redis —
            the coverage matrix below shows what is waiting.
          </div>
        </div>
      )}

      {serversError && (
        <div className="mb-2.5 rounded-md border border-[var(--fc-warn)]/45 bg-[var(--fc-warn-bg)] px-3 py-2 text-xs text-[var(--fc-warn)]">
          Could not retrieve live server data ({serversError}) — showing the last
          loaded state.
        </div>
      )}

      {/* Coverage matrix + right rail */}
      <div className="grid grid-cols-1 items-start gap-2.5 xl:grid-cols-[1fr_300px]">
        <CoverageMatrix
          coverage={coverage}
          error={coverageError}
          filter={filter}
          onFilterChange={setFilter}
          onJumpToServer={jumpToServer}
          nowMs={now}
        />
        <div className="flex flex-col gap-2.5">
          <CancelPanel cancel={cancel} error={cancelError} hasServers={servers.length > 0} />
          {servers.length > 0 && (
            <StuckPanel servers={servers} nowMs={now} appTasksPath={appPaths.TASKS} />
          )}
        </div>
      </div>

      {/* Servers table */}
      {servers.length > 0 && (
        <div className={cn(panelClass, "mt-2.5")}>
          <div className="flex items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
            Servers
            <span className="font-normal text-[var(--fc-ink3)]">
              {fmt(servers.length)} live · {fmt(totalSlots)} slots ·{" "}
              {totalSlots > 0 ? Math.round((totalBusy / totalSlots) * 100) : 0}% busy
            </span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-[12.5px]">
              <thead>
                <tr>
                  <th className={theadClass} />
                  <th className={theadClass}>Server</th>
                  <th className={theadClass}>Status</th>
                  <th className={cn(theadClass, "text-right")}>Workers</th>
                  <th className={theadClass}>Busy</th>
                  <th className={theadClass}>Queues · weight</th>
                  <th className={cn(theadClass, "text-right")}>Started</th>
                </tr>
              </thead>
              <tbody>
                {servers.map((s) => (
                  <ServerRow
                    key={s.id}
                    server={s}
                    expanded={!!expanded[s.id]}
                    onToggle={() => setExpanded((p) => ({ ...p, [s.id]: !p[s.id] }))}
                    flash={flash === s.id}
                    nowMs={now}
                    appTasksPath={appPaths.TASKS}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
