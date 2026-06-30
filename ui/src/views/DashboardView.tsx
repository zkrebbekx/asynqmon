import { useEffect, lazy, Suspense } from "react";
import { useSelector, useDispatch } from "react-redux";
import { Info, AlertCircle } from "lucide-react";
import { AppState } from "../store";
import { listQueuesAsync, pauseQueueAsync, resumeQueueAsync, deleteQueueAsync } from "../actions/queuesActions";
import { listQueueStatsAsync } from "../actions/queueStatsActions";
import { dailyStatsKeyChange } from "../actions/settingsActions";
import { DailyStatsKey } from "../constants";
import { usePolling } from "../hooks";
// Charts pull in recharts; load them after the dashboard shell + table paint.
const QueueSizeChart = lazy(() => import("../components/QueueSizeChart"));
const ProcessedTasksChart = lazy(() => import("../components/ProcessedTasksChart"));
const DailyStatsChart = lazy(() => import("../components/DailyStatsChart"));
import QueuesOverviewTable from "../components/QueuesOverviewTable";

function ChartSkeleton() {
  return (
    <div className="flex h-full w-full items-center justify-center">
      <div className="h-full w-full animate-pulse rounded-md bg-[hsl(var(--muted))]/40" />
    </div>
  );
}
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "../components/ui/tooltip";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";

export default function DashboardView() {
  const dispatch = useDispatch();
  const { loading, data: queueData, error } = useSelector((s: AppState) => s.queues);
  const queueStats = useSelector((s: AppState) => s.queueStats.data);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const dailyStatsKey = useSelector((s: AppState) => s.settings.dailyStatsChartType);

  const queues = queueData.map((q) => ({
    ...q.currentStats,
    requestPending: q.requestPending,
  }));

  usePolling(() => dispatch(listQueuesAsync() as any), pollInterval);

  const qnames = queues.map((q) => q.queue).sort().join(",");
  useEffect(() => {
    dispatch(listQueueStatsAsync() as any);
  }, [dispatch, qnames]);

  const processedStats = queues.map((q) => ({
    queue: q.queue,
    succeeded: q.processed - q.failed,
    failed: q.failed,
  }));

  return (
    <div className="max-w-7xl mx-auto px-4 py-8 space-y-6">
      {error.length > 0 && (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>Could not retrieve queues live data — see the logs for details.</AlertDescription>
        </Alert>
      )}

      {/* Charts row */}
      <div className="grid grid-cols-2 gap-4">
        <Card>
          <CardHeader className="pb-2">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-1.5">
                <CardTitle className="text-sm font-semibold text-[hsl(var(--foreground))]">Queue Size</CardTitle>
                <TooltipProvider>
                  <Tooltip>
                    <TooltipTrigger>
                      <Info size={14} className="text-[hsl(var(--muted-foreground))]" />
                    </TooltipTrigger>
                    <TooltipContent className="max-w-xs">
                      <p>Total number of tasks in each queue broken down by state.</p>
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              </div>
            </div>
          </CardHeader>
          <CardContent className="h-72 pb-4">
            <Suspense fallback={<ChartSkeleton />}>
              <QueueSizeChart data={queues} />
            </Suspense>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-1.5">
                <CardTitle className="text-sm font-semibold text-[hsl(var(--foreground))]">Tasks Processed</CardTitle>
                <TooltipProvider>
                  <Tooltip>
                    <TooltipTrigger>
                      <Info size={14} className="text-[hsl(var(--muted-foreground))]" />
                    </TooltipTrigger>
                    <TooltipContent className="max-w-xs">
                      <p>Total number of tasks processed over time.</p>
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              </div>
              <Select
                value={dailyStatsKey}
                onValueChange={(v) => dispatch(dailyStatsKeyChange(v as DailyStatsKey))}
              >
                <SelectTrigger className="w-32 h-7 text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="today">Today</SelectItem>
                  <SelectItem value="last-7d">Last 7d</SelectItem>
                  <SelectItem value="last-30d">Last 30d</SelectItem>
                  <SelectItem value="last-90d">Last 90d</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </CardHeader>
          <CardContent className="h-72 pb-4">
            <Suspense fallback={<ChartSkeleton />}>
              {dailyStatsKey === "today" && <ProcessedTasksChart data={processedStats} />}
              {dailyStatsKey === "last-7d" && <DailyStatsChart data={queueStats} numDays={7} />}
              {dailyStatsKey === "last-30d" && <DailyStatsChart data={queueStats} numDays={30} />}
              {dailyStatsKey === "last-90d" && <DailyStatsChart data={queueStats} numDays={90} />}
            </Suspense>
          </CardContent>
        </Card>
      </div>

      {/* Queues table */}
      <Card>
        <QueuesOverviewTable
          queues={queues}
          onPauseClick={(qname) => dispatch(pauseQueueAsync(qname) as any)}
          onResumeClick={(qname) => dispatch(resumeQueueAsync(qname) as any)}
          onDeleteClick={(qname) => dispatch(deleteQueueAsync(qname) as any)}
        />
      </Card>
    </div>
  );
}
