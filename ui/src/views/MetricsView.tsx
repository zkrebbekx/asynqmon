import { useEffect, lazy, Suspense } from "react";
import { useDispatch, useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Info, AlertTriangle } from "lucide-react";
import { getMetricsAsync } from "../actions/metricsActions";
import { listQueuesAsync } from "../actions/queuesActions";
import { AppState } from "../store";
import { currentUnixtime } from "../utils";
import { useQuery } from "../hooks";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "../components/ui/tooltip";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { AlertCircle } from "lucide-react";

// QueueMetricsChart pulls in recharts; load it lazily.
const QueueMetricsChart = lazy(() => import("../components/QueueMetricsChart"));
import MetricsFetchControls from "../components/MetricsFetchControls";

export default function MetricsView() {
  const dispatch = useDispatch();
  const navigate = useNavigate();
  const query = useQuery();
  const { loading, error, data: metrics } = useSelector((s: AppState) => s.metrics);
  const queues = useSelector((s: AppState) => s.queues.data.map((q) => q.name));
  const endTime = parseInt(query.get("end_time") || String(currentUnixtime()), 10);
  const duration = parseInt(query.get("duration") || "60", 10);

  useEffect(() => {
    dispatch(listQueuesAsync() as any);
    dispatch(getMetricsAsync(endTime, duration, queues) as any);
  }, [dispatch, endTime, duration]);

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
