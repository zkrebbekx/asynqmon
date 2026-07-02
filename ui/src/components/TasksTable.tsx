import { useState, useCallback, useEffect, type ReactElement } from "react";
import { Trash2, Play, Archive, X, ChevronLeft, ChevronRight, Filter } from "lucide-react";
import { usePolling } from "../hooks";
import { prettifyPayload } from "../utils";
import { matchesQuery } from "../lib/filter";
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
import ConfirmDialog from "./ConfirmDialog";

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
  const [confirmDeleteAll, setConfirmDeleteAll] = useState(false);

  const fetchTasks = useCallback(() => {
    listTasks(queue, { size: pageSize, page: page + 1 });
  }, [listTasks, queue, pageSize, page]);

  usePolling(fetchTasks, pollInterval, [queue, pageSize, page]);

  // Clamp the page when the task count shrinks (deletions, queue drains) so
  // the user is never stranded on an empty out-of-range page.
  useEffect(() => {
    const lastPage = Math.max(0, Math.ceil(props.totalTaskCount / pageSize) - 1);
    if (page > lastPage) {
      setPage(lastPage);
      setSelectedIds([]);
    }
  }, [props.totalTaskCount, pageSize, page]);

  // Filter the current page by task type or (decoded) payload contents.
  const needle = filter.trim();
  const visibleTasks = needle
    ? props.tasks.filter(
        (t) => matchesQuery(t.type, needle) || matchesQuery(prettifyPayload(t.payload), needle)
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
  const allSelected =
    visibleTasks.length > 0 &&
    visibleTasks.every((t) => selectedIds.includes(t.id));
  const someSelected = selectedIds.length > 0 && !allSelected;

  // Selections are page-local: carrying them across pages lets bulk actions
  // hit off-screen tasks the user can no longer see.
  const goToPage = (p: number) => {
    setPage(p);
    setSelectedIds([]);
  };

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
            <>
              <Button
                size="sm"
                variant="ghost"
                className="h-7 text-red-500 hover:text-red-600 text-xs"
                disabled={props.allActionPending}
                onClick={() => setConfirmDeleteAll(true)}
              >
                Delete all ({props.totalTaskCount})
              </Button>
              <ConfirmDialog
                open={confirmDeleteAll}
                title="Delete All Tasks"
                description={
                  <>
                    Are you sure you want to delete all{" "}
                    <strong>{props.totalTaskCount}</strong> {props.taskState} tasks in
                    queue <strong>{queue}</strong>? This action cannot be undone.
                  </>
                }
                confirmLabel="Delete all"
                onConfirm={() => {
                  setConfirmDeleteAll(false);
                  props.deleteAllTasks!(queue);
                }}
                onClose={() => setConfirmDeleteAll(false)}
              />
            </>
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
              onChange={(e) => {
                setFilter(e.target.value);
                // Drop any selection that may no longer be visible so bulk
                // actions never act on filtered-out rows.
                setSelectedIds([]);
              }}
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
          {props.loading && visibleTasks.length === 0 ? (
            // Skeleton rows on first load so the empty state doesn't flash
            // before data arrives.
            Array.from({ length: 5 }, (_, i) => (
              <TableRow key={`skeleton-${i}`}>
                {!window.READ_ONLY && <TableCell className="w-10 pr-0" />}
                {props.columns.map((col) => (
                  <TableCell key={col.key}>
                    <div className="h-4 animate-pulse rounded bg-[hsl(var(--muted))]" />
                  </TableCell>
                ))}
              </TableRow>
            ))
          ) : visibleTasks.length === 0 ? (
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
                goToPage(0);
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
              onClick={() => goToPage(page - 1)}
            >
              <ChevronLeft size={14} />
            </Button>
            <Button
              size="icon"
              variant="ghost"
              className="h-7 w-7"
              disabled={page >= totalPages - 1}
              onClick={() => goToPage(page + 1)}
            >
              <ChevronRight size={14} />
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
