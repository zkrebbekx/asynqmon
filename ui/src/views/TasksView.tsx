// Queue Workspace (`/queues/:qname`) — build contract §3.3. "Why is this
// queue unhealthy, who consumes it, what do I do?" without leaving the page:
// header health strip, an Attention default tab (never `active`) synthesized
// from cheap reads, all seven state tabs with always-visible counts, an
// error-clusters/consumers/histograms/memory right rail, and whole-scope
// verbs that enter the §4.3 bulk-job flow pre-scoped to this queue.
//
// Deep links: ?focus=<state> (written by attentionTarget for Fleet findings)
// lands on the Attention tab pre-focused on the anomaly; ?status=<state>
// opens a state tab directly (legacy links keep working). The topbar
// breadcrumb owns Queues › <name> — no in-page breadcrumb duplication.

import { useCallback, useEffect, useState, KeyboardEvent } from "react";
import { useSelector } from "react-redux";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { Search, AlertTriangle } from "lucide-react";
import { AppState } from "../store";
import * as api from "../api";
import { DailyStat, JobVerb, TaskInfo } from "../api";
import {
  CoverageRow,
  FleetQueueRow,
  PendingWaitSampleResponse,
  RetryHistogramResponse,
  getCoverage,
  getPendingWaitSample,
  getRetryHistogram,
  listFleetQueues,
} from "../api-fleet";
import { usePolling, useQuery } from "../hooks";
import { paths, taskDetailsPath } from "../paths";
import { quoteAqlValue } from "../lib/aql";
import {
  FocusState,
  WorkspaceTab,
  WORKSPACE_TABS,
  healthySummary,
  mergeErrorClusters,
  orphanedActives,
  parseWorkspaceParams,
  pastDueScheduled,
  serializeWorkspaceTab,
  synthesizeAttention,
  upcomingRetries,
} from "../lib/workspace";
import WorkspaceHealthStrip from "../components/workspace/WorkspaceHealthStrip";
import WorkspaceAttentionTab from "../components/workspace/WorkspaceAttentionTab";
import WorkspaceRail from "../components/workspace/WorkspaceRail";
import BulkJobModal from "../components/BulkJobModal";
import ActiveTasksTable from "../components/ActiveTasksTable";
import PendingTasksTable from "../components/PendingTasksTable";
import ScheduledTasksTable from "../components/ScheduledTasksTable";
import RetryTasksTable from "../components/RetryTasksTable";
import ArchivedTasksTable from "../components/ArchivedTasksTable";
import CompletedTasksTable from "../components/CompletedTasksTable";
import AggregatingTasksTableContainer from "../components/AggregatingTasksTableContainer";
import { Input } from "../components/ui/input";
import { cn } from "../lib/utils";

const TAB_LABELS: Record<WorkspaceTab, string> = {
  attention: "⚑ Attention",
  pending: "pending",
  active: "active",
  aggregating: "aggregating",
  scheduled: "scheduled",
  retry: "retry",
  archived: "archived",
  completed: "completed",
};

interface BulkTarget {
  verb: JobVerb;
  state: string;
  aql?: string;
}

export default function TasksView() {
  const { qname } = useParams<{ qname: string }>();
  const queue = qname!;
  const navigate = useNavigate();
  const location = useLocation();
  const query = useQuery();
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  const { tab, focus } = parseWorkspaceParams(query);

  // ---------- polled data ----------
  const [counts, setCounts] = useState<Record<string, number> | null>(null);
  const [fleetRow, setFleetRow] = useState<FleetQueueRow | null>(null);
  const [fleetUnavailable, setFleetUnavailable] = useState(false);
  const [fleetChecked, setFleetChecked] = useState(false);
  const [coverageRow, setCoverageRow] = useState<CoverageRow | null>(null);
  const [retryHist, setRetryHist] = useState<RetryHistogramResponse | null>(null);
  const [waitSample, setWaitSample] = useState<PendingWaitSampleResponse | null>(null);
  const [retryClusters, setRetryClusters] = useState<api.TaskAggregateResponse | null>(null);
  const [archivedClusters, setArchivedClusters] = useState<api.TaskAggregateResponse | null>(null);
  const [clustersFailed, setClustersFailed] = useState(false);

  const fetchCore = useCallback(async () => {
    const [c, fq, cov, rh, pw, aggR, aggA] = await Promise.allSettled([
      api.taskStateCounts(queue),
      listFleetQueues({ f: `name=${queue}` }),
      getCoverage(),
      getRetryHistogram(queue),
      getPendingWaitSample(queue),
      api.taskAggregate({ queue, state: "retry", by: "error", maxScan: 2000, limit: 8 }),
      api.taskAggregate({ queue, state: "archived", by: "error", maxScan: 2000, limit: 8 }),
    ]);
    if (c.status === "fulfilled") setCounts(c.value.counts);
    if (fq.status === "fulfilled") {
      setFleetRow(fq.value.queues.find((r) => r.queue === queue) ?? null);
      setFleetUnavailable(false);
    } else {
      setFleetRow(null);
      setFleetUnavailable(true);
    }
    setFleetChecked(true);
    setCoverageRow(
      cov.status === "fulfilled"
        ? (cov.value.rows.find((r) => r.queue === queue) ?? null)
        : null
    );
    setRetryHist(rh.status === "fulfilled" ? rh.value : null);
    setWaitSample(pw.status === "fulfilled" ? pw.value : null);
    setRetryClusters(aggR.status === "fulfilled" ? aggR.value : null);
    setArchivedClusters(aggA.status === "fulfilled" ? aggA.value : null);
    setClustersFailed(aggR.status === "rejected" && aggA.status === "rejected");
  }, [queue]);
  usePolling(fetchCore, pollInterval, [queue]);

  // ---------- attention-tab data (only fetched while it is the tab) ----------
  const [retryTasks, setRetryTasks] = useState<TaskInfo[]>([]);
  const [activeTasks, setActiveTasks] = useState<TaskInfo[]>([]);
  const [scheduledTasks, setScheduledTasks] = useState<TaskInfo[]>([]);
  const [attentionLoaded, setAttentionLoaded] = useState(false);

  const fetchAttention = useCallback(async () => {
    if (tab !== "attention") return;
    const [r, a, s] = await Promise.allSettled([
      api.listRetryTasks(queue, { size: 20, page: 1 }),
      api.listActiveTasks(queue, { size: 100, page: 1 }),
      api.listScheduledTasks(queue, { size: 20, page: 1 }),
    ]);
    if (r.status === "fulfilled") setRetryTasks(r.value.tasks ?? []);
    if (a.status === "fulfilled") setActiveTasks(a.value.tasks ?? []);
    if (s.status === "fulfilled") setScheduledTasks(s.value.tasks ?? []);
    setAttentionLoaded(true);
  }, [queue, tab]);
  usePolling(fetchAttention, pollInterval, [queue, tab]);

  // ---------- one-shot queue snapshot: history + memory (sample-on-demand) ----------
  const [history, setHistory] = useState<DailyStat[]>([]);
  const [memoryBytes, setMemoryBytes] = useState<number | null>(null);
  const [memorySampledAt, setMemorySampledAt] = useState<Date | null>(null);
  const [memSampling, setMemSampling] = useState(false);

  const sampleQueue = useCallback(
    async (manual: boolean) => {
      if (manual) setMemSampling(true);
      try {
        const resp = await api.getQueue(queue);
        setHistory(resp.history ?? []);
        setMemoryBytes(resp.current.memory_usage_bytes);
        setMemorySampledAt(new Date());
      } catch {
        // queue may not exist; the not-found banner owns that story.
      }
      if (manual) setMemSampling(false);
    },
    [queue]
  );
  useEffect(() => {
    // One sample on arrival (GetQueueInfo runs MEMORY USAGE server-side, so
    // this is deliberately NOT on the poll loop — §3.3(d) sample-on-demand).
    setHistory([]);
    setMemoryBytes(null);
    setMemorySampledAt(null);
    sampleQueue(false);
  }, [sampleQueue]);

  // ---------- 1s ticker for live countdowns / ages ----------
  const [nowMs, setNowMs] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  // ---------- derived (pure fns, lib/workspace.ts) ----------
  const items = synthesizeAttention({
    counts,
    fleetRow,
    activeTasks,
    scheduledTasks,
    retryHistogram: retryHist,
    history,
    nowMs,
  });
  const retryRows = upcomingRetries(retryTasks, nowMs);
  const orphans = orphanedActives(activeTasks);
  const pastDue = pastDueScheduled(scheduledTasks, nowMs);
  const clusters = mergeErrorClusters(
    retryClusters?.groups ?? [],
    archivedClusters?.groups ?? []
  );
  const clustersTruncated =
    (retryClusters?.truncated ?? false) || (archivedClusters?.truncated ?? false);
  const clustersScanned =
    (retryClusters?.scanned ?? 0) + (archivedClusters?.scanned ?? 0);

  const notFound =
    fleetChecked &&
    !fleetUnavailable &&
    fleetRow === null &&
    counts !== null &&
    Object.values(counts).every((n) => n <= 0);

  // ---------- navigation ----------
  const openTab = (next: WorkspaceTab) => {
    const params = serializeWorkspaceTab(next, new URLSearchParams(location.search));
    const s = params.toString();
    navigate(`${location.pathname}${s ? `?${s}` : ""}`);
  };

  const [searchId, setSearchId] = useState("");
  const handleSearchKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" && searchId.trim()) {
      navigate(taskDetailsPath(queue, searchId.trim()));
    }
  };

  // ---------- bulk-job modal (§4.3, pre-scoped to this queue) ----------
  const [bulk, setBulk] = useState<BulkTarget | null>(null);
  const onClusterVerb = (verb: JobVerb, state: "retry" | "archived", signature: string) =>
    setBulk({ verb, state, aql: `error~${quoteAqlValue(signature)}` });

  const countLabel = (t: WorkspaceTab): string => {
    if (t === "attention") return "";
    const n = counts?.[t];
    if (n === undefined) return "";
    if (n < 0) return "—"; // unknowable, never a guess
    return n.toLocaleString("en-US");
  };

  return (
    <div className="space-y-3 px-4 py-4">
      {/* §3.3 error state: queue not in the fleet cache and empty everywhere. */}
      {notFound && (
        <div className="flex items-start gap-2 rounded-lg border border-[var(--fc-warn)]/40 bg-[var(--fc-warn-bg)] px-3 py-2.5 text-xs text-[var(--fc-ink2)]">
          <AlertTriangle size={14} className="mt-0.5 shrink-0 text-[var(--fc-warn)]" />
          <span>
            Queue <b className="font-mono">{queue}</b> is not in the fleet cache — it may
            have been removed.{" "}
            <button
              onClick={() => navigate(paths().QUEUES)}
              className="text-[var(--fc-acc)] hover:underline"
            >
              Check the Queues directory →
            </button>
          </span>
        </div>
      )}

      {/* A. Header health strip */}
      <WorkspaceHealthStrip
        qname={queue}
        fleetRow={fleetRow}
        fleetUnavailable={fleetUnavailable}
        counts={counts}
        coverageRow={coverageRow}
        nowMs={nowMs}
      />

      <div className="grid grid-cols-1 gap-2.5 xl:grid-cols-[minmax(0,1fr)_320px]">
        {/* B/C. Tabs: Attention default + all seven states, counts always visible */}
        <div className="min-w-0 rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
          <div className="flex flex-wrap items-center gap-1 border-b border-[var(--fc-line)] px-2 py-1.5">
            {WORKSPACE_TABS.map((t) => (
              <button
                key={t}
                onClick={() => openTab(t)}
                className={cn(
                  "rounded-md px-2.5 py-1 text-xs transition-colors",
                  tab === t
                    ? "bg-[var(--fc-acc-bg)] font-semibold text-[var(--fc-acc)]"
                    : "text-[var(--fc-ink2)] hover:bg-[var(--fc-raise)] hover:text-[var(--fc-ink)]"
                )}
              >
                {TAB_LABELS[t]}
                {t === "attention" && items.length > 0 && (
                  <span className="ml-1.5 rounded bg-[var(--fc-warn-bg)] px-1 font-mono text-[10.5px] font-semibold text-[var(--fc-warn)]">
                    {items.length}
                  </span>
                )}
                {countLabel(t) && (
                  <span
                    className={cn(
                      "ml-1.5 font-mono text-[10.5px] tabular-nums",
                      tab === t ? "opacity-80" : "text-[var(--fc-ink3)]"
                    )}
                  >
                    {countLabel(t)}
                  </span>
                )}
              </button>
            ))}
            <span className="flex-1" />
            <div className="relative">
              <Search
                size={13}
                className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[hsl(var(--muted-foreground))]"
              />
              <Input
                placeholder="Jump to task ID"
                value={searchId}
                onChange={(e) => setSearchId(e.target.value)}
                onKeyDown={handleSearchKeyDown}
                className="h-7 w-44 pl-7 text-xs"
              />
            </div>
          </div>

          {/* Tab body. Keyed by queue so page/filter/selection state resets
              when switching queues instead of leaking across them. */}
          {tab === "attention" && (
            <WorkspaceAttentionTab
              qname={queue}
              items={items}
              retryRows={retryRows}
              retryTotal={counts?.retry ?? retryRows.length}
              orphans={orphans}
              pastDue={pastDue}
              focus={focus}
              loading={!attentionLoaded}
              healthySentence={healthySummary(fleetRow, history, nowMs)}
              nowMs={nowMs}
              onOpenTab={(state: FocusState) => openTab(state)}
              onPaceRetries={() => setBulk({ verb: "run", state: "retry" })}
            />
          )}
          {tab === "active" && (
            <ActiveTasksTable
              key={queue}
              queue={queue}
              totalTaskCount={counts?.active ?? 0}
              onWholeScopeVerb={(verb) => setBulk({ verb, state: "active" })}
            />
          )}
          {tab === "pending" && (
            <PendingTasksTable
              key={queue}
              queue={queue}
              totalTaskCount={counts?.pending ?? 0}
              onWholeScopeVerb={(verb) => setBulk({ verb, state: "pending" })}
            />
          )}
          {tab === "aggregating" && (
            <AggregatingTasksTableContainer key={queue} queue={queue} />
          )}
          {tab === "scheduled" && (
            <ScheduledTasksTable
              key={queue}
              queue={queue}
              totalTaskCount={counts?.scheduled ?? 0}
              onWholeScopeVerb={(verb) => setBulk({ verb, state: "scheduled" })}
            />
          )}
          {tab === "retry" && (
            <RetryTasksTable
              key={queue}
              queue={queue}
              totalTaskCount={counts?.retry ?? 0}
              onWholeScopeVerb={(verb) => setBulk({ verb, state: "retry" })}
            />
          )}
          {tab === "archived" && (
            <ArchivedTasksTable
              key={queue}
              queue={queue}
              totalTaskCount={counts?.archived ?? 0}
              onWholeScopeVerb={(verb) => setBulk({ verb, state: "archived" })}
            />
          )}
          {tab === "completed" && (
            <CompletedTasksTable
              key={queue}
              queue={queue}
              totalTaskCount={counts?.completed ?? 0}
              onWholeScopeVerb={(verb) => setBulk({ verb, state: "completed" })}
            />
          )}
        </div>

        {/* E. Right rail */}
        <WorkspaceRail
          clusters={clusters}
          clustersTruncated={clustersTruncated}
          clustersScanned={clustersScanned}
          clustersUnavailable={clustersFailed}
          coverageRow={coverageRow}
          retryHist={retryHist}
          waitSample={waitSample}
          memory={{
            bytes: memoryBytes,
            sampledAt: memorySampledAt,
            sampling: memSampling,
            onSample: () => sampleQueue(true),
          }}
          onClusterVerb={onClusterVerb}
        />
      </div>

      {/* §4.3 bulk-op modal — every whole-scope/cluster verb lands here,
          pre-scoped queue=X state=Y (error~"sig" for cluster verbs). */}
      {bulk && (
        <BulkJobModal
          open
          verb={bulk.verb}
          scope={{
            queue,
            state: bulk.state,
            q: "",
            meta: [],
            aql: bulk.aql ?? "",
          }}
          onClose={() => setBulk(null)}
          onStarted={() => {
            setBulk(null);
            fetchCore();
          }}
        />
      )}
    </div>
  );
}
