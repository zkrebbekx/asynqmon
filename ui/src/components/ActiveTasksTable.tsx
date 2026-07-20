import { useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { X } from "lucide-react";
import { AppState, useAppDispatch } from "../store";
import {
  listActiveTasksAsync, cancelActiveTaskAsync,
  batchCancelActiveTasksAsync, cancelAllActiveTasksAsync,
} from "../actions/tasksActions";
import { taskRowsPerPageChange } from "../actions/settingsActions";
import { taskDetailsPath } from "../paths";
import { prettifyPayload, uuidPrefix, durationBefore, durationSince, formatTimestamp } from "../utils";
import TasksTable, { RowProps } from "./TasksTable";
import { TableCell, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { Badge } from "./ui/badge";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import SyntaxHighlighter from "./SyntaxHighlighter";
import { cn, clickableRowClass, clickableRowProps } from "../lib/utils";

interface Props {
  queue: string;
  totalTaskCount: number;
}

const columns = [
  { key: "id", label: "ID", align: "left" as const },
  { key: "type", label: "Type", align: "left" as const },
  { key: "payload", label: "Payload", align: "left" as const },
  { key: "status", label: "Status", align: "left" as const },
  { key: "running-for", label: "Running For", align: "left" as const },
  { key: "deadline", label: "Deadline", align: "left" as const },
  ...(!window.READ_ONLY ? [{ key: "actions", label: "Actions", align: "center" as const }] : []),
];

function Row({ task, isSelected, onSelectChange }: RowProps) {
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  return (
    <TableRow
      className={cn(clickableRowClass, isSelected && "bg-[hsl(var(--muted))]/60")}
      {...clickableRowProps(() => navigate(taskDetailsPath(task.queue, task.id)))}
    >
      {!window.READ_ONLY && (
        <TableCell className="w-10 pr-0" onClick={(e) => e.stopPropagation()}>
          <input
            type="checkbox"
            checked={isSelected}
            onChange={(e) => onSelectChange(e.target.checked)}
            className="h-4 w-4 accent-[hsl(var(--primary))]"
          />
        </TableCell>
      )}
      <TableCell className="font-mono text-xs">{uuidPrefix(task.id)}</TableCell>
      <TableCell className="max-w-xs">
        <span className="font-medium text-xs">{task.type}</span>
      </TableCell>
      <TableCell className="max-w-sm">
        <div className="max-h-16 overflow-hidden text-xs">
          <SyntaxHighlighter>{prettifyPayload(task.payload)}</SyntaxHighlighter>
        </div>
      </TableCell>
      <TableCell>
        {task.is_orphaned ? (
          <Badge variant="warning">orphaned</Badge>
        ) : (
          <Badge variant="info">active</Badge>
        )}
      </TableCell>
      <TableCell
        className="text-xs text-[hsl(var(--muted-foreground))]"
        title={formatTimestamp(task.start_time)}
      >
        {durationSince(task.start_time)}
      </TableCell>
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">{durationBefore(task.deadline)}</TableCell>
      {!window.READ_ONLY && (
        <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
          <TooltipProvider>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  size="icon"
                  variant="ghost"
                  className="h-7 w-7"
                  onClick={() => dispatch(cancelActiveTaskAsync(task.queue, task.id))}
                >
                  <X size={14} />
                </Button>
              </TooltipTrigger>
              <TooltipContent>Cancel</TooltipContent>
            </Tooltip>
          </TooltipProvider>
        </TableCell>
      )}
    </TableRow>
  );
}

export default function ActiveTasksTable({ queue, totalTaskCount }: Props) {
  const dispatch = useAppDispatch();
  const { loading, error, data: tasks, batchActionPending, allActionPending } = useSelector(
    (s: AppState) => s.tasks.activeTasks
  );
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pageSize = useSelector((s: AppState) => s.settings.taskRowsPerPage);

  return (
    <TasksTable
      queue={queue}
      totalTaskCount={totalTaskCount}
      taskState="active"
      loading={loading}
      error={error}
      tasks={tasks}
      batchActionPending={batchActionPending}
      allActionPending={allActionPending}
      pollInterval={pollInterval}
      pageSize={pageSize}
      columns={columns}
      listTasks={(q, pgn) => dispatch(listActiveTasksAsync(q, pgn))}
      batchCancelTasks={(q, ids) => dispatch(batchCancelActiveTasksAsync(q, ids))}
      cancelAllTasks={(q) => dispatch(cancelAllActiveTasksAsync(q))}
      taskRowsPerPageChange={(n) => dispatch(taskRowsPerPageChange(n))}
      renderRow={(rp) => <Row key={rp.task.id} {...rp} />}
    />
  );
}
