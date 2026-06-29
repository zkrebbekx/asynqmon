import { useDispatch, useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Trash2, Archive, Play } from "lucide-react";
import { AppState } from "../store";
import {
  listScheduledTasksAsync, deleteScheduledTaskAsync,
  batchDeleteScheduledTasksAsync, deleteAllScheduledTasksAsync,
  runScheduledTaskAsync, batchRunScheduledTasksAsync, runAllScheduledTasksAsync,
  archiveScheduledTaskAsync, batchArchiveScheduledTasksAsync, archiveAllScheduledTasksAsync,
} from "../actions/tasksActions";
import { taskRowsPerPageChange } from "../actions/settingsActions";
import { taskDetailsPath } from "../paths";
import { prettifyPayload, timeAgo, uuidPrefix, durationBefore } from "../utils";
import TasksTable, { RowProps } from "./TasksTable";
import { TableCell, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import SyntaxHighlighter from "./SyntaxHighlighter";

interface Props { queue: string; totalTaskCount: number }

const columns = [
  { key: "id", label: "ID", align: "left" as const },
  { key: "type", label: "Type", align: "left" as const },
  { key: "payload", label: "Payload", align: "left" as const },
  { key: "retried", label: "Retried", align: "left" as const },
  { key: "process-at", label: "Process At", align: "left" as const },
  ...(!window.READ_ONLY ? [{ key: "actions", label: "Actions", align: "center" as const }] : []),
];

function Row({ task, isSelected, onSelectChange }: RowProps) {
  const dispatch = useDispatch();
  const navigate = useNavigate();
  return (
    <TableRow className="cursor-pointer" onClick={() => navigate(taskDetailsPath(task.queue, task.id))}>
      {!window.READ_ONLY && (
        <TableCell className="w-10 pr-0" onClick={(e) => e.stopPropagation()}>
          <input type="checkbox" checked={isSelected} onChange={(e) => onSelectChange(e.target.checked)} className="h-4 w-4 accent-[hsl(var(--primary))]" />
        </TableCell>
      )}
      <TableCell className="font-mono text-xs">{uuidPrefix(task.id)}</TableCell>
      <TableCell className="text-xs font-medium">{task.type}</TableCell>
      <TableCell className="max-w-sm">
        <div className="max-h-16 overflow-hidden text-xs">
          <SyntaxHighlighter>{prettifyPayload(task.payload)}</SyntaxHighlighter>
        </div>
      </TableCell>
      <TableCell className="text-xs">{task.retried}/{task.max_retry}</TableCell>
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">{durationBefore(task.next_process_at)}</TableCell>
      {!window.READ_ONLY && (
        <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
          <TooltipProvider>
            <div className="flex items-center justify-center gap-1">
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => dispatch(runScheduledTaskAsync(task.queue, task.id) as any)}>
                  <Play size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Run now</TooltipContent></Tooltip>
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => dispatch(archiveScheduledTaskAsync(task.queue, task.id) as any)}>
                  <Archive size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Archive</TooltipContent></Tooltip>
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7 text-red-500" onClick={() => dispatch(deleteScheduledTaskAsync(task.queue, task.id) as any)}>
                  <Trash2 size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Delete</TooltipContent></Tooltip>
            </div>
          </TooltipProvider>
        </TableCell>
      )}
    </TableRow>
  );
}

export default function ScheduledTasksTable({ queue, totalTaskCount }: Props) {
  const dispatch = useDispatch();
  const { loading, error, data: tasks, batchActionPending, allActionPending } = useSelector((s: AppState) => s.tasks.scheduledTasks);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pageSize = useSelector((s: AppState) => s.settings.taskRowsPerPage);
  return (
    <TasksTable
      queue={queue} totalTaskCount={totalTaskCount} taskState="scheduled"
      loading={loading} error={error} tasks={tasks}
      batchActionPending={batchActionPending} allActionPending={allActionPending}
      pollInterval={pollInterval} pageSize={pageSize} columns={columns}
      listTasks={(q, pgn) => dispatch(listScheduledTasksAsync(q, pgn) as any)}
      batchDeleteTasks={(q, ids) => dispatch(batchDeleteScheduledTasksAsync(q, ids) as any)}
      deleteAllTasks={(q) => dispatch(deleteAllScheduledTasksAsync(q) as any)}
      batchRunTasks={(q, ids) => dispatch(batchRunScheduledTasksAsync(q, ids) as any)}
      runAllTasks={(q) => dispatch(runAllScheduledTasksAsync(q) as any)}
      batchArchiveTasks={(q, ids) => dispatch(batchArchiveScheduledTasksAsync(q, ids) as any)}
      archiveAllTasks={(q) => dispatch(archiveAllScheduledTasksAsync(q) as any)}
      taskRowsPerPageChange={(n) => dispatch(taskRowsPerPageChange(n))}
      renderRow={(rp) => <Row key={rp.task.id} {...rp} />}
    />
  );
}
