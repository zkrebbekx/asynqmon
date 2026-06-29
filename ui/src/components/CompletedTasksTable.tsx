import { useDispatch, useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Trash2 } from "lucide-react";
import { AppState } from "../store";
import {
  listCompletedTasksAsync, deleteCompletedTaskAsync,
  batchDeleteCompletedTasksAsync, deleteAllCompletedTasksAsync,
} from "../actions/tasksActions";
import { taskRowsPerPageChange } from "../actions/settingsActions";
import { taskDetailsPath } from "../paths";
import { prettifyPayload, timeAgo, uuidPrefix, stringifyDuration, durationFromSeconds } from "../utils";
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
  { key: "completed-at", label: "Completed At", align: "left" as const },
  { key: "ttl", label: "Expires In", align: "left" as const },
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
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">{timeAgo(task.completed_at)}</TableCell>
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">
        {task.ttl_seconds > 0 ? stringifyDuration(durationFromSeconds(task.ttl_seconds)) : "–"}
      </TableCell>
      {!window.READ_ONLY && (
        <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
          <TooltipProvider>
            <Tooltip><TooltipTrigger asChild>
              <Button size="icon" variant="ghost" className="h-7 w-7 text-red-500" onClick={() => dispatch(deleteCompletedTaskAsync(task.queue, task.id) as any)}>
                <Trash2 size={13} />
              </Button>
            </TooltipTrigger><TooltipContent>Delete</TooltipContent></Tooltip>
          </TooltipProvider>
        </TableCell>
      )}
    </TableRow>
  );
}

export default function CompletedTasksTable({ queue, totalTaskCount }: Props) {
  const dispatch = useDispatch();
  const { loading, error, data: tasks, batchActionPending, allActionPending } = useSelector((s: AppState) => s.tasks.completedTasks);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pageSize = useSelector((s: AppState) => s.settings.taskRowsPerPage);
  return (
    <TasksTable
      queue={queue} totalTaskCount={totalTaskCount} taskState="completed"
      loading={loading} error={error} tasks={tasks}
      batchActionPending={batchActionPending} allActionPending={allActionPending}
      pollInterval={pollInterval} pageSize={pageSize} columns={columns}
      listTasks={(q, pgn) => dispatch(listCompletedTasksAsync(q, pgn) as any)}
      batchDeleteTasks={(q, ids) => dispatch(batchDeleteCompletedTasksAsync(q, ids) as any)}
      deleteAllTasks={(q) => dispatch(deleteAllCompletedTasksAsync(q) as any)}
      taskRowsPerPageChange={(n) => dispatch(taskRowsPerPageChange(n))}
      renderRow={(rp) => <Row key={rp.task.id} {...rp} />}
    />
  );
}
