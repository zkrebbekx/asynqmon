import { useState, useCallback } from "react";
import { useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Archive, Play } from "lucide-react";
import { AppState, useAppDispatch } from "../store";
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
import DeleteConfirmButton from "./DeleteConfirmButton";
import { clickableRowClass, clickableRowProps } from "../lib/utils";

interface Props { queue: string }

const columns = [
  { key: "id", label: "ID", align: "left" as const },
  { key: "type", label: "Type", align: "left" as const },
  { key: "payload", label: "Payload", align: "left" as const },
  { key: "group", label: "Group", align: "left" as const },
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
      <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">{task.group}</TableCell>
      {!window.READ_ONLY && (
        <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
          <TooltipProvider>
            <div className="flex items-center justify-center gap-1">
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => dispatch(runAggregatingTaskAsync(task.queue, task.group, task.id))}>
                  <Play size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Run now</TooltipContent></Tooltip>
              <Tooltip><TooltipTrigger asChild>
                <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => dispatch(archiveAggregatingTaskAsync(task.queue, task.group, task.id))}>
                  <Archive size={13} />
                </Button>
              </TooltipTrigger><TooltipContent>Archive</TooltipContent></Tooltip>
              <DeleteConfirmButton
                description={<>Delete task <strong>{uuidPrefix(task.id)}</strong>? This action cannot be undone.</>}
                onDelete={() => dispatch(deleteAggregatingTaskAsync(task.queue, task.group, task.id))}
              />
            </div>
          </TooltipProvider>
        </TableCell>
      )}
    </TableRow>
  );
}

export default function AggregatingTasksTableContainer({ queue }: Props) {
  const dispatch = useAppDispatch();
  const { data: groups, error: groupsError } = useSelector((s: AppState) => s.groups);
  const { loading, error, data: tasks, batchActionPending, allActionPending } = useSelector((s: AppState) => s.tasks.aggregatingTasks);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pageSize = useSelector((s: AppState) => s.settings.taskRowsPerPage);
  const [selectedGroup, setSelectedGroup] = useState<string>("");

  const fetchGroups = useCallback(() => {
    dispatch(listGroupsAsync(queue));
  }, [dispatch, queue]);

  usePolling(fetchGroups, pollInterval, [queue]);

  // Fall back to the first group when the selected one drains and disappears,
  // so the table never keeps polling a group that no longer exists.
  const currentGroup =
    selectedGroup && groups.some((g) => g.group === selectedGroup)
      ? selectedGroup
      : groups[0]?.group || "";

  if (groupsError) {
    return (
      <Alert variant="destructive" className="m-4">
        <AlertCircle className="h-4 w-4" />
        <AlertTitle>Error</AlertTitle>
        <AlertDescription>{groupsError}</AlertDescription>
      </Alert>
    );
  }

  // Aggregating tasks only exist within a group. With no groups there's nothing
  // to fetch — listing without a group name errors on the API, so show an empty
  // state instead of rendering the auto-fetching table.
  if (groups.length === 0 || currentGroup === "") {
    return (
      <div className="px-6 py-8 text-center text-sm text-[hsl(var(--muted-foreground))]">
        No task groups
      </div>
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
            {groups.map((g) => (
              <SelectItem key={g.group} value={g.group}>{g.group} ({g.size})</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <TasksTable
        // Keyed by group: switching groups resets page/selection and fetches
        // the new group's tasks immediately.
        key={currentGroup}
        queue={queue} totalTaskCount={totalCount} taskState="aggregating"
        loading={loading} error={error} tasks={tasks}
        batchActionPending={batchActionPending} allActionPending={allActionPending}
        pollInterval={pollInterval} pageSize={pageSize} columns={columns}
        listTasks={(q, pgn) => dispatch(listAggregatingTasksAsync(q, currentGroup, pgn))}
        batchDeleteTasks={(q, ids) => dispatch(batchDeleteAggregatingTasksAsync(q, currentGroup, ids))}
        deleteAllTasks={(q) => dispatch(deleteAllAggregatingTasksAsync(q, currentGroup))}
        batchRunTasks={(q, ids) => dispatch(batchRunAggregatingTasksAsync(q, currentGroup, ids))}
        runAllTasks={(q) => dispatch(runAllAggregatingTasksAsync(q, currentGroup))}
        batchArchiveTasks={(q, ids) => dispatch(batchArchiveAggregatingTasksAsync(q, currentGroup, ids))}
        archiveAllTasks={(q) => dispatch(archiveAllAggregatingTasksAsync(q, currentGroup))}
        taskRowsPerPageChange={(n) => dispatch(taskRowsPerPageChange(n))}
        renderRow={(rp) => <Row key={rp.task.id} {...rp} />}
      />
    </div>
  );
}
