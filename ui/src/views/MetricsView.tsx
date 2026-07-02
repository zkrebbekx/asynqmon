import { useCallback, useMemo, useState, lazy, Suspense } from "react";
import { useSelector } from "react-redux";
import { AlertTriangle } from "lucide-react";
import { getMetricsAsync } from "../actions/metricsActions";
import { listQueuesAsync } from "../actions/queuesActions";
import { AppState, useAppDispatch } from "../store";
import { currentUnixtime } from "../utils";
import { useQuery, usePolling } from "../hooks";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { AlertCircle } from "lucide-react";

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
  const queuesKey = queues.join(",");

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
      <div className="max-w-2xl mx-auto px-4 py-16 text-center">
        <Alert>
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Prometheus Not Configured</AlertTitle>
          <AlertDescription>
            Set the <code>--prometheus-addr</code> flag when starting asynqmon to enable metrics.
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  return (
    <div className="max-w-7xl mx-auto px-4 py-8 space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Queue Metrics</h1>
        <MetricsFetchControls endTime={endTime} duration={duration} />
      </div>

      {error && (
        <div className="flex items-center gap-2 text-amber-500 text-sm">
          <AlertTriangle size={16} />
          <span>Could not load some metrics data</span>
        </div>
      )}

      {metrics && (
        <Suspense fallback={<div className="h-64 animate-pulse rounded-md bg-[hsl(var(--muted))]/40" />}>
          <QueueMetricsChart metrics={metrics} queues={queues} />
        </Suspense>
      )}
    </div>
  );
}
