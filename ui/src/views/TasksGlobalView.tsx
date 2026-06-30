import { useEffect, useMemo, useState, useCallback } from "react";
import { useDispatch, useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Search, Play, Trash2, Archive, X, ChevronLeft, ChevronRight, Tag, AlertCircle, AlertTriangle } from "lucide-react";
import { AppState } from "../store";
import { listQueuesAsync } from "../actions/queuesActions";
import { pollTick } from "../actions/settingsActions";
import * as api from "../api";
import { TaskInfo } from "../api";
import { taskDetailsPath } from "../paths";
import { prettifyPayload, uuidPrefix } from "../utils";
import { metaId, MetaPair } from "../lib/metadata";
import { cn } from "../lib/utils";
import { Input } from "../components/ui/input";
import { Button } from "../components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "../components/ui/tooltip";
import SyntaxHighlighter from "../components/SyntaxHighlighter";

type ActionFn = (qname: string, taskId: string) => Promise<unknown>;

const STATES = ["active", "pending", "scheduled", "retry", "archived", "completed"] as const;
type State = (typeof STATES)[number];

const actionFns: Record<State, { run?: ActionFn; archive?: ActionFn; delete?: ActionFn; cancel?: ActionFn }> = {
  active: { cancel: api.cancelActiveTask },
  pending: { delete: api.deletePendingTask, archive: api.archivePendingTask },
  scheduled: { run: api.runScheduledTask, archive: api.archiveScheduledTask, delete: api.deleteScheduledTask },
  retry: { run: api.runRetryTask, archive: api.archiveRetryTask, delete: api.deleteRetryTask },
  archived: { run: api.runArchivedTask, delete: api.deleteArchivedTask },
  completed: { delete: api.deleteCompletedTask },
};

const PAGE_SIZE = 20;

export default function TasksGlobalView() {
  const dispatch = useDispatch();
  const navigate = useNavigate();
  const queues = useSelector((s: AppState) => s.queues.data.map((q) => q.name));
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pollingActive = useSelector((s: AppState) => s.settings.pollingActive);

  const [selectedQueue, setSelectedQueue] = useState<string>("all");
  const [selectedState, setSelectedState] = useState<State>("pending");
  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [metaFilters, setMetaFilters] = useState<MetaPair[]>([]);
  const [tasks, setTasks] = useState<TaskInfo[]>([]);
  const [total, setTotal] = useState(0);
  const [truncated, setTruncated] = useState(false);
  const [facets, setFacets] = useState<{ key: string; value: string; count: number }[]>([]);
  const [error, setError] = useState("");
  const [page, setPage] = useState(0);
  // Failure-analytics groupings (only for retry/archived).
  const [errorGroups, setErrorGroups] = useState<{ label: string; count: number }[]>([]);
  const [typeGroups, setTypeGroups] = useState<{ label: string; count: number }[]>([]);
  // Two-step confirm for bulk-on-filter actions.
  const [confirmAction, setConfirmAction] = useState<"run" | "archive" | "delete" | "cancel" | null>(null);
  const [bulkBusy, setBulkBusy] = useState(false);

  useEffect(() => {
    dispatch(listQueuesAsync() as any);
  }, [dispatch]);

  // Debounce the search box so typing doesn't hammer the server-side scan.
  useEffect(() => {
    const id = setTimeout(() => setDebouncedSearch(search), 300);
    return () => clearTimeout(id);
  }, [search]);

  const metaParam = metaFilters.map((p) => `${p.key}:${p.value}`);
  const metaKey = metaParam.join("|");

  const fetchTasks = useCallback(async () => {
    try {
      const resp = await api.searchTasks({
        queue: selectedQueue,
        state: selectedState,
        q: debouncedSearch,
        meta: metaParam,
        page: page + 1,
        size: PAGE_SIZE,
      });
      setTasks(resp.tasks ?? []);
      setTotal(resp.total);
      setTruncated(resp.truncated);
      setError("");
    } catch (e) {
      setError(String(e));
    }
    dispatch(pollTick());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedQueue, selectedState, debouncedSearch, metaKey, page, dispatch]);

  useEffect(() => {
    fetchTasks();
    if (!pollingActive) return;
    const id = setInterval(fetchTasks, pollInterval * 1000);
    return () => clearInterval(id);
  }, [fetchTasks, pollingActive, pollInterval]);

  // Global metadata facets (across the whole filtered set, not just this page).
  const fetchFacets = useCallback(async () => {
    try {
      const resp = await api.taskMetadata({
        queue: selectedQueue,
        state: selectedState,
        q: debouncedSearch,
        meta: metaParam,
      });
      setFacets(resp.facets);
    } catch {
      setFacets([]);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedQueue, selectedState, debouncedSearch, metaKey]);

  useEffect(() => {
    fetchFacets();
  }, [fetchFacets]);

  // Failure analytics: group retry/archived tasks by error + type.
  const isFailureState = selectedState === "retry" || selectedState === "archived";
  const fetchAnalytics = useCallback(async () => {
    if (!isFailureState) {
      setErrorGroups([]);
      setTypeGroups([]);
      return;
    }
    try {
      const [byError, byType] = await Promise.all([
        api.taskAggregate({ queue: selectedQueue, state: selectedState, q: debouncedSearch, meta: metaParam, by: "error", limit: 8 }),
        api.taskAggregate({ queue: selectedQueue, state: selectedState, q: debouncedSearch, meta: metaParam, by: "type", limit: 8 }),
      ]);
      setErrorGroups(byError.groups);
      setTypeGroups(byType.groups);
    } catch {
      setErrorGroups([]);
      setTypeGroups([]);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isFailureState, selectedQueue, selectedState, debouncedSearch, metaKey]);

  useEffect(() => {
    fetchAnalytics();
  }, [fetchAnalytics]);

  // Reset to first page when filters change (page itself is excluded).
  useEffect(() => {
    setPage(0);
  }, [selectedQueue, selectedState, debouncedSearch, metaKey]);

  // Chips = global facets minus already-active filters.
  const activeIds = new Set(metaFilters.map(metaId));
  const chips = useMemo(
    () => facets.filter((p) => !activeIds.has(metaId(p))),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [facets, metaKey]
  );

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const addFilter = (p: MetaPair) => setMetaFilters((prev) => [...prev, p]);
  const removeFilter = (p: MetaPair) =>
    setMetaFilters((prev) => prev.filter((x) => metaId(x) !== metaId(p)));

  const acts = actionFns[selectedState];
  const runAction = async (fn: ActionFn, queue: string, id: string) => {
    await fn(queue, id);
    fetchTasks();
  };

  const applyBulk = async (action: "run" | "archive" | "delete" | "cancel") => {
    setBulkBusy(true);
    try {
      await api.bulkFilteredTasks({
        queue: selectedQueue,
        state: selectedState,
        q: debouncedSearch,
        meta: metaParam,
        action,
      });
      setError("");
    } catch (e) {
      setError(String(e));
    }
    setBulkBusy(false);
    setConfirmAction(null);
    fetchTasks();
    fetchFacets();
    fetchAnalytics();
  };

  // Which bulk actions are valid for the current state (mirrors per-row actions).
  const bulkActions: { action: "run" | "archive" | "delete" | "cancel"; label: string }[] = [];
  if (acts.run) bulkActions.push({ action: "run", label: "Run" });
  if (acts.cancel) bulkActions.push({ action: "cancel", label: "Cancel" });
  if (acts.archive) bulkActions.push({ action: "archive", label: "Archive" });
  if (acts.delete) bulkActions.push({ action: "delete", label: "Delete" });

  return (
    <div className="max-w-7xl mx-auto px-4 py-6 space-y-4">
      <h1 className="text-2xl font-semibold">Tasks</h1>

      {/* Controls */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex items-center gap-1.5">
          <span className="text-xs text-[hsl(var(--muted-foreground))]">Queue</span>
          <select
            value={selectedQueue}
            onChange={(e) => setSelectedQueue(e.target.value)}
            className="h-8 rounded-md border border-[hsl(var(--input))] bg-[hsl(var(--background))] px-2 text-xs"
          >
            <option value="all">All queues</option>
            {queues.map((q) => (
              <option key={q} value={q}>{q}</option>
            ))}
          </select>
        </div>

        <div className="flex items-center gap-1 flex-wrap">
          {STATES.map((st) => (
            <button
              key={st}
              onClick={() => setSelectedState(st)}
              className={cn(
                "inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-xs font-medium border transition-colors capitalize",
                selectedState === st
                  ? "bg-[hsl(var(--primary))] text-[hsl(var(--primary-foreground))] border-[hsl(var(--primary))]"
                  : "border-[hsl(var(--border))] text-[hsl(var(--muted-foreground))] hover:border-[hsl(var(--primary))] hover:text-[hsl(var(--foreground))]"
              )}
            >
              {st}
              {selectedState === st && <span className="opacity-70">{total}</span>}
            </button>
          ))}
        </div>

        <div className="relative ml-auto">
          <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[hsl(var(--muted-foreground))]" />
          <Input
            placeholder="Search name, queue, metadata"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="pl-7 h-8 w-64 text-xs"
          />
        </div>
      </div>

      {/* Metadata filter chips */}
      <div className="flex flex-wrap items-center gap-1.5">
        <Tag size={13} className="text-[hsl(var(--muted-foreground))]" />
        {metaFilters.map((p) => (
          <button
            key={metaId(p)}
            onClick={() => removeFilter(p)}
            className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs bg-[hsl(var(--primary))] text-[hsl(var(--primary-foreground))]"
          >
            {p.key}: {p.value}
            <X size={11} />
          </button>
        ))}
        {chips.map((p) => (
          <button
            key={metaId(p)}
            onClick={() => addFilter({ key: p.key, value: p.value })}
            className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs border border-[hsl(var(--border))] text-[hsl(var(--muted-foreground))] hover:border-[hsl(var(--primary))] hover:text-[hsl(var(--foreground))] transition-colors"
          >
            {p.key}: {p.value}
            <span className="opacity-60">{p.count}</span>
          </button>
        ))}
        {metaFilters.length === 0 && chips.length === 0 && (
          <span className="text-xs text-[hsl(var(--muted-foreground))]">No metadata to filter on</span>
        )}
      </div>

      {/* Failure analytics (retry/archived) */}
      {isFailureState && (errorGroups.length > 0 || typeGroups.length > 0) && (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
          <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))] p-4">
            <h3 className="mb-2 text-sm font-semibold">Top errors</h3>
            <div className="space-y-1">
              {errorGroups.length === 0 && <span className="text-xs text-[hsl(var(--muted-foreground))]">None</span>}
              {errorGroups.map((g) => (
                <div key={g.label} className="flex items-center justify-between gap-3 text-xs">
                  <span className="truncate text-red-500" title={g.label}>{g.label}</span>
                  <span className="shrink-0 tabular-nums text-[hsl(var(--muted-foreground))]">{g.count}</span>
                </div>
              ))}
            </div>
          </div>
          <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))] p-4">
            <h3 className="mb-2 text-sm font-semibold">Top failing types</h3>
            <div className="space-y-1">
              {typeGroups.length === 0 && <span className="text-xs text-[hsl(var(--muted-foreground))]">None</span>}
              {typeGroups.map((g) => (
                <button
                  key={g.label}
                  onClick={() => setSearch(g.label)}
                  className="flex w-full items-center justify-between gap-3 text-xs hover:text-[hsl(var(--primary))]"
                  title={`Filter by ${g.label}`}
                >
                  <span className="truncate font-medium">{g.label}</span>
                  <span className="shrink-0 tabular-nums text-[hsl(var(--muted-foreground))]">{g.count}</span>
                </button>
              ))}
            </div>
          </div>
        </div>
      )}

      {/* Bulk-on-filter toolbar */}
      {!window.READ_ONLY && total > 0 && bulkActions.length > 0 && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--muted))]/30 px-4 py-2">
          <span className="text-xs text-[hsl(var(--muted-foreground))]">
            Apply to all <span className="font-semibold text-[hsl(var(--foreground))]">{truncated ? `${total}+` : total}</span> matching:
          </span>
          {bulkActions.map((b) => (
            <Button
              key={b.action}
              size="sm"
              variant={b.action === "delete" ? "outline" : "ghost"}
              className={cn("h-7 text-xs", b.action === "delete" && "text-red-500 hover:text-red-600")}
              disabled={bulkBusy}
              onClick={() => setConfirmAction(b.action)}
            >
              {b.label} all
            </Button>
          ))}
          {confirmAction && (
            <div className="flex items-center gap-2 ml-2 pl-2 border-l border-[hsl(var(--border))]">
              <span className="text-xs text-[hsl(var(--foreground))]">
                {confirmAction} {truncated ? `${total}+` : total} task(s)?
              </span>
              <Button size="sm" className="h-7 text-xs" disabled={bulkBusy} onClick={() => applyBulk(confirmAction)}>
                Confirm
              </Button>
              <Button size="sm" variant="ghost" className="h-7 text-xs" disabled={bulkBusy} onClick={() => setConfirmAction(null)}>
                Cancel
              </Button>
            </div>
          )}
        </div>
      )}

      {error && (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      {truncated && (
        <div className="flex items-center gap-2 text-xs text-amber-500">
          <AlertTriangle size={14} />
          <span>Result set is large; counts are capped by the scan limit. Narrow with search or metadata filters for exact totals.</span>
        </div>
      )}

      {/* Table */}
      <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))]">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Queue</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Payload</TableHead>
              {!window.READ_ONLY && <TableHead className="text-center">Actions</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {tasks.length === 0 ? (
              <TableRow>
                <TableCell colSpan={window.READ_ONLY ? 4 : 5} className="text-center py-8 text-[hsl(var(--muted-foreground))]">
                  No tasks
                </TableCell>
              </TableRow>
            ) : (
              tasks.map((t) => (
                <TableRow
                  key={`${t.queue}:${t.id}`}
                  className="cursor-pointer"
                  onClick={() => navigate(taskDetailsPath(t.queue, t.id))}
                >
                  <TableCell className="font-mono text-xs">{uuidPrefix(t.id)}</TableCell>
                  <TableCell className="text-xs">{t.queue}</TableCell>
                  <TableCell className="text-xs font-medium">{t.type}</TableCell>
                  <TableCell className="max-w-sm">
                    <div className="max-h-16 overflow-hidden text-xs">
                      <SyntaxHighlighter>{prettifyPayload(t.payload)}</SyntaxHighlighter>
                    </div>
                  </TableCell>
                  {!window.READ_ONLY && (
                    <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
                      <TooltipProvider>
                        <div className="flex items-center justify-center gap-1">
                          {acts.run && (
                            <Tooltip><TooltipTrigger asChild>
                              <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => runAction(acts.run!, t.queue, t.id)}>
                                <Play size={13} />
                              </Button>
                            </TooltipTrigger><TooltipContent>Run now</TooltipContent></Tooltip>
                          )}
                          {acts.cancel && (
                            <Tooltip><TooltipTrigger asChild>
                              <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => runAction(acts.cancel!, t.queue, t.id)}>
                                <X size={13} />
                              </Button>
                            </TooltipTrigger><TooltipContent>Cancel</TooltipContent></Tooltip>
                          )}
                          {acts.archive && (
                            <Tooltip><TooltipTrigger asChild>
                              <Button size="icon" variant="ghost" className="h-7 w-7" onClick={() => runAction(acts.archive!, t.queue, t.id)}>
                                <Archive size={13} />
                              </Button>
                            </TooltipTrigger><TooltipContent>Archive</TooltipContent></Tooltip>
                          )}
                          {acts.delete && (
                            <Tooltip><TooltipTrigger asChild>
                              <Button size="icon" variant="ghost" className="h-7 w-7 text-red-500" onClick={() => runAction(acts.delete!, t.queue, t.id)}>
                                <Trash2 size={13} />
                              </Button>
                            </TooltipTrigger><TooltipContent>Delete</TooltipContent></Tooltip>
                          )}
                        </div>
                      </TooltipProvider>
                    </TableCell>
                  )}
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>

        {/* Pagination */}
        {total > PAGE_SIZE && (
          <div className="flex items-center justify-between px-4 py-3 border-t border-[hsl(var(--border))]">
            <span className="text-xs text-[hsl(var(--muted-foreground))]">
              {page * PAGE_SIZE + 1}–{Math.min((page + 1) * PAGE_SIZE, total)} of {truncated ? `${total}+` : total}
            </span>
            <div className="flex items-center gap-1">
              <Button size="icon" variant="ghost" className="h-7 w-7" disabled={page === 0} onClick={() => setPage(page - 1)}>
                <ChevronLeft size={14} />
              </Button>
              <Button size="icon" variant="ghost" className="h-7 w-7" disabled={page >= totalPages - 1} onClick={() => setPage(page + 1)}>
                <ChevronRight size={14} />
              </Button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
