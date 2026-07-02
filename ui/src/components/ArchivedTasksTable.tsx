import { useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Play } from "lucide-react";
import { AppState, useAppDispatch } from "../store";
import {
  listArchivedTasksAsync, deleteArchivedTaskAsync,
  batchDeleteArchivedTasksAsync, deleteAllArchivedTasksAsync,
  runArchivedTaskAsync, batchRunArchivedTasksAsync, runAllArchivedTasksAsync,
} from "../actions/tasksActions";
import { taskRowsPerPageChange } from "../actions/settingsActions";
import { taskDetailsPath } from "../paths";
import { prettifyPayload, timeAgo, uuidPrefix } from "../utils";
import TasksTable, { RowProps } from "./TasksTable";
import { TableCell, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import SyntaxHighlighter from "./SyntaxHighlighter";
import DeleteConfirmButton from "./DeleteConfirmButton";
import { clickableRowClass, clickableRowProps } from "../lib/utils";

interface Props { queue: string; totalTaskCount: number }

const columns = [
  { key: "id", label: "ID", align: "left" as const },
  { key: "type", label: "Type", align: "left" as const },
  { key: "payload", label: "Payload", align: "left" as const },
  { key: "retried", label: "Retried", align: "left" as const },
  { key: "last-error", label: "Last Error", align: "left" as const },
  { key: "last-failed", label: "Last Failed", align: "left" as const },
  ...(!window.READ_ONLY ? [{ key: "actions", label: "Actions", align: "center" as const }] : []),
];

function Row({ task, isSelected, onSelectChange }: RowProps) {
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  return (
    <TableRow className={clickableRowClass} {...clickableRowProps(() => navigate(taskDetailsPath(task.queue, task.id)))}>
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
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))] max-w-xs truncate">{task.error_message || "–"}</TableCell>
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">{timeAgo(task.last_failed_at)}</TableCell>
      {!window.READ_ONLY && (
        <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
          <TooltipProvider>
            <div className="flex items-center justify-center gap-1">
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => dispatch(runArchivedTaskAsync(task.queue, task.id))}>
                  <Play size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Run now</TooltipContent></Tooltip>
              <DeleteConfirmButton
                description={<>Delete task <strong>{uuidPrefix(task.id)}</strong>? This action cannot be undone.</>}
                onDelete={() => dispatch(deleteArchivedTaskAsync(task.queue, task.id))}
              />
            </div>
          </TooltipProvider>
        </TableCell>
      )}
    </TableRow>
  );
}

export default function ArchivedTasksTable({ queue, totalTaskCount }: Props) {
  const dispatch = useAppDispatch();
  const { loading, error, data: tasks, batchActionPending, allActionPending } = useSelector((s: AppState) => s.tasks.archivedTasks);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pageSize = useSelector((s: AppState) => s.settings.taskRowsPerPage);
  return (
    <TasksTable
      queue={queue} totalTaskCount={totalTaskCount} taskState="archived"
      loading={loading} error={error} tasks={tasks}
      batchActionPending={batchActionPending} allActionPending={allActionPending}
      pollInterval={pollInterval} pageSize={pageSize} columns={columns}
      listTasks={(q, pgn) => dispatch(listArchivedTasksAsync(q, pgn))}
      batchDeleteTasks={(q, ids) => dispatch(batchDeleteArchivedTasksAsync(q, ids))}
      deleteAllTasks={(q) => dispatch(deleteAllArchivedTasksAsync(q))}
      batchRunTasks={(q, ids) => dispatch(batchRunArchivedTasksAsync(q, ids))}
      runAllTasks={(q) => dispatch(runAllArchivedTasksAsync(q))}
      taskRowsPerPageChange={(n) => dispatch(taskRowsPerPageChange(n))}
      renderRow={(rp) => <Row key={rp.task.id} {...rp} />}
    />
  );
}
