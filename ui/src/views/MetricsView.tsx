import { useCallback, useMemo, useState, lazy, Suspense } from "react";
import { useSelector } from "react-redux";
import { AlertTriangle } from "lucide-react";
import { getMetricsAsync } from "../actions/metricsActions";
import { listQueuesAsync } from "../actions/queuesActions";
import { AppState, useAppDispatch } from "../store";
import { currentUnixtime } from "../utils";
import { useQuery, usePolling } from "../hooks";
import PageShell, { panelClass } from "../components/PageShell";

// QueueMetricsChart pulls in recharts; load it lazily.
const QueueMetricsChart = lazy(() => import("../components/QueueMetricsChart"));
import MetricsFetchControls from "../components/MetricsFetchControls";

export default function MetricsView() {
  const dispatch = useAppDispatch();
  const query = useQuery();
  const { error, data: metrics } = useSelector((s: AppState) => s.metrics);
  const queuesData = useSelector((s: AppState) => s.queues.data);
  const queues = useMemo(() => queuesData.map((q) => q.name), [queuesData]);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  // Sort before joining: the key is only meant to change when the *set* of
  // queues changes. An order-sensitive key would flip on any server-side
  // reordering and retrigger the polling effect (an extra fetch + restarted
  // interval) on every poll.
  const queuesKey = useMemo(() => [...queues].sort().join(","), [queues]);

  // With no end_time pinned in the URL we're "live": each poll recomputes
  // "now" inside the fetch instead of freezing the window at mount time
  // (recomputing it during render made every render refire the fetch effect).
  const endTimeParam = query.get("end_time");
  const duration = parseInt(query.get("duration") || "60", 10);
  const [endTime, setEndTime] = useState(() =>
    endTimeParam ? parseInt(endTimeParam, 10) : currentUnixtime()
  );

  const fetchMetrics = useCallback(() => {
    const end = endTimeParam ? parseInt(endTimeParam, 10) : currentUnixtime();
    setEndTime(end);
    dispatch(listQueuesAsync());
    dispatch(getMetricsAsync(end, duration, queues));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dispatch, endTimeParam, duration, queuesKey]);

  usePolling(fetchMetrics, pollInterval, [endTimeParam ?? "live", duration, queuesKey]);

  if (!window.PROMETHEUS_SERVER_ADDRESS) {
    return (
      <PageShell>
        <div className={`${panelClass} p-8 text-center`}>
          <div className="text-sm font-semibold text-[var(--fc-ink)]">
            Prometheus Not Configured
          </div>
          <p className="mx-auto mt-2 max-w-md text-xs text-[var(--fc-ink2)]">
            Set the <code className="font-mono text-[var(--fc-ink)]">--prometheus-addr</code>{" "}
            flag when starting asynqmon to enable metrics.
          </p>
        </div>
      </PageShell>
    );
  }

  return (
    <PageShell
      className="space-y-3"
      title="Queue Metrics"
      actions={<MetricsFetchControls endTime={endTime} duration={duration} />}
    >
      {error && (
        <div className="flex items-center gap-2 rounded-md border border-[var(--fc-warn)]/45 bg-[var(--fc-warn-bg)] px-3 py-2 text-xs text-[var(--fc-warn)]">
          <AlertTriangle size={14} />
          <span>Could not load some metrics data</span>
        </div>
      )}

      {metrics && (
        <Suspense fallback={<div className="h-64 animate-pulse rounded-lg bg-[var(--fc-raise)]/60" />}>
          <QueueMetricsChart metrics={metrics} queues={queues} />
        </Suspense>
      )}
    </PageShell>
  );
}
