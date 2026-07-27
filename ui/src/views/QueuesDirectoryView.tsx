// Queues Directory (build contract §3.2): "which of my N-thousand queues is
// worst by any metric?" — one sort click. Dense, full-width table over
// GET /api/fleet/queues with server-side sort/filter/cursor-pagination; all
// state in the URL (parse/serializeDirectoryState).
//
// Rendering note: rows are plain-rendered. The page is hard-capped at
// limit ≤ 200 rows by the URL contract, which modern browsers lay out in
// single-digit milliseconds — @tanstack/react-virtual buys nothing until
// paging is replaced by an infinite scroller, so the dependency is
// deliberately not added in v1.
//
// Polling (not SSE) per §4.4: table pages poll on the visible cadence.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useSelector } from "react-redux";
import { useNavigate, useSearchParams } from "react-router-dom";
import { BookmarkPlus, X } from "lucide-react";
import { AppState } from "../store";
import { usePolling } from "../hooks";
import { useFrozenData } from "../hooks/useFrozenData";
import { listFleetQueues, FleetQueuesResponse, FleetQueueRow } from "../api-fleet";
import { SeriesResponse, SeriesSpec, getSeriesBatch } from "../api-series";
import {
  DirectoryState,
  DirectorySortKey,
  DIRECTORY_LIMITS,
  parseDirectoryState,
  serializeDirectoryState,
} from "../lib/urlstate";
import { ageTone, formatErrorRate, humanizeMs, isStale, formatLatencyMs } from "../lib/fleet";
import { queueDetailsPath } from "../paths";
import { timeAgo, toErrorString } from "../utils";
import { cn, clickableRowClass, clickableRowProps } from "../lib/utils";
import { FcChip } from "../components/FleetBits";
import PageShell from "../components/PageShell";
import { LazyMount, Sparkline } from "../components/charts";
import SaveViewModal from "../components/SaveViewModal";
import UpdatesPausedPill from "../components/UpdatesPausedPill";

const fmt = (n: number | undefined | null) =>
  n === undefined || n === null || !Number.isFinite(n) ? "—" : n.toLocaleString("en-US");

// asynq trims archived to 10k per queue — a full set means dead-letters are
// being discarded (honesty ledger §7.3).
const ARCHIVE_CAP = 10_000;

// Sparkline column (§3.2): 30m pending trend from the §5.8 hot ring, only
// for the top SPARK_ROW_CAP rows of the page, rendered only when the row is
// in the viewport (LazyMount), refreshed at the hot-slot cadence.
const SPARK_ROW_CAP = 50;
const SPARK_POINTS = 60; // 60 × 30s = 30m
const SPARK_REFRESH_MS = 30_000;

interface Col {
  label: string;
  sortKey?: DirectorySortKey;
  numeric?: boolean;
}

const COLUMNS: Col[] = [
  { label: "Queue", sortKey: "name" },
  { label: "Pending", sortKey: "pending", numeric: true },
  { label: "Trend 30m" }, // §5.8 sparkline; not server-sortable
  { label: "Active", sortKey: "active", numeric: true },
  { label: "Scheduled", sortKey: "scheduled", numeric: true },
  { label: "Retry", sortKey: "retry", numeric: true },
  { label: "Archived", sortKey: "archived", numeric: true },
  { label: "Oldest pend", sortKey: "oldest_pending_age", numeric: true },
  { label: "Latency", sortKey: "latency", numeric: true },
  { label: "Proc today", sortKey: "processed_today", numeric: true },
  { label: "Fail today", sortKey: "failed_today", numeric: true },
  { label: "Err rate", sortKey: "error_rate", numeric: true },
  { label: "Consumers", sortKey: "consumers", numeric: true },
  { label: "Paused since" }, // not server-sortable per contract
  { label: "" }, // staleness dot
];

function QueueRowView({
  row,
  now,
  spark,
  sparkEligible,
}: {
  row: FleetQueueRow;
  now: number;
  spark?: SeriesResponse;
  // False beyond the top-50 rows (§3.2 cap) — renders a dash, not a fetch.
  sparkEligible: boolean;
}) {
  const navigate = useNavigate();
  const uncovered = row.consumers === 0 && row.pending > 0;
  const oldTone = ageTone(row.oldest_pending_age_ms);
  const stale = isStale(row.refreshed_at, now);
  return (
    <tr
      {...clickableRowProps(() => navigate(queueDetailsPath(row.queue)))}
      className={cn(
        "border-b border-[var(--fc-line2)] last:border-b-0 hover:bg-[var(--fc-raise)]",
        clickableRowClass,
        uncovered && "shadow-[inset_2px_0_0_var(--fc-crit)]",
        !uncovered && row.paused && "shadow-[inset_2px_0_0_var(--fc-warn)]"
      )}
    >
      <td className="whitespace-nowrap px-2.5 py-1.5">
        <span className="font-mono text-xs font-semibold text-[var(--fc-acc)]">{row.queue}</span>
        {row.paused && (
          <FcChip tone="mut" className="ml-1.5">
            paused
          </FcChip>
        )}
        {uncovered && (
          <FcChip tone="crit" className="ml-1.5">
            no consumers
          </FcChip>
        )}
        {row.orphan_candidates > 0 && (
          <FcChip tone="warn" className="ml-1.5">
            orphans {fmt(row.orphan_candidates)}
          </FcChip>
        )}
        {row.archived >= ARCHIVE_CAP && (
          <FcChip tone="warn" className="ml-1.5" title="archived set at the 10k trim cap — oldest dead-letters are being discarded">
            archive cap
          </FcChip>
        )}
      </td>
      <td className="num-cell">{fmt(row.pending)}</td>
      <td className="px-2.5 py-1.5">
        {sparkEligible && spark ? (
          <LazyMount width={72} height={18}>
            <Sparkline
              points={spark.points}
              width={72}
              height={18}
              tone="mut"
              label={`${row.queue} pending — last 30m`}
            />
          </LazyMount>
        ) : (
          <span
            className="text-[10px] text-[var(--fc-ink3)]"
            title={
              sparkEligible
                ? "no series yet — the ring-buffer sampler hasn't published for this queue"
                : `sparklines render for the top ${SPARK_ROW_CAP} rows`
            }
          >
            —
          </span>
        )}
      </td>
      <td className="num-cell">{fmt(row.active)}</td>
      <td className="num-cell">
        {fmt(row.scheduled)}
        {row.past_due_scheduled > 0 && (
          <span className="ml-1 font-semibold text-[var(--fc-crit)]" title="scheduled tasks past due">
            (+{fmt(row.past_due_scheduled)} due)
          </span>
        )}
      </td>
      <td className="num-cell">{fmt(row.retry)}</td>
      <td className="num-cell">{fmt(row.archived)}</td>
      <td
        className={cn(
          "num-cell",
          oldTone === "crit" && "font-semibold text-[var(--fc-crit)]",
          oldTone === "warn" && "font-semibold text-[var(--fc-warn)]"
        )}
      >
        {humanizeMs(row.oldest_pending_age_ms)}
      </td>
      <td className="num-cell text-[var(--fc-ink3)]">{formatLatencyMs(row.latency_ms)}</td>
      <td className="num-cell text-[var(--fc-ink3)]">{fmt(row.processed_today)}</td>
      <td className="num-cell text-[var(--fc-ink3)]">{fmt(row.failed_today)}</td>
      <td
        className={cn(
          "num-cell",
          row.error_rate >= 0.25
            ? "font-semibold text-[var(--fc-crit)]"
            : "text-[var(--fc-ink3)]"
        )}
      >
        {formatErrorRate(row.error_rate)}
      </td>
      <td className="num-cell">
        {row.consumers === 0 ? (
          <b className="text-[var(--fc-crit)]">0</b>
        ) : (
          fmt(row.consumers)
        )}
      </td>
      <td className="whitespace-nowrap px-2.5 py-1.5 text-xs text-[var(--fc-ink3)]">
        {row.paused ? (row.paused_since ? timeAgo(row.paused_since) : "paused") : "—"}
      </td>
      <td className="px-2 py-1.5">
        {stale && (
          <span
            className="inline-block h-1.5 w-1.5 rounded-full bg-[var(--fc-ink3)] align-middle"
            title={`refreshed ${timeAgo(row.refreshed_at)} — cold tier`}
          />
        )}
      </td>
    </tr>
  );
}

export default function QueuesDirectoryView() {
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const [searchParams, setSearchParams] = useSearchParams();
  const view = useMemo(() => parseDirectoryState(searchParams), [searchParams]);

  const updateView = useCallback(
    (updates: Partial<DirectoryState>, opts?: { replace?: boolean }) => {
      setSearchParams(
        (prev) => serializeDirectoryState({ ...parseDirectoryState(prev), ...updates }, prev),
        { replace: opts?.replace ?? false }
      );
    },
    [setSearchParams]
  );

  const [resp, setResp] = useState<FleetQueuesResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  // The dismissed filter_ignored note (stores the dismissed text so a *new*
  // ignore reason shows again).
  const [dismissedNote, setDismissedNote] = useState<string | null>(null);

  // Filter box: local echo, applied on Enter (server owns the grammar).
  const [filterInput, setFilterInput] = useState(view.f);
  useEffect(() => setFilterInput(view.f), [view.f]);

  // §4.4 freeze-on-interaction: hovering the table suspends row updates
  // (data still fetched; meta/coverage keep updating). §4.2 Save view….
  const [hoveringTable, setHoveringTable] = useState(false);
  const [saveViewOpen, setSaveViewOpen] = useState(false);

  // Staleness dots track wall-clock time, refreshed every 5s.
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const fetchQueues = useCallback(async () => {
    try {
      const r = await listFleetQueues({
        sort: view.sort,
        dir: view.dir,
        f: view.f || undefined,
        cursor: view.cursor || undefined,
        limit: view.limit,
      });
      setResp(r);
      setError("");
      setNow(Date.now());
    } catch (e) {
      setError(toErrorString(e as Parameters<typeof toErrorString>[0]));
    } finally {
      setLoading(false);
    }
  }, [view.sort, view.dir, view.f, view.cursor, view.limit]);

  usePolling(fetchQueues, pollInterval, [view.sort, view.dir, view.f, view.cursor, view.limit]);

  // Sparkline data (§3.2): ONE batch call for the page's top rows, refreshed
  // at the hot-slot cadence. Data fetch is capped at SPARK_ROW_CAP and
  // rendering is additionally viewport-gated per cell (LazyMount).
  const [sparks, setSparks] = useState<Record<string, SeriesResponse>>({});
  const sparkNames = useMemo(
    () => (resp?.queues ?? []).slice(0, SPARK_ROW_CAP).map((r) => r.queue).join(" "),
    [resp]
  );
  useEffect(() => {
    if (sparkNames === "") return;
    let disposed = false;
    const fetchSparks = async () => {
      const specs: SeriesSpec[] = sparkNames.split(" ").map((q) => ({
        scope: `queue:${q}`,
        metric: "pending",
        window: "3h",
        points: SPARK_POINTS,
      }));
      try {
        const batch = await getSeriesBatch(specs);
        if (disposed) return;
        const next: Record<string, SeriesResponse> = {};
        for (const s of batch.series) next[s.scope.replace(/^queue:/, "")] = s;
        setSparks(next);
      } catch {
        // Endpoint absent: the column degrades to dashes, never fake lines.
      }
    };
    fetchSparks();
    const id = setInterval(fetchSparks, SPARK_REFRESH_MS);
    return () => {
      disposed = true;
      clearInterval(id);
    };
  }, [sparkNames]);

  const onSort = (key: DirectorySortKey) => {
    if (view.sort === key) {
      updateView({ dir: view.dir === "desc" ? "asc" : "desc", cursor: "" });
    } else {
      // Fresh sort: names read best ascending, every metric worst-first.
      updateView({ sort: key, dir: key === "name" ? "asc" : "desc", cursor: "" });
    }
  };

  const applyFilter = () => {
    if (filterInput !== view.f) updateView({ f: filterInput, cursor: "" });
  };

  // §4.4: rows render through the freeze buffer; the meta line (counts,
  // coverage) stays live. Mutations don't originate here, so no flush path.
  const liveRows = useMemo(() => resp?.queues ?? [], [resp]);
  // Equality ignores refreshed_at (the cache stamp isn't rendered as a cell
  // — an idle fleet must not count "updates" every sweep); everything else,
  // including seconds-granular ages, is a visible row change.
  const dirRowsEqual = useCallback((a: FleetQueueRow[], b: FleetQueueRow[]) => {
    if (a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) {
      const { refreshed_at: _x, ...x } = a[i];
      const { refreshed_at: _y, ...y } = b[i];
      if (JSON.stringify(x) !== JSON.stringify(y)) return false;
    }
    return true;
  }, []);
  const {
    data: rows,
    pending: pausedUpdates,
    apply: applyUpdates,
  } = useFrozenData(
    liveRows,
    hoveringTable,
    dirRowsEqual,
    // Sort/filter/page changes are deliberate — they flush through the
    // freeze (sort headers live INSIDE the hovered table).
    `${view.sort}|${view.dir}|${view.f}|${view.cursor}|${view.limit}`
  );
  const sortLabel = COLUMNS.find((c) => c.sortKey === view.sort)?.label ?? view.sort;

  // The urlstate-owned directory params, cursor stripped — what "Save
  // view…" persists (§4.2; served back verbatim).
  const directoryViewState = useMemo(() => {
    const params = serializeDirectoryState({ ...view, cursor: "" });
    return Object.fromEntries(params.entries());
  }, [view]);

  return (
    <PageShell>
      {/* Filter bar — f= passed to the server verbatim */}
      <div className="mb-2.5 flex items-center gap-2">
        <div className="flex flex-1 items-center gap-2 rounded-[7px] border border-[var(--fc-line)] bg-[var(--fc-panel)] px-3 py-2">
          <span className="text-[10.5px] uppercase tracking-[0.04em] text-[var(--fc-ink3)]">
            filter
          </span>
          <input
            value={filterInput}
            data-fc-querybar
            onChange={(e) => setFilterInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") applyFilter();
            }}
            placeholder="consumers=0 pending>10000 name~email"
            spellCheck={false}
            className="flex-1 border-0 bg-transparent font-mono text-[13px] text-[var(--fc-ink)] outline-none placeholder:text-[var(--fc-ink3)]"
          />
          <span className="text-[10.5px] text-[var(--fc-ink3)]">Enter to apply</span>
        </div>
        {!window.READ_ONLY && (
          <button
            onClick={() => setSaveViewOpen(true)}
            title="Save the current filter + sort as a server-side team view (launchable from ⌘K)"
            className="flex items-center gap-1.5 rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)] hover:border-[var(--fc-ink3)]"
          >
            <BookmarkPlus size={13} />
            Save view…
          </button>
        )}
        <select
          value={view.limit}
          onChange={(e) => updateView({ limit: Number(e.target.value), cursor: "" })}
          title="Rows per page"
          className="rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-2 py-2 text-xs font-semibold text-[var(--fc-ink)]"
        >
          {DIRECTORY_LIMITS.map((l) => (
            <option key={l} value={l}>
              {l} rows
            </option>
          ))}
        </select>
      </div>

      {/* Server couldn't apply (part of) the filter — say so, dismissably. */}
      {resp?.filter_ignored && dismissedNote !== resp.filter_ignored && (
        <div className="mb-2.5 flex items-center gap-2 rounded-md border border-[var(--fc-warn)]/45 bg-[var(--fc-warn-bg)] px-3 py-2 text-xs">
          <span className="font-semibold text-[var(--fc-warn)]">Filter partially ignored:</span>
          <span className="font-mono text-[var(--fc-ink2)]">{resp.filter_ignored}</span>
          <span className="flex-1" />
          <button
            onClick={() => setDismissedNote(resp.filter_ignored ?? null)}
            aria-label="Dismiss"
            className="text-[var(--fc-ink3)] hover:text-[var(--fc-ink)]"
          >
            <X size={13} />
          </button>
        </div>
      )}

      {/* Refresh failures atop stale-but-present data */}
      {error && resp && (
        <div className="mb-2.5 rounded-md border border-[var(--fc-warn)]/45 bg-[var(--fc-warn-bg)] px-3 py-2 text-xs text-[var(--fc-warn)]">
          Refresh failing ({error}) — showing the last loaded page.
        </div>
      )}

      {/* Meta row */}
      <div className="mb-2.5 flex flex-wrap items-center gap-3 text-xs text-[var(--fc-ink2)]">
        <span>
          <b className="font-mono tabular-nums text-[var(--fc-ink)]">
            {resp ? fmt(resp.total_matched) : "—"}
          </b>{" "}
          queues · sorted by <b className="text-[var(--fc-ink)]">{sortLabel}</b>{" "}
          {view.dir === "desc" ? "↓" : "↑"} · showing {rows.length}
          {view.cursor && " (continued)"}
        </span>
        <span className="flex-1" />
        {resp?.coverage && (
          <span className="text-[10.5px] text-[var(--fc-ink3)]">
            {resp.coverage.refreshed_pct_5m}% of queues &lt;5m fresh · tier {resp.coverage.tier}
          </span>
        )}
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full bg-[var(--fc-ink3)] align-middle" />
          = refreshed &gt;15s ago (cold tier)
        </span>
      </div>

      {/* Table — hover suspends row updates (§4.4) */}
      <div
        className="overflow-x-auto rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]"
        onMouseEnter={() => setHoveringTable(true)}
        onMouseLeave={() => setHoveringTable(false)}
      >
        {!resp && loading ? (
          <div className="px-4 py-10 text-center text-xs text-[var(--fc-ink3)]">
            Loading queues…
          </div>
        ) : !resp && error ? (
          <div className="px-4 py-10 text-center">
            <div className="text-sm font-semibold text-[var(--fc-ink)]">
              Queues directory unavailable
            </div>
            <p className="mx-auto mt-2 max-w-md text-xs text-[var(--fc-ink2)]">
              The fleet stats endpoints are not answering ({error}). The stats engine
              may not be deployed on this server yet.
            </p>
          </div>
        ) : rows.length === 0 ? (
          <div className="px-4 py-10 text-center text-xs text-[var(--fc-ink2)]">
            {view.f ? (
              <>
                No queues match{" "}
                <code className="rounded bg-[var(--fc-raise)] px-1.5 py-0.5 font-mono">
                  {view.f}
                </code>
                .{" "}
                <button
                  onClick={() => updateView({ f: "", cursor: "" })}
                  className="text-[var(--fc-acc)] hover:underline"
                >
                  Clear filter
                </button>
              </>
            ) : (
              "No queues found — nothing has enqueued yet."
            )}
          </div>
        ) : (
          <table className="w-full border-collapse text-[12.5px]">
            <thead>
              <tr>
                {COLUMNS.map((col) => (
                  <th
                    key={col.label || "stale"}
                    className={cn(
                      "sticky top-0 z-[2] whitespace-nowrap border-b border-[var(--fc-line)] bg-[var(--fc-panel)] px-2.5 py-[7px] text-[10.5px] font-semibold uppercase tracking-[0.07em] text-[var(--fc-ink3)]",
                      col.numeric ? "text-right" : "text-left",
                      col.sortKey && "cursor-pointer select-none hover:text-[var(--fc-ink)]"
                    )}
                    aria-sort={
                      col.sortKey === view.sort
                        ? view.dir === "desc"
                          ? "descending"
                          : "ascending"
                        : undefined
                    }
                    onClick={col.sortKey ? () => onSort(col.sortKey!) : undefined}
                  >
                    {col.label}
                    {col.sortKey === view.sort && (
                      <span className="ml-1 text-[var(--fc-acc)]">
                        {view.dir === "desc" ? "▼" : "▲"}
                      </span>
                    )}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="font-mono tabular-nums [&_td.num-cell]:whitespace-nowrap [&_td.num-cell]:px-2.5 [&_td.num-cell]:py-1.5 [&_td.num-cell]:text-right">
              {rows.map((row, i) => (
                <QueueRowView
                  key={row.queue}
                  row={row}
                  now={now}
                  spark={sparks[row.queue]}
                  sparkEligible={i < SPARK_ROW_CAP}
                />
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Cursor pagination */}
      <div className="mt-2.5 flex items-center gap-2">
        <button
          onClick={() => updateView({ cursor: "" })}
          disabled={!view.cursor}
          className="rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-3 py-1 text-xs font-semibold text-[var(--fc-ink)] disabled:cursor-not-allowed disabled:opacity-45"
        >
          ⇤ First page
        </button>
        <button
          onClick={() => resp?.next_cursor && updateView({ cursor: resp.next_cursor })}
          disabled={!resp?.next_cursor}
          className="rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-3 py-1 text-xs font-semibold text-[var(--fc-ink)] disabled:cursor-not-allowed disabled:opacity-45"
        >
          Next →
        </button>
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          cursor pagination — stable across polls; use Back for the previous page
        </span>
      </div>

      {/* §4.4 "N updates paused — refresh" pill. */}
      <UpdatesPausedPill count={pausedUpdates} onRefresh={applyUpdates} />

      {/* §4.2 "Save view…" — persists the urlstate-owned directory params. */}
      <SaveViewModal
        open={saveViewOpen}
        target="queues"
        state={directoryViewState}
        onClose={() => setSaveViewOpen(false)}
      />
    </PageShell>
  );
}
