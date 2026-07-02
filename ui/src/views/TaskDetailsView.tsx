import { useMemo, useEffect, useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useSelector } from "react-redux";
import {
  ArrowLeft, AlertCircle, Copy, Check, Play, Archive, Trash2, X,
  Clock, AlertTriangle, CheckCircle2, Loader2,
} from "lucide-react";
import { AppState, useAppDispatch } from "../store";
import {
  getTaskInfoAsync,
  runScheduledTaskAsync, runRetryTaskAsync, runArchivedTaskAsync,
  archiveScheduledTaskAsync, archiveRetryTaskAsync, archivePendingTaskAsync,
  deleteScheduledTaskAsync, deleteRetryTaskAsync, deleteArchivedTaskAsync,
  deletePendingTaskAsync, deleteCompletedTaskAsync,
  cancelActiveTaskAsync,
} from "../actions/tasksActions";
import { listQueuesAsync } from "../actions/queuesActions";
import { usePolling } from "../hooks";
import { durationFromSeconds, stringifyDuration, timeAgo, formatTimestamp, prettifyPayload } from "../utils";
import ConfirmDialog from "../components/ConfirmDialog";
import QueueBreadcrumb from "../components/QueueBreadcrumb";
import SyntaxHighlighter from "../components/SyntaxHighlighter";
import { Button } from "../components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { Badge } from "../components/ui/badge";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "../components/ui/tooltip";
import { cn } from "../lib/utils";

type Dispatcher = (q: string, id: string) => any;

const runActions: Record<string, Dispatcher> = {
  scheduled: runScheduledTaskAsync,
  retry: runRetryTaskAsync,
  archived: runArchivedTaskAsync,
};
const archiveActions: Record<string, Dispatcher> = {
  scheduled: archiveScheduledTaskAsync,
  retry: archiveRetryTaskAsync,
  pending: archivePendingTaskAsync,
};
const deleteActions: Record<string, Dispatcher> = {
  scheduled: deleteScheduledTaskAsync,
  retry: deleteRetryTaskAsync,
  archived: deleteArchivedTaskAsync,
  pending: deletePendingTaskAsync,
  completed: deleteCompletedTaskAsync,
};

function stateBadgeVariant(state: string): "info" | "success" | "warning" | "destructive" | "secondary" {
  switch (state) {
    case "active": return "info";
    case "completed": return "success";
    case "retry": return "warning";
    case "archived": return "destructive";
    default: return "secondary";
  }
}

function MetaCard({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="rounded-md border border-[hsl(var(--border))] bg-[hsl(var(--background))] px-3 py-2">
      <div className="text-[11px] uppercase tracking-wide text-[hsl(var(--muted-foreground))]">{label}</div>
      <div className="mt-0.5 text-sm font-medium text-[hsl(var(--foreground))]">{value}</div>
    </div>
  );
}

interface TimelineEvent {
  icon: React.ReactNode;
  title: string;
  time?: string;
  detail?: React.ReactNode;
  tone?: "default" | "error" | "success";
}

function Timeline({ events }: { events: TimelineEvent[] }) {
  if (events.length === 0) return null;
  return (
    <ol className="relative ml-2 border-l border-[hsl(var(--border))]">
      {events.map((e, i) => (
        <li key={i} className="ml-5 pb-5 last:pb-0">
          <span
            className={cn(
              "absolute -left-[9px] flex h-[18px] w-[18px] items-center justify-center rounded-full ring-4 ring-[hsl(var(--card))]",
              e.tone === "error" ? "bg-red-500/15 text-red-500"
                : e.tone === "success" ? "bg-emerald-500/15 text-emerald-500"
                : "bg-[hsl(var(--muted))] text-[hsl(var(--muted-foreground))]"
            )}
          >
            {e.icon}
          </span>
          <div className="flex flex-wrap items-baseline gap-x-2">
            <span className="text-sm font-medium text-[hsl(var(--foreground))]">{e.title}</span>
            {e.time && <span className="text-xs text-[hsl(var(--muted-foreground))]">{e.time}</span>}
          </div>
          {e.detail && <div className="mt-1 text-xs">{e.detail}</div>}
        </li>
      ))}
    </ol>
  );
}

export default function TaskDetailsView() {
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const { qname, taskId } = useParams<{ qname: string; taskId: string }>();
  const { error, data: fetchedTaskInfo } = useSelector((s: AppState) => s.tasks.taskInfo);
  const queuesData = useSelector((s: AppState) => s.queues.data);
  const queues = useMemo(() => queuesData.map((q) => q.name), [queuesData]);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const [copied, setCopied] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const fetchTaskInfo = useMemo(() => {
    return () => { dispatch(getTaskInfoAsync(qname!, taskId!)); };
  }, [dispatch, qname, taskId]);

  usePolling(fetchTaskInfo, pollInterval, [qname, taskId]);

  // The store may still hold the previously viewed task while this one loads.
  // Render (and act on) it only if it matches the route, so a fast click on
  // Delete/Run can never hit the wrong task.
  const taskInfo =
    fetchedTaskInfo &&
    fetchedTaskInfo.id === taskId &&
    fetchedTaskInfo.queue === qname
      ? fetchedTaskInfo
      : undefined;

  useEffect(() => {
    dispatch(listQueuesAsync());
  }, [dispatch]);

  const copyId = () => {
    if (!taskInfo) return;
    navigator.clipboard?.writeText(taskInfo.id);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  const state = taskInfo?.state ?? "";
  const canRun = !window.READ_ONLY && !!runActions[state];
  const canArchive = !window.READ_ONLY && !!archiveActions[state];
  const canDelete = !window.READ_ONLY && !!deleteActions[state];
  const canCancel = !window.READ_ONLY && state === "active";

  const doRun = () => taskInfo && dispatch(runActions[state](taskInfo.queue, taskInfo.id));
  const doArchive = () => taskInfo && dispatch(archiveActions[state](taskInfo.queue, taskInfo.id));
  const doCancel = () => taskInfo && dispatch(cancelActiveTaskAsync(taskInfo.queue, taskInfo.id));
  const doDelete = async () => {
    if (!taskInfo) return;
    await dispatch(deleteActions[state](taskInfo.queue, taskInfo.id));
    navigate(-1);
  };

  // Build a lightweight lifecycle timeline from the fields asynq exposes.
  const events: TimelineEvent[] = [];
  if (taskInfo) {
    if (taskInfo.last_failed_at) {
      events.push({
        icon: <AlertTriangle size={11} />,
        title: `Last failed (attempt ${taskInfo.retried}/${taskInfo.max_retry})`,
        time: `${timeAgo(taskInfo.last_failed_at)} · ${formatTimestamp(taskInfo.last_failed_at)}`,
        tone: "error",
        detail: taskInfo.error_message ? (
          <span className="text-red-500">{taskInfo.error_message}</span>
        ) : undefined,
      });
    }
    if (taskInfo.state === "active" && taskInfo.start_time) {
      events.push({
        icon: <Loader2 size={11} />,
        title: "Processing started",
        time: `${timeAgo(taskInfo.start_time)} · ${formatTimestamp(taskInfo.start_time)}`,
      });
    }
    if ((taskInfo.state === "scheduled" || taskInfo.state === "retry") && taskInfo.next_process_at) {
      events.push({
        icon: <Clock size={11} />,
        title: "Scheduled to run",
        time: formatTimestamp(taskInfo.next_process_at),
      });
    }
    if (taskInfo.state === "completed" && taskInfo.completed_at) {
      events.push({
        icon: <CheckCircle2 size={11} />,
        title: "Completed",
        time: `${timeAgo(taskInfo.completed_at)} · ${formatTimestamp(taskInfo.completed_at)}`,
        tone: "success",
      });
    }
  }

  return (
    <div className="max-w-4xl mx-auto px-4 py-6 space-y-4">
      <QueueBreadcrumb queues={queues} queueName={qname} taskId={taskId} />

      {error ? (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : taskInfo ? (
        <>
          {/* Header card */}
          <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))] p-5">
            <div className="flex items-start justify-between gap-4">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <h1 className="text-xl font-semibold font-mono truncate text-[hsl(var(--foreground))]">{taskInfo.type}</h1>
                  <Badge variant={stateBadgeVariant(taskInfo.state)}>{taskInfo.state}</Badge>
                  {taskInfo.is_orphaned && <Badge variant="destructive">orphaned</Badge>}
                </div>
                <div className="mt-1 flex items-center gap-1.5 text-xs text-[hsl(var(--muted-foreground))]">
                  <span className="font-mono">{taskInfo.id}</span>
                  <button onClick={copyId} className="hover:text-[hsl(var(--foreground))] transition-colors">
                    {copied ? <Check size={12} className="text-emerald-500" /> : <Copy size={12} />}
                  </button>
                  <span>·</span>
                  <span>queue: <span className="text-[hsl(var(--foreground))]">{taskInfo.queue}</span></span>
                </div>
              </div>

              {/* Actions */}
              {!window.READ_ONLY && (canRun || canArchive || canDelete || canCancel) && (
                <TooltipProvider>
                  <div className="flex items-center gap-1.5 shrink-0">
                    {canRun && (
                      <Button size="sm" variant="outline" className="h-8" onClick={doRun}>
                        <Play size={13} className="mr-1.5" /> Run now
                      </Button>
                    )}
                    {canCancel && (
                      <Button size="sm" variant="outline" className="h-8" onClick={doCancel}>
                        <X size={13} className="mr-1.5" /> Cancel
                      </Button>
                    )}
                    {canArchive && (
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <Button size="icon" variant="outline" className="h-8 w-8" onClick={doArchive}>
                            <Archive size={14} />
                          </Button>
                        </TooltipTrigger>
                        <TooltipContent>Archive</TooltipContent>
                      </Tooltip>
                    )}
                    {canDelete && (
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <Button size="icon" variant="outline" className="h-8 w-8 text-red-500 hover:text-red-600" onClick={() => setConfirmDelete(true)}>
                            <Trash2 size={14} />
                          </Button>
                        </TooltipTrigger>
                        <TooltipContent>Delete</TooltipContent>
                      </Tooltip>
                    )}
                    <ConfirmDialog
                      open={confirmDelete}
                      title="Delete Task"
                      description={
                        <>
                          Delete task <strong>{taskInfo.id}</strong>? This action cannot be undone.
                        </>
                      }
                      onConfirm={() => {
                        setConfirmDelete(false);
                        doDelete();
                      }}
                      onClose={() => setConfirmDelete(false)}
                    />
                  </div>
                </TooltipProvider>
              )}
            </div>

            {/* Metadata grid */}
            <div className="mt-4 grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-2">
              <MetaCard label="Retried" value={`${taskInfo.retried} / ${taskInfo.max_retry}`} />
              {taskInfo.timeout_seconds > 0 && <MetaCard label="Timeout" value={`${taskInfo.timeout_seconds}s`} />}
              {taskInfo.deadline && <MetaCard label="Deadline" value={<span className="text-xs">{formatTimestamp(taskInfo.deadline)}</span>} />}
              {taskInfo.group && <MetaCard label="Group" value={taskInfo.group} />}
              {taskInfo.next_process_at && (taskInfo.state === "scheduled" || taskInfo.state === "retry") && (
                <MetaCard label="Next Process" value={<span className="text-xs">{formatTimestamp(taskInfo.next_process_at)}</span>} />
              )}
              {taskInfo.state === "completed" && (
                <MetaCard
                  label="Result TTL"
                  value={taskInfo.ttl_seconds > 0 ? `${stringifyDuration(durationFromSeconds(taskInfo.ttl_seconds))} left` : "expired"}
                />
              )}
            </div>
          </div>

          {/* Timeline */}
          {events.length > 0 && (
            <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))] p-5">
              <h2 className="mb-4 text-sm font-semibold text-[hsl(var(--foreground))]">Lifecycle</h2>
              <Timeline events={events} />
            </div>
          )}

          {/* Error message (prominent) */}
          {taskInfo.error_message && (
            <div className="rounded-lg border border-red-500/30 bg-red-500/5 p-5">
              <h2 className="mb-2 text-sm font-semibold text-red-500">Last Error</h2>
              <pre className="whitespace-pre-wrap break-words text-xs text-red-400">{taskInfo.error_message}</pre>
            </div>
          )}

          {/* Payload */}
          <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))] p-5">
            <h2 className="mb-3 text-sm font-semibold text-[hsl(var(--foreground))]">Payload</h2>
            {taskInfo.payload ? (
              <SyntaxHighlighter>{prettifyPayload(taskInfo.payload)}</SyntaxHighlighter>
            ) : (
              <span className="text-sm text-[hsl(var(--muted-foreground))]">No payload</span>
            )}
          </div>

          {/* Result */}
          {taskInfo.state === "completed" && (
            <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))] p-5">
              <h2 className="mb-3 text-sm font-semibold text-[hsl(var(--foreground))]">Result</h2>
              {taskInfo.result ? (
                <SyntaxHighlighter>{prettifyPayload(taskInfo.result)}</SyntaxHighlighter>
              ) : (
                <span className="text-sm text-[hsl(var(--muted-foreground))]">No result</span>
              )}
            </div>
          )}
        </>
      ) : (
        // Loading skeleton while the routed task is fetched.
        <div className="space-y-4" aria-busy="true">
          <div className="h-44 animate-pulse rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--muted))]/40" />
          <div className="h-28 animate-pulse rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--muted))]/40" />
        </div>
      )}

      <div className="pt-2">
        <Button variant="ghost" onClick={() => navigate(-1)}>
          <ArrowLeft size={16} className="mr-2" /> Go Back
        </Button>
      </div>
    </div>
  );
}
