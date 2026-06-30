import { useEffect, useMemo, useState, useCallback } from "react";
import { useDispatch, useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Search, Play, Trash2, Archive, X, ChevronLeft, ChevronRight, Tag, AlertCircle } from "lucide-react";
import { AppState } from "../store";
import { listQueuesAsync } from "../actions/queuesActions";
import { pollTick } from "../actions/settingsActions";
import * as api from "../api";
import { TaskInfo, ListTasksResponse, PaginationOptions } from "../api";
import { taskDetailsPath } from "../paths";
import { prettifyPayload, uuidPrefix } from "../utils";
import { matchesQuery } from "../lib/filter";
import { matchesMetadata, collectMetadata, metaId, MetaPair } from "../lib/metadata";
import { cn } from "../lib/utils";
import { Input } from "../components/ui/input";
import { Button } from "../components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "../components/ui/tooltip";
import SyntaxHighlighter from "../components/SyntaxHighlighter";

type ListFn = (qname: string, opts?: PaginationOptions) => Promise<ListTasksResponse>;
type ActionFn = (qname: string, taskId: string) => Promise<unknown>;

const STATES = ["active", "pending", "scheduled", "retry", "archived", "completed"] as const;
type State = (typeof STATES)[number];

const listFns: Record<State, ListFn> = {
  active: api.listActiveTasks,
  pending: api.listPendingTasks,
  scheduled: api.listScheduledTasks,
  retry: api.listRetryTasks,
  archived: api.listArchivedTasks,
  completed: api.listCompletedTasks,
};

const actionFns: Record<State, { run?: ActionFn; archive?: ActionFn; delete?: ActionFn; cancel?: ActionFn }> = {
  active: { cancel: api.cancelActiveTask },
  pending: { delete: api.deletePendingTask, archive: api.archivePendingTask },
  scheduled: { run: api.runScheduledTask, archive: api.archiveScheduledTask, delete: api.deleteScheduledTask },
  retry: { run: api.runRetryTask, archive: api.archiveRetryTask, delete: api.deleteRetryTask },
  archived: { run: api.runArchivedTask, delete: api.deleteArchivedTask },
  completed: { delete: api.deleteCompletedTask },
};

const PER_QUEUE_FETCH = 100;
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
  const [metaFilters, setMetaFilters] = useState<MetaPair[]>([]);
  const [tasks, setTasks] = useState<TaskInfo[]>([]);
  const [error, setError] = useState("");
  const [page, setPage] = useState(0);

  useEffect(() => {
    dispatch(listQueuesAsync() as any);
  }, [dispatch]);

  const targetQueues = selectedQueue === "all" ? queues : [selectedQueue];
  const queueKey = `${selectedQueue}|${queues.join(",")}`;

  const fetchTasks = useCallback(async () => {
    if (targetQueues.length === 0) {
      setTasks([]);
      return;
    }
    const listFn = listFns[selectedState];
    try {
      const perQueue = await Promise.all(
        targetQueues.map((q) =>
          listFn(q, { size: PER_QUEUE_FETCH, page: 1 })
            .then((r) => r.tasks ?? [])
            .catch(() => [] as TaskInfo[])
        )
      );
      setTasks(perQueue.flat());
      setError("");
    } catch (e) {
      setError(String(e));
    }
    dispatch(pollTick());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedState, queueKey, dispatch]);

  // Immediate refetch on control change + periodic polling.
  useEffect(() => {
    fetchTasks();
    if (!pollingActive) return;
    const id = setInterval(fetchTasks, pollInterval * 1000);
    return () => clearInterval(id);
  }, [fetchTasks, pollingActive, pollInterval]);

  // Reset to first page whenever the result set could change.
  useEffect(() => {
    setPage(0);
  }, [selectedQueue, selectedState, search, metaFilters]);

  // search over id / type / queue / payload
  const searchFiltered = useMemo(
    () =>
      tasks.filter((t) =>
        matchesQuery(`${t.id} ${t.type} ${t.queue} ${prettifyPayload(t.payload)}`, search)
      ),
    [tasks, search]
  );
  const filtered = useMemo(
    () => searchFiltered.filter((t) => matchesMetadata(t.payload, metaFilters)),
    [searchFiltered, metaFilters]
  );

  // Metadata chips discovered from the currently-filtered set, minus active ones.
  const activeIds = new Set(metaFilters.map(metaId));
  const chips = useMemo(
    () => collectMetadata(filtered.map((t) => t.payload)).filter((p) => !activeIds.has(metaId(p))),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filtered, metaFilters]
  );

  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const pageTasks = filtered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);

  const addFilter = (p: MetaPair) => setMetaFilters((prev) => [...prev, p]);
  const removeFilter = (p: MetaPair) =>
    setMetaFilters((prev) => prev.filter((x) => metaId(x) !== metaId(p)));

  const acts = actionFns[selectedState];
  const runAction = async (fn: ActionFn, queue: string, id: string) => {
    await fn(queue, id);
    fetchTasks();
  };

  return (
    <div className="max-w-7xl mx-auto px-4 py-6 space-y-4">
      <h1 className="text-2xl font-semibold">Tasks</h1>

      {/* Controls */}
      <div className="flex flex-wrap items-center gap-2">
        {/* Queue filter */}
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

        {/* State tabs */}
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
              {selectedState === st && <span className="opacity-70">{filtered.length}</span>}
            </button>
          ))}
        </div>

        {/* Search */}
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
            onClick={() => addFilter(p)}
            className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs border border-[hsl(var(--border))] text-[hsl(var(--muted-foreground))] hover:border-[hsl(var(--primary))] hover:text-[hsl(var(--foreground))] transition-colors"
          >
            {p.key}: {p.value}
          </button>
        ))}
        {metaFilters.length === 0 && chips.length === 0 && (
          <span className="text-xs text-[hsl(var(--muted-foreground))]">No metadata to filter on</span>
        )}
      </div>

      {error && (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
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
            {pageTasks.length === 0 ? (
              <TableRow>
                <TableCell colSpan={window.READ_ONLY ? 4 : 5} className="text-center py-8 text-[hsl(var(--muted-foreground))]">
                  No tasks
                </TableCell>
              </TableRow>
            ) : (
              pageTasks.map((t) => (
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
        {filtered.length > PAGE_SIZE && (
          <div className="flex items-center justify-between px-4 py-3 border-t border-[hsl(var(--border))]">
            <span className="text-xs text-[hsl(var(--muted-foreground))]">
              {page * PAGE_SIZE + 1}–{Math.min((page + 1) * PAGE_SIZE, filtered.length)} of {filtered.length}
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

      <p className="text-xs text-[hsl(var(--muted-foreground))]">
        Showing up to {PER_QUEUE_FETCH} tasks per queue. Refine with search or metadata filters to narrow results.
      </p>
    </div>
  );
}
