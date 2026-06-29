import { useMemo, useEffect } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useDispatch, useSelector } from "react-redux";
import { ArrowLeft, AlertCircle } from "lucide-react";
import { AppState } from "../store";
import { getTaskInfoAsync } from "../actions/tasksActions";
import { listQueuesAsync } from "../actions/queuesActions";
import { usePolling } from "../hooks";
import { durationFromSeconds, stringifyDuration, timeAgo, prettifyPayload } from "../utils";
import QueueBreadcrumb from "../components/QueueBreadcrumb";
import SyntaxHighlighter from "../components/SyntaxHighlighter";
import { Button } from "../components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { Badge } from "../components/ui/badge";

function InfoRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-start gap-4 py-2">
      <span className="w-36 shrink-0 text-sm font-medium text-[hsl(var(--muted-foreground))]">{label}</span>
      <div className="text-sm text-[hsl(var(--foreground))]">{value}</div>
    </div>
  );
}

function stateBadgeVariant(state: string): "info" | "success" | "warning" | "destructive" | "secondary" {
  switch (state) {
    case "active": return "info";
    case "completed": return "success";
    case "retry": return "warning";
    case "archived": return "destructive";
    default: return "secondary";
  }
}

export default function TaskDetailsView() {
  const dispatch = useDispatch();
  const navigate = useNavigate();
  const { qname, taskId } = useParams<{ qname: string; taskId: string }>();
  const { loading, error, data: taskInfo } = useSelector((s: AppState) => s.tasks.taskInfo);
  const queues = useSelector((s: AppState) => s.queues.data.map((q) => q.name));
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  const fetchTaskInfo = useMemo(() => {
    return () => { dispatch(getTaskInfoAsync(qname!, taskId!) as any); };
  }, [dispatch, qname, taskId]);

  usePolling(fetchTaskInfo, pollInterval);

  useEffect(() => {
    dispatch(listQueuesAsync() as any);
  }, [dispatch]);

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
        <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))] p-6 space-y-1 divide-y divide-[hsl(var(--border))]">
          <InfoRow label="ID" value={<span className="font-mono text-xs">{taskInfo.id}</span>} />
          <InfoRow label="Type" value={<span className="font-mono text-sm font-medium">{taskInfo.type}</span>} />
          <InfoRow label="Queue" value={taskInfo.queue} />
          <InfoRow label="State" value={<Badge variant={stateBadgeVariant(taskInfo.state)}>{taskInfo.state}</Badge>} />
          {taskInfo.state === "active" && taskInfo.start_time && (
            <InfoRow label="Started" value={`${timeAgo(taskInfo.start_time)} (${taskInfo.start_time})`} />
          )}
          <InfoRow label="Retry" value={`${taskInfo.retried} / ${taskInfo.max_retry}`} />
          {taskInfo.last_failed_at && (
            <InfoRow
              label="Last Failure"
              value={
                <div>
                  <p className="text-red-500 text-xs mb-1">{taskInfo.error_message}</p>
                  <p className="text-[hsl(var(--muted-foreground))] text-xs">{taskInfo.last_failed_at}</p>
                </div>
              }
            />
          )}
          {taskInfo.next_process_at && (
            <InfoRow label="Next Process" value={taskInfo.next_process_at} />
          )}
          {taskInfo.timeout_seconds > 0 && (
            <InfoRow label="Timeout" value={`${taskInfo.timeout_seconds}s`} />
          )}
          {taskInfo.deadline && (
            <InfoRow label="Deadline" value={taskInfo.deadline} />
          )}
          <InfoRow
            label="Payload"
            value={
              taskInfo.payload ? (
                <SyntaxHighlighter>{prettifyPayload(taskInfo.payload)}</SyntaxHighlighter>
              ) : (
                <span className="text-[hsl(var(--muted-foreground))]">–</span>
              )
            }
          />
          {taskInfo.state === "completed" && (
            <>
              <InfoRow
                label="Completed"
                value={`${timeAgo(taskInfo.completed_at)} (${taskInfo.completed_at})`}
              />
              <InfoRow
                label="Result"
                value={
                  taskInfo.result ? (
                    <SyntaxHighlighter>{prettifyPayload(taskInfo.result)}</SyntaxHighlighter>
                  ) : (
                    <span className="text-[hsl(var(--muted-foreground))]">–</span>
                  )
                }
              />
              <InfoRow
                label="TTL"
                value={
                  taskInfo.ttl_seconds > 0
                    ? `${stringifyDuration(durationFromSeconds(taskInfo.ttl_seconds))} left`
                    : "expired"
                }
              />
            </>
          )}
        </div>
      ) : null}

      <div className="pt-4">
        <Button variant="ghost" onClick={() => navigate(-1)}>
          <ArrowLeft size={16} className="mr-2" /> Go Back
        </Button>
      </div>
    </div>
  );
}
