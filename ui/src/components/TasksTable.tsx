import { useState, useCallback, type ReactElement } from "react";
import { useNavigate } from "react-router-dom";
import { Trash2, Play, Archive, X, ChevronLeft, ChevronRight, Filter } from "lucide-react";
import { usePolling } from "../hooks";
import { prettifyPayload } from "../utils";
import { Input } from "./ui/input";
import { TaskInfoExtended } from "../reducers/tasksReducer";
import { TableColumn } from "../types/table";
import { PaginationOptions } from "../api";
import { TaskState } from "../types/taskState";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { Alert, AlertDescription, AlertTitle } from "./ui/alert";
import { AlertCircle } from "lucide-react";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./ui/select";
import { cn } from "../lib/utils";

export const rowsPerPageOptions = [10, 20, 50, 100];
export const defaultPageSize = 20;

export interface RowProps {
  task: TaskInfoExtended;
  isSelected: boolean;
  onSelectChange: (checked: boolean) => void;
  onActionComplete: () => void;
  allActionPending: boolean;
}

interface Props {
  queue: string;
  totalTaskCount: number;
  taskState: TaskState;
  loading: boolean;
  error: string;
  tasks: TaskInfoExtended[];
  batchActionPending: boolean;
  allActionPending: boolean;
  pollInterval: number;
  pageSize: number;
  columns: TableColumn[];

  listTasks: (qname: string, pgn: PaginationOptions) => void;
  batchDeleteTasks?: (qname: string, taskIds: string[]) => Promise<void>;
  batchRunTasks?: (qname: string, taskIds: string[]) => Promise<void>;
  batchArchiveTasks?: (qname: string, taskIds: string[]) => Promise<void>;
  batchCancelTasks?: (qname: string, taskIds: string[]) => Promise<void>;
  deleteAllTasks?: (qname: string) => Promise<void>;
  runAllTasks?: (qname: string) => Promise<void>;
  archiveAllTasks?: (qname: string) => Promise<void>;
  cancelAllTasks?: (qname: string) => Promise<void>;
  taskRowsPerPageChange: (n: number) => void;

  renderRow: (rowProps: RowProps) => ReactElement;
}

export default function TasksTable(props: Props) {
  const { pollInterval, listTasks, queue, pageSize } = props;
  const [page, setPage] = useState(0);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [filter, setFilter] = useState("");

  const fetchTasks = useCallback(() => {
    listTasks(queue, { size: pageSize, page: page + 1 });
  }, [listTasks, queue, pageSize, page]);

  usePolling(fetchTasks, pollInterval);

  // Filter the current page by task type or (decoded) payload contents.
  const needle = filter.trim().toLowerCase();
  const visibleTasks = needle
    ? props.tasks.filter(
        (t) =>
          t.type.toLowerCase().includes(needle) ||
          prettifyPayload(t.payload).toLowerCase().includes(needle)
      )
    : props.tasks;

  const handleSelectAll = (checked: boolean) => {
    setSelectedIds(checked ? visibleTasks.map((t) => t.id) : []);
  };

  const handleSelectOne = (id: string, checked: boolean) => {
    setSelectedIds((prev) =>
      checked ? [...prev, id] : prev.filter((x) => x !== id)
    );
  };

  const totalPages = Math.ceil(props.totalTaskCount / pageSize);
  const allSelected = visibleTasks.length > 0 && selectedIds.length === visibleTasks.length;
  const someSelected = selectedIds.length > 0 && !allSelected;

  if (props.error) {
    return (
      <Alert variant="destructive" className="m-4">
        <AlertCircle className="h-4 w-4" />
        <AlertTitle>Error</AlertTitle>
        <AlertDescription>{props.error}</AlertDescription>
      </Alert>
    );
  }

  return (
    <div className="flex flex-col">
      {/* Bulk action toolbar */}
      {selectedIds.length > 0 && !window.READ_ONLY && (
        <div className="flex items-center gap-2 px-4 py-2 bg-[hsl(var(--muted))]/50 border-b border-[hsl(var(--border))]">
          <span className="text-sm text-[hsl(var(--muted-foreground))]">
            {selectedIds.length} selected
          </span>
          {props.batchDeleteTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-red-500 hover:text-red-600"
              disabled={props.batchActionPending}
              onClick={async () => {
                await props.batchDeleteTasks!(queue, selectedIds);
                setSelectedIds([]);
              }}
            >
              <Trash2 size={13} className="mr-1" /> Delete
            </Button>
          )}
          {props.batchRunTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7"
              disabled={props.batchActionPending}
              onClick={async () => {
                await props.batchRunTasks!(queue, selectedIds);
                setSelectedIds([]);
              }}
            >
              <Play size={13} className="mr-1" /> Run now
            </Button>
          )}
          {props.batchArchiveTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7"
              disabled={props.batchActionPending}
              onClick={async () => {
                await props.batchArchiveTasks!(queue, selectedIds);
                setSelectedIds([]);
              }}
            >
              <Archive size={13} className="mr-1" /> Archive
            </Button>
          )}
          {props.batchCancelTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7"
              disabled={props.batchActionPending}
              onClick={async () => {
                await props.batchCancelTasks!(queue, selectedIds);
                setSelectedIds([]);
              }}
            >
              <X size={13} className="mr-1" /> Cancel
            </Button>
          )}
        </div>
      )}

      {/* All-tasks action bar */}
      {props.totalTaskCount > 0 && selectedIds.length === 0 && !window.READ_ONLY && (
        <div className="flex items-center gap-2 px-4 py-2 border-b border-[hsl(var(--border))]">
          {props.deleteAllTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-red-500 hover:text-red-600 text-xs"
              disabled={props.allActionPending}
              onClick={() => props.deleteAllTasks!(queue)}
            >
              Delete all ({props.totalTaskCount})
            </Button>
          )}
          {props.runAllTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-xs"
              disabled={props.allActionPending}
              onClick={() => props.runAllTasks!(queue)}
            >
              Run all ({props.totalTaskCount})
            </Button>
          )}
          {props.archiveAllTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-xs"
              disabled={props.allActionPending}
              onClick={() => props.archiveAllTasks!(queue)}
            >
              Archive all ({props.totalTaskCount})
            </Button>
          )}
          {props.cancelAllTasks && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-xs"
              disabled={props.allActionPending}
              onClick={() => props.cancelAllTasks!(queue)}
            >
              Cancel all ({props.totalTaskCount})
            </Button>
          )}
        </div>
      )}

      {/* Filter current page by type / payload */}
      {props.tasks.length > 0 && (
        <div className="flex items-center justify-between px-4 py-2 border-b border-[hsl(var(--border))]">
          <div className="relative">
            <Filter size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[hsl(var(--muted-foreground))]" />
            <Input
              placeholder="Filter by type or payload"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              className="pl-7 h-8 w-64 text-xs"
            />
          </div>
          {needle && (
            <span className="text-xs text-[hsl(var(--muted-foreground))]">
              {visibleTasks.length} of {props.tasks.length} on this page
            </span>
          )}
        </div>
      )}

      <Table>
        <TableHeader>
          <TableRow>
            {!window.READ_ONLY && (
              <TableHead className="w-10 pr-0">
                <input
                  type="checkbox"
                  checked={allSelected}
                  ref={(el) => { if (el) el.indeterminate = someSelected; }}
                  onChange={(e) => handleSelectAll(e.target.checked)}
                  className="h-4 w-4 accent-[hsl(var(--primary))]"
                />
              </TableHead>
            )}
            {props.columns.map((col) => (
              <TableHead key={col.key} className={col.align === "right" ? "text-right" : col.align === "center" ? "text-center" : ""}>
                {col.label}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {visibleTasks.length === 0 ? (
            <TableRow>
              <TableCell
                colSpan={props.columns.length + (window.READ_ONLY ? 0 : 1)}
                className="text-center py-8 text-[hsl(var(--muted-foreground))]"
              >
                {needle ? `No tasks match "${filter}" on this page` : "No tasks"}
              </TableCell>
            </TableRow>
          ) : (
            visibleTasks.map((task) =>
              props.renderRow({
                task,
                isSelected: selectedIds.includes(task.id),
                onSelectChange: (checked) => handleSelectOne(task.id, checked),
                onActionComplete: fetchTasks,
                allActionPending: props.allActionPending,
              })
            )
          )}
        </TableBody>
      </Table>

      {/* Pagination */}
      {props.totalTaskCount > 0 && (
        <div className="flex items-center justify-between px-4 py-3 border-t border-[hsl(var(--border))]">
          <div className="flex items-center gap-2 text-xs text-[hsl(var(--muted-foreground))]">
            <span>Rows per page:</span>
            <Select
              value={String(pageSize)}
              onValueChange={(v) => {
                props.taskRowsPerPageChange(Number(v));
                setPage(0);
              }}
            >
              <SelectTrigger className="h-7 w-16 text-xs">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {rowsPerPageOptions.map((n) => (
                  <SelectItem key={n} value={String(n)}>{n}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <span>
              {page * pageSize + 1}–{Math.min((page + 1) * pageSize, props.totalTaskCount)} of {props.totalTaskCount}
            </span>
          </div>
          <div className="flex items-center gap-1">
            <Button
              size="icon"
              variant="ghost"
              className="h-7 w-7"
              disabled={page === 0}
              onClick={() => setPage(page - 1)}
            >
              <ChevronLeft size={14} />
            </Button>
            <Button
              size="icon"
              variant="ghost"
              className="h-7 w-7"
              disabled={page >= totalPages - 1}
              onClick={() => setPage(page + 1)}
            >
              <ChevronRight size={14} />
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
