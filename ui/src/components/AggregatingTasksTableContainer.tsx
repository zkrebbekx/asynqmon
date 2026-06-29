import { useState, useCallback } from "react";
import { useDispatch, useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Trash2, Archive, Play } from "lucide-react";
import { AppState } from "../store";
import { listGroupsAsync } from "../actions/groupsActions";
import {
  listAggregatingTasksAsync, deleteAggregatingTaskAsync,
  batchDeleteAggregatingTasksAsync, deleteAllAggregatingTasksAsync,
  runAggregatingTaskAsync, batchRunAggregatingTasksAsync, runAllAggregatingTasksAsync,
  archiveAggregatingTaskAsync, batchArchiveAggregatingTasksAsync, archiveAllAggregatingTasksAsync,
} from "../actions/tasksActions";
import { taskRowsPerPageChange } from "../actions/settingsActions";
import { taskDetailsPath } from "../paths";
import { prettifyPayload, uuidPrefix } from "../utils";
import { usePolling } from "../hooks";
import TasksTable, { RowProps } from "./TasksTable";
import { TableCell, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./ui/select";
import { Alert, AlertDescription, AlertTitle } from "./ui/alert";
import { AlertCircle } from "lucide-react";
import SyntaxHighlighter from "./SyntaxHighlighter";

interface Props { queue: string }

const columns = [
  { key: "id", label: "ID", align: "left" as const },
  { key: "type", label: "Type", align: "left" as const },
  { key: "payload", label: "Payload", align: "left" as const },
  { key: "group", label: "Group", align: "left" as const },
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
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">{task.group}</TableCell>
      {!window.READ_ONLY && (
        <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
          <TooltipProvider>
            <div className="flex items-center justify-center gap-1">
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => dispatch(runAggregatingTaskAsync(task.queue, task.group, task.id) as any)}>
                  <Play size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Run now</TooltipContent></Tooltip>
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => dispatch(archiveAggregatingTaskAsync(task.queue, task.group, task.id) as any)}>
                  <Archive size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Archive</TooltipContent></Tooltip>
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7 text-red-500" onClick={() => dispatch(deleteAggregatingTaskAsync(task.queue, task.group, task.id) as any)}>
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

export default function AggregatingTasksTableContainer({ queue }: Props) {
  const dispatch = useDispatch();
  const { data: groups, error: groupsError } = useSelector((s: AppState) => s.groups);
  const { loading, error, data: tasks, batchActionPending, allActionPending } = useSelector((s: AppState) => s.tasks.aggregatingTasks);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pageSize = useSelector((s: AppState) => s.settings.taskRowsPerPage);
  const [selectedGroup, setSelectedGroup] = useState<string>("");

  const fetchGroups = useCallback(() => {
    dispatch(listGroupsAsync(queue) as any);
  }, [dispatch, queue]);

  usePolling(fetchGroups, pollInterval);

  const currentGroup = selectedGroup || groups[0]?.group || "";

  if (groupsError) {
    return (
      <Alert variant="destructive" className="m-4">
        <AlertCircle className="h-4 w-4" />
        <AlertTitle>Error</AlertTitle>
        <AlertDescription>{groupsError}</AlertDescription>
      </Alert>
    );
  }

  const totalCount = groups.find((g) => g.group === currentGroup)?.size ?? 0;

  return (
    <div>
      <div className="px-4 py-2 border-b border-[hsl(var(--border))]">
        <Select value={currentGroup} onValueChange={setSelectedGroup}>
          <SelectTrigger className="w-48 h-8 text-xs">
            <SelectValue placeholder="Select group" />
          </SelectTrigger>
          <SelectContent>
            {(groups as any[]).map((g) => (
              <SelectItem key={g.group} value={g.group}>{g.group} ({g.size})</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <TasksTable
        queue={queue} totalTaskCount={totalCount} taskState="aggregating"
        loading={loading} error={error} tasks={tasks}
        batchActionPending={batchActionPending} allActionPending={allActionPending}
        pollInterval={pollInterval} pageSize={pageSize} columns={columns}
        listTasks={(q, pgn) => dispatch(listAggregatingTasksAsync(q, currentGroup, pgn) as any)}
        batchDeleteTasks={(q, ids) => dispatch(batchDeleteAggregatingTasksAsync(q, currentGroup, ids) as any)}
        deleteAllTasks={(q) => dispatch(deleteAllAggregatingTasksAsync(q, currentGroup) as any)}
        batchRunTasks={(q, ids) => dispatch(batchRunAggregatingTasksAsync(q, currentGroup, ids) as any)}
        runAllTasks={(q) => dispatch(runAllAggregatingTasksAsync(q, currentGroup) as any)}
        batchArchiveTasks={(q, ids) => dispatch(batchArchiveAggregatingTasksAsync(q, currentGroup, ids) as any)}
        archiveAllTasks={(q) => dispatch(archiveAllAggregatingTasksAsync(q, currentGroup) as any)}
        taskRowsPerPageChange={(n) => dispatch(taskRowsPerPageChange(n))}
        renderRow={(rp) => <Row key={rp.task.id} {...rp} />}
      />
    </div>
  );
}
