import { useEffect, useMemo, useState, useCallback, useRef } from "react";
import { useSelector } from "react-redux";
import { Link, useSearchParams } from "react-router-dom";
import { Search, Play, Trash2, Archive, X, ChevronLeft, ChevronRight, Tag, AlertCircle, BookmarkPlus } from "lucide-react";
import { AppState, useAppDispatch } from "../store";
import { listQueuesAsync } from "../actions/queuesActions";
import * as api from "../api";
import { TaskInfo } from "../api";
import { prettifyPayload, toErrorString, uuidPrefix } from "../utils";
import { metaId, MetaPair } from "../lib/metadata";
import {
  ConsoleState,
  CONSOLE_STATES,
  PAGE_SIZES,
  RESULT_MODES,
  parseConsoleState,
  serializeConsoleState,
  parsePeek,
  serializePeek,
  PeekTarget,
} from "../lib/urlstate";
import {
  addMetaClause,
  removeMetaClause,
  setClause,
  isAqlQuery,
  isAqlRejection,
  caretLine,
  AqlRejection,
} from "../lib/aql";
import { paths } from "../paths";
import { cn, clickableRowClass, clickableRowProps } from "../lib/utils";
import { usePolling } from "../hooks";
import { useKeymap } from "../hooks/useKeymap";
import { useFrozenData } from "../hooks/useFrozenData";
import { STATE_BULK_VERBS } from "../lib/palette";
import { Button } from "../components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "../components/ui/tooltip";
import SyntaxHighlighter from "../components/SyntaxHighlighter";
import ConfirmDialog from "../components/ConfirmDialog";
import BulkJobModal from "../components/BulkJobModal";
import TaskDrawer, { PivotRequest } from "../components/TaskDrawer";
import SaveViewModal from "../components/SaveViewModal";
import UpdatesPausedPill from "../components/UpdatesPausedPill";

type ActionFn = (qname: string, taskId: string) => Promise<unknown>;

type State = (typeof CONSOLE_STATES)[number];

// Per-row actions. Aggregating tasks have none here: acting on one requires
// its group name, which the cross-queue search response doesn't carry —
// bulk-on-filter actions below still cover them.
const actionFns: Record<State, { run?: ActionFn; archive?: ActionFn; delete?: ActionFn; cancel?: ActionFn }> = {
  active: { cancel: api.cancelActiveTask },
  pending: { delete: api.deletePendingTask, archive: api.archivePendingTask },
  aggregating: {},
  scheduled: { run: api.runScheduledTask, archive: api.archiveScheduledTask, delete: api.deleteScheduledTask },
  retry: { run: api.runRetryTask, archive: api.archiveRetryTask, delete: api.deleteRetryTask },
  archived: { run: api.runArchivedTask, delete: api.deleteArchivedTask },
  completed: { delete: api.deleteCompletedTask },
};

// Bulk-on-filter capabilities per state (the backend acts by queue + task id,
// which works for aggregating tasks even though per-row actions can't).
// Shared with the ⌘K palette's verb entries (lib/palette).
const bulkCaps = STATE_BULK_VERBS;

// Verb used for "Scan-to-completion as job": the LEAST destructive verb the
// state supports — the point is the completed preview enumeration (exact
// count on the Operations screen), never an execution.
const scanJobVerb: Record<State, api.JobVerb> = {
  active: "cancel",
  pending: "archive",
  aggregating: "archive",
  scheduled: "archive",
  retry: "archive",
  archived: "run",
  completed: "delete",
};

// AQL wire fields of one search response the console renders from.
interface SearchMeta {
  mode: "cursor" | "scan" | "legacy";
  exact: boolean;
  cursor: string;
  scanCursor: string;
  candidateEstimate: number;
  scanned: number;
}

export default function TasksGlobalView() {
  const dispatch = useAppDispatch();
  // Select the stable data reference and derive names in a memo — mapping
  // inside the selector returns a fresh array per dispatch and re-renders
  // this view on every poll tick of unrelated slices.
  const queuesData = useSelector((s: AppState) => s.queues.data);
  const queues = useMemo(() => queuesData.map((q) => q.name), [queuesData]);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  // All console state lives in the URL (build contract §2); since phase 6
  // the `q` param carries AQL and is the single source of truth — queue,
  // state and metadata chips are clauses of the query string.
  const [searchParams, setSearchParams] = useSearchParams();
  const view = useMemo(() => parseConsoleState(searchParams), [searchParams]);
  const peek = useMemo(() => parsePeek(searchParams.get("peek")), [searchParams]);
  const isAql = useMemo(() => isAqlQuery(view.q), [view.q]);

  // updateView merges console-state changes into the URL, preserving params
  // it doesn't own (peek). Discrete changes (tabs, chips, paging) push a
  // history entry; keystroke-driven changes replace instead.
  const updateView = useCallback(
    (updates: Partial<ConsoleState>, opts?: { replace?: boolean }) => {
      setSearchParams(
        (prev) => serializeConsoleState({ ...parseConsoleState(prev), ...updates }, prev),
        { replace: opts?.replace ?? false }
      );
    },
    [setSearchParams]
  );

  // Drawer open/close/navigate is the `peek` param. Opening pushes (Back
  // closes the drawer); prev/next and close replace so walking a result list
  // doesn't bloat history.
  const setPeekParam = useCallback(
    (target: PeekTarget | null, opts?: { replace?: boolean }) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          if (target) next.set("peek", serializePeek(target));
          else next.delete("peek");
          return next;
        },
        { replace: opts?.replace ?? false }
      );
    },
    [setSearchParams]
  );

  // The query bar needs immediate local echo; the URL write is debounced so
  // typing doesn't hammer the server-side scan or spam history (replace).
  const [searchInput, setSearchInput] = useState(view.q);
  const inputRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    // Sync from the URL when it changes externally (back/forward, pivots).
    setSearchInput(view.q);
  }, [view.q]);
  useEffect(() => {
    if (searchInput === view.q) return;
    const id = setTimeout(() => updateView({ q: searchInput, page: 0 }, { replace: true }), 300);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [searchInput]);

  // Queue typeahead: when the query ends in a partial `queue=` token, offer
  // completions from the fleet queues cache. Selecting one edits the TEXT —
  // chips and completions compose into the query, never beside it.
  const [typeaheadOpen, setTypeaheadOpen] = useState(false);
  const queuePrefix = useMemo(() => {
    const m = /(?:^|\s)queue=(\S*)$/.exec(searchInput);
    return m ? m[1] : null;
  }, [searchInput]);
  const queueSuggestions = useMemo(() => {
    if (queuePrefix === null) return [];
    const p = queuePrefix.toLowerCase();
    return queues.filter((q) => q.toLowerCase().startsWith(p) && q !== queuePrefix).slice(0, 8);
  }, [queuePrefix, queues]);
  const applyQueueSuggestion = (name: string) => {
    const next = searchInput.replace(/(?:^|\s)queue=\S*$/, (m) =>
      m.startsWith(" ") ? ` queue=${name}` : `queue=${name}`
    );
    setSearchInput(next);
    setTypeaheadOpen(false);
    inputRef.current?.focus();
  };

  const [tasks, setTasks] = useState<TaskInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [total, setTotal] = useState(0);
  const [truncated, setTruncated] = useState(false);
  const [meta, setMeta] = useState<SearchMeta>({
    mode: "legacy", exact: false, cursor: "", scanCursor: "", candidateEstimate: 0, scanned: 0,
  });
  const [facets, setFacets] = useState<{ key: string; value: string; count: number }[]>([]);
  const [error, setError] = useState("");
  // Structured AQL rejection ({error, position, hint}) rendered inline with
  // a caret under the offending clause. Last-good results stay visible.
  const [rejection, setRejection] = useState<(AqlRejection & { query: string }) | null>(null);
  // Failure-analytics groupings (only for retry/archived).
  const [errorGroups, setErrorGroups] = useState<{ label: string; count: number }[]>([]);
  const [typeGroups, setTypeGroups] = useState<{ label: string; count: number }[]>([]);
  // All-seven state-pill counts (state_counts endpoint, polled).
  const [stateCounts, setStateCounts] = useState<Record<string, number> | null>(null);
  // Whole-scope verbs open the §4.3 bulk-job modal (preview job + cost
  // disclosure + gated execute). Selection/row-scoped actions keep their
  // direct endpoints — cheap and exact.
  const [bulkVerb, setBulkVerb] = useState<"run" | "archive" | "delete" | "cancel" | null>(null);
  // Row-level delete confirmation target.
  const [confirmDelete, setConfirmDelete] = useState<TaskInfo | null>(null);

  // §4.5 keyboard model: focused row (j/k, visible ring), selection (x /
  // shift-x range; keys are "<queue>/<id>"), and the selection-verb confirm
  // (r/e/# — always through a confirm, never instant). §4.4 freeze input:
  // hovering the rows table.
  const [focusIdx, setFocusIdx] = useState(-1);
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const selectAnchor = useRef(-1);
  const [selectionVerb, setSelectionVerb] = useState<"run" | "archive" | "delete" | null>(null);
  const [hoveringTable, setHoveringTable] = useState(false);
  // "Save view…" modal (§4.2).
  const [saveViewOpen, setSaveViewOpen] = useState(false);

  // Cursor pagination (mode=cursor): the stack's top is the cursor of the
  // CURRENT page; [] = first page. Exact totals come with every response.
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const currentCursor = cursorStack[cursorStack.length - 1] ?? "";

  // Progressive-scan continuation (§5.9): Continue extends coverage by
  // feeding the resume cursor; results append below the first window.
  const [extraTasks, setExtraTasks] = useState<TaskInfo[]>([]);
  const [extraScanned, setExtraScanned] = useState(0);
  const [extraMatches, setExtraMatches] = useState(0);
  // null = not continued yet (follow the base response's cursor); "" = done.
  const [contCursor, setContCursor] = useState<string | null>(null);
  const [continuing, setContinuing] = useState(false);
  // Scan-to-completion preview job handoff.
  const [scanJobId, setScanJobId] = useState<string | null>(null);

  // After a drawer pivot, close the drawer if the peeked task fell out of
  // the visible results (kept open when it survived the new filter).
  const pivotPending = useRef(false);
  const peekRef = useRef(peek);
  peekRef.current = peek;

  useEffect(() => {
    dispatch(listQueuesAsync());
  }, [dispatch]);

  const filterKey = `${view.q}|${view.state}|${view.queue}|${view.size}`;

  const fetchTasks = useCallback(async () => {
    try {
      const resp = await api.searchTasks({
        queue: view.queue,
        state: view.state,
        q: view.q,
        page: view.page + 1,
        size: view.size,
        cursor: currentCursor || undefined,
      });
      const rows = resp.tasks ?? [];
      setTasks(rows);
      setTotal(resp.total);
      setTruncated(resp.truncated);
      setMeta({
        mode: resp.mode ?? "legacy",
        exact: resp.exact ?? false,
        cursor: resp.cursor ?? "",
        scanCursor: resp.scan_cursor ?? "",
        candidateEstimate: resp.candidate_estimate ?? 0,
        scanned: resp.scanned ?? 0,
      });
      setError("");
      setRejection(null);
      if (pivotPending.current) {
        pivotPending.current = false;
        const pk = peekRef.current;
        if (pk && !rows.some((t) => t.id === pk.id && t.queue === pk.queue)) {
          setPeekParam(null, { replace: true });
        }
      }
    } catch (e) {
      const data = (e as { response?: { status?: number; data?: unknown } })?.response;
      if (data?.status === 400 && isAqlRejection(data.data)) {
        setRejection({ ...data.data, query: view.q });
        setError("");
      } else {
        setError(toErrorString(e));
      }
    }
    setLoading(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view.queue, view.state, view.q, view.page, view.size, currentCursor]);

  // usePolling gives pause-on-hidden-tab and the shared "Updated Ns ago"
  // indicator for free; the fetch key refetches immediately on filter change.
  usePolling(fetchTasks, pollInterval, [view.queue, view.state, view.q, view.page, view.size, currentCursor]);

  // All-seven state pill counts: one pipelined pass server-side, polled.
  const fetchCounts = useCallback(async () => {
    try {
      const resp = await api.taskStateCounts(view.queue);
      setStateCounts(resp.counts);
    } catch {
      /* keep the last counts — pills degrade to stale, not empty */
    }
  }, [view.queue]);
  usePolling(fetchCounts, pollInterval, [view.queue]);

  // Global metadata facets (across the whole filtered set, not just this page).
  const fetchFacets = useCallback(async () => {
    try {
      const resp = await api.taskMetadata({
        queue: view.queue,
        state: view.state,
        q: view.q,
      });
      setFacets(resp.facets);
    } catch {
      setFacets([]);
    }
  }, [view.queue, view.state, view.q]);

  useEffect(() => {
    fetchFacets();
  }, [fetchFacets]);

  // Failure analytics: group retry/archived tasks by error + type. In a
  // group result mode the groups ARE the result set, so fetch more of them.
  const isFailureState = view.state === "retry" || view.state === "archived";
  const isGroupMode = isFailureState && view.mode !== "rows";
  const fetchAnalytics = useCallback(async () => {
    if (!isFailureState) {
      setErrorGroups([]);
      setTypeGroups([]);
      return;
    }
    const limit = isGroupMode ? 50 : 8;
    try {
      const [byError, byType] = await Promise.all([
        api.taskAggregate({ queue: view.queue, state: view.state, q: view.q, by: "error", limit }),
        api.taskAggregate({ queue: view.queue, state: view.state, q: view.q, by: "type", limit }),
      ]);
      setErrorGroups(byError.groups);
      setTypeGroups(byType.groups);
    } catch {
      setErrorGroups([]);
      setTypeGroups([]);
    }
  }, [isFailureState, isGroupMode, view.queue, view.state, view.q]);

  useEffect(() => {
    fetchAnalytics();
  }, [fetchAnalytics]);

  // Drop the now-mismatched rows and all pagination/continuation state when
  // the filter changes so stale windows don't linger.
  useEffect(() => {
    setTasks([]);
    setLoading(true);
    setCursorStack([]);
    setExtraTasks([]);
    setExtraScanned(0);
    setExtraMatches(0);
    setContCursor(null);
    setScanJobId(null);
    setFocusIdx(-1);
    setSelected(new Set());
    selectAnchor.current = -1;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filterKey]);

  // Chips = global facets minus already-active clauses (view.meta derives
  // from the query string's meta.* clauses).
  const activeIds = new Set(view.meta.map(metaId));
  const metaKey = view.meta.map(metaId).join("|");
  const chips = useMemo(
    () => facets.filter((p) => !activeIds.has(metaId(p))),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [facets, metaKey]
  );

  const totalPages = Math.max(1, Math.ceil(total / view.size));
  const isCursorMode = meta.mode === "cursor";

  // Clamp the page when the filtered total shrinks (bulk actions, drains) so
  // the user is never stranded on an empty out-of-range page.
  useEffect(() => {
    if (!loading && !isCursorMode && view.page > totalPages - 1) {
      updateView({ page: totalPages - 1 }, { replace: true });
    }
  }, [loading, isCursorMode, view.page, totalPages, updateView]);

  // ---- All console mutations edit the QUERY STRING (single source of truth).
  const setStateClause = (st: State) =>
    updateView({ q: setClause(view.q, "state", "=", st), page: 0 });
  const addFilter = (p: MetaPair) =>
    updateView({ q: addMetaClause(view.q, p.key, p.value), page: 0 });
  const removeFilter = (p: MetaPair) =>
    updateView({ q: removeMetaClause(view.q, p.key, p.value), page: 0 });

  const acts = actionFns[view.state];
  const runAction = async (fn: ActionFn, queue: string, id: string) => {
    try {
      await fn(queue, id);
      setError("");
    } catch (e) {
      setError(toErrorString(e));
    }
    // Mutations invalidate immediately (§4.4): the refetch bypasses an
    // active freeze once. Assigned later (hoisted via ref-style flag).
    applyNextChangeRef.current?.();
    fetchTasks();
  };
  // applyNextChange comes from useFrozenData below; runAction is declared
  // first, so it reaches it through a ref.
  const applyNextChangeRef = useRef<(() => void) | null>(null);

  // Drawer pivots ("find all like this") compose into the query string.
  const handlePivot = (p: PivotRequest) => {
    pivotPending.current = true;
    let q = view.q;
    if (p.q !== undefined) q = setClause(q, "type", "=", p.q);
    if (p.queue) q = setClause(q, "queue", "=", p.queue);
    if (p.meta) q = addMetaClause(q, p.meta.key, p.meta.value);
    updateView({ q, page: 0 });
  };

  // Scan continuation: feed the resume cursor, append the new window.
  const effectiveScanCursor = contCursor ?? meta.scanCursor;
  const scanPartial = meta.mode === "scan" && effectiveScanCursor !== "";
  const scannedTotal = meta.scanned + extraScanned;
  const continueScan = async () => {
    if (!effectiveScanCursor || continuing) return;
    setContinuing(true);
    try {
      const resp = await api.searchTasks({
        queue: view.queue,
        state: view.state,
        q: view.q,
        size: view.size,
        scanCursor: effectiveScanCursor,
      });
      setExtraTasks((prev) => [...prev, ...(resp.tasks ?? [])]);
      setExtraScanned((s) => s + (resp.scanned ?? 0));
      setExtraMatches((m) => m + (resp.total ?? 0));
      setContCursor(resp.scan_cursor ?? "");
    } catch (e) {
      setError(toErrorString(e));
    }
    setContinuing(false);
  };

  // Scan-to-completion as a preview job: the job runner enumerates the whole
  // scope server-side and reports the exact final count on /ops (§3.4 —
  // exactness moves into the job, not the preview). Least-destructive verb;
  // it is never executed from here.
  const scanAsJob = async () => {
    try {
      const j = await api.createJob({
        verb: scanJobVerb[view.state],
        scope: {
          queue: view.queue,
          state: view.state,
          q: !isAql && view.q ? view.q : undefined,
          aql: isAql ? view.q : undefined,
        },
        reason: "Scan-to-completion count for console query (preview only)",
      });
      setScanJobId(j.id);
    } catch (e) {
      setError(toErrorString(e));
    }
  };

  // Which bulk actions are valid for the current state.
  const bulkActions = bulkCaps[view.state].map((action) => ({
    action,
    label: action.charAt(0).toUpperCase() + action.slice(1),
  }));

  const groupRows = view.mode === "group:error" ? errorGroups : typeGroups;
  const visibleTasks = useMemo(() => {
    if (extraTasks.length === 0) return tasks;
    const seen = new Set(tasks.map((t) => `${t.queue}:${t.id}`));
    return [...tasks, ...extraTasks.filter((t) => !seen.has(`${t.queue}:${t.id}`))];
  }, [tasks, extraTasks]);

  // §4.4 freeze-on-interaction: hovering the table, holding a selection, or
  // having the drawer open suspends ROW rendering — data is still fetched
  // into the buffer, the pill counts the suspended changes, and counts
  // (state pills, totals) stay live. Mutations flush via applyNextChange.
  //
  // Equality compares only the fields the table RENDERS (id/queue/type/
  // payload/state + order): asynq recomputes e.g. next_process_at to "now"
  // on every read of a pending task, and an invisible field must not count
  // as a paused update.
  const renderedRowsEqual = useCallback((a: TaskInfo[], b: TaskInfo[]) => {
    if (a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) {
      const x = a[i];
      const y = b[i];
      if (
        x.id !== y.id ||
        x.queue !== y.queue ||
        x.type !== y.type ||
        x.payload !== y.payload ||
        x.state !== y.state
      ) {
        return false;
      }
    }
    return true;
  }, []);
  const frozen = hoveringTable || selected.size > 0 || peek !== null;
  const {
    data: rows,
    pending: pausedUpdates,
    apply: applyUpdates,
    applyNextChange,
  } = useFrozenData(visibleTasks, frozen, renderedRowsEqual, filterKey);
  applyNextChangeRef.current = applyNextChange;

  // Clamp the keyboard focus when the rendered set shrinks.
  useEffect(() => {
    setFocusIdx((i) => Math.min(i, rows.length - 1));
  }, [rows.length]);

  // ---- §4.5 selection + keyboard model over the RENDERED rows.
  const rowKey = (t: TaskInfo) => `${t.queue}/${t.id}`;
  const clearSelection = () => {
    setSelected(new Set());
    selectAnchor.current = -1;
  };
  const toggleSelect = (idx: number) => {
    const t = rows[idx];
    if (!t) return;
    setSelected((prev) => {
      const next = new Set(prev);
      const k = rowKey(t);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
    selectAnchor.current = idx;
  };
  const rangeSelect = (idx: number) => {
    if (selectAnchor.current < 0) {
      toggleSelect(idx);
      return;
    }
    const a = Math.min(selectAnchor.current, idx);
    const b = Math.max(selectAnchor.current, idx);
    setSelected((prev) => {
      const next = new Set(prev);
      for (let i = a; i <= b && i < rows.length; i++) next.add(rowKey(rows[i]));
      return next;
    });
  };
  const moveFocus = (delta: number) => {
    setFocusIdx((i) => {
      if (rows.length === 0) return -1;
      const next = i < 0 ? (delta > 0 ? 0 : rows.length - 1) : Math.max(0, Math.min(rows.length - 1, i + delta));
      document.querySelector(`[data-task-row="${next}"]`)?.scrollIntoView({ block: "nearest" });
      return next;
    });
  };
  const selectedTasks = rows.filter((t) => selected.has(rowKey(t)));

  // r/e/# act on the SELECTION and always route through the confirm dialog
  // (never instant). Verbs the state doesn't support are silently inert.
  const openSelectionVerb = (verb: "run" | "archive" | "delete") => {
    if (window.READ_ONLY || selectedTasks.length === 0 || !acts[verb]) return;
    setSelectionVerb(verb);
  };
  useKeymap(
    {
      j: () => moveFocus(1),
      k: () => moveFocus(-1),
      x: () => {
        if (focusIdx >= 0) toggleSelect(focusIdx);
      },
      "shift+x": () => {
        if (focusIdx >= 0) rangeSelect(focusIdx);
      },
      p: () => {
        const t = rows[focusIdx];
        if (t) setPeekParam({ queue: t.queue, id: t.id });
      },
      r: () => openSelectionVerb("run"),
      e: () => openSelectionVerb("archive"),
      "#": () => openSelectionVerb("delete"),
    },
    !isGroupMode
  );

  const runSelectionVerb = async () => {
    const verb = selectionVerb;
    setSelectionVerb(null);
    if (!verb) return;
    const fn = acts[verb];
    if (!fn) return;
    const targets = selectedTasks;
    try {
      await Promise.all(targets.map((t) => fn(t.queue, t.id)));
      setError("");
    } catch (e) {
      setError(toErrorString(e));
    }
    clearSelection();
    applyNextChange(); // mutations invalidate immediately (§4.4)
    fetchTasks();
  };

  // The urlstate-owned params of the current console view, page stripped —
  // what "Save view…" persists (§4.2; served back verbatim).
  const consoleViewState = useMemo(() => {
    const params = serializeConsoleState({ ...view, page: 0 });
    return Object.fromEntries(params.entries());
  }, [view]);

  const countLabel = (st: State): string => {
    const n = stateCounts?.[st];
    if (n === undefined) return "";
    if (n < 0) return "—"; // unknowable (fleet-wide aggregating) — never a guess
    return n.toLocaleString("en-US");
  };

  return (
    <div className="max-w-7xl mx-auto px-4 py-6 space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Tasks</h1>
        {!window.READ_ONLY && (
          <Button
            size="sm"
            variant="outline"
            className="h-7 text-xs"
            onClick={() => setSaveViewOpen(true)}
            title="Save the current query as a server-side team view (launchable from ⌘K)"
          >
            <BookmarkPlus size={13} className="mr-1.5" />
            Save view…
          </Button>
        )}
      </div>

      {/* Query bar: AQL text input (single source of truth). Chips, pills
          and pivots all COMPOSE INTO this text. */}
      <div className="relative">
        <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-[hsl(var(--muted-foreground))]" />
        {/* Plain <input> (the shadcn Input doesn't forward refs, and the
            typeahead needs one) with the same visual classes. */}
        <input
          ref={inputRef}
          data-fc-querybar
          placeholder='AQL — e.g. queue=email state=retry error~"gateway timeout"  ·  free text still works'
          value={searchInput}
          onChange={(e) => {
            setSearchInput(e.target.value);
            setTypeaheadOpen(true);
          }}
          onFocus={() => setTypeaheadOpen(true)}
          onBlur={() => setTimeout(() => setTypeaheadOpen(false), 150)}
          className="flex h-9 w-full rounded-md border border-[hsl(var(--input))] bg-transparent py-1 pl-8 pr-3 font-mono text-xs shadow-sm transition-colors placeholder:text-[hsl(var(--muted-foreground))] focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-[hsl(var(--ring))]"
          spellCheck={false}
          autoComplete="off"
        />
        {/* queue= typeahead from the fleet queues cache */}
        {typeaheadOpen && queueSuggestions.length > 0 && (
          <div className="absolute z-20 mt-1 w-72 rounded-md border border-[hsl(var(--border))] bg-[hsl(var(--card))] shadow-lg">
            {queueSuggestions.map((name) => (
              <button
                key={name}
                onMouseDown={(e) => {
                  e.preventDefault();
                  applyQueueSuggestion(name);
                }}
                className="block w-full px-3 py-1.5 text-left font-mono text-xs hover:bg-[hsl(var(--muted))]"
              >
                queue=<span className="font-semibold">{name}</span>
              </button>
            ))}
          </div>
        )}
      </div>

      {/* Inline AQL rejection: {error, position, hint} + caret (§3.4) */}
      {rejection && (
        <div className="rounded-lg border border-[var(--fc-crit)]/50 bg-[var(--fc-crit-bg)]/60 px-4 py-3 text-xs space-y-2">
          <div className="flex items-start gap-2">
            <span className="font-bold text-[var(--fc-crit)]">✗</span>
            <div>
              <span className="font-medium">{rejection.error}</span>
              {rejection.hint && (
                <span className="text-[hsl(var(--muted-foreground))]"> — {rejection.hint}</span>
              )}
            </div>
          </div>
          <pre className="overflow-x-auto rounded bg-[hsl(var(--muted))]/40 px-2 py-1 font-mono text-[11px] leading-4">
            {rejection.query + "\n" + caretLine(rejection.query, rejection.position)}
          </pre>
        </div>
      )}

      {/* State pills — all seven counts always visible (state_counts, §3.4) */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex items-center gap-1 flex-wrap">
          {CONSOLE_STATES.map((st) => (
            <button
              key={st}
              onClick={() => setStateClause(st)}
              className={cn(
                "inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-xs font-medium border transition-colors capitalize",
                view.state === st
                  ? "bg-[hsl(var(--primary))] text-[hsl(var(--primary-foreground))] border-[hsl(var(--primary))]"
                  : "border-[hsl(var(--border))] text-[hsl(var(--muted-foreground))] hover:border-[hsl(var(--primary))] hover:text-[hsl(var(--foreground))]"
              )}
            >
              {st}
              <span className={cn("font-mono tabular-nums", view.state === st ? "opacity-80" : "opacity-60")}>
                {countLabel(st)}
              </span>
            </button>
          ))}
        </div>

        {/* Result mode (failure states only — group-by error/type) */}
        {isFailureState && (
          <div className="flex items-center gap-0.5 rounded-full border border-[hsl(var(--border))] p-0.5">
            {RESULT_MODES.map((m) => (
              <button
                key={m}
                onClick={() => updateView({ mode: m })}
                className={cn(
                  "px-2.5 py-0.5 rounded-full text-xs transition-colors",
                  view.mode === m
                    ? "bg-[hsl(var(--primary))]/10 text-[hsl(var(--primary))] font-medium"
                    : "text-[hsl(var(--muted-foreground))] hover:text-[hsl(var(--foreground))]"
                )}
              >
                {m === "rows" ? "rows" : m === "group:error" ? "group: error" : "group: type"}
              </button>
            ))}
          </div>
        )}

        {isCursorMode && (
          <span className="ml-auto text-xs text-[hsl(var(--muted-foreground))]">
            score-cursorable · totals exact
          </span>
        )}
      </div>

      {/* Scan meter (§5.9): visible whenever scan coverage is partial */}
      {meta.mode === "scan" && (scanPartial || extraScanned > 0) && (
        <div className="flex flex-wrap items-center gap-3 rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--muted))]/30 px-4 py-2 text-xs">
          <span className="text-[hsl(var(--muted-foreground))]">scan</span>
          <div className="h-1.5 w-40 overflow-hidden rounded-full bg-[hsl(var(--border))]">
            <div
              className="h-full bg-[hsl(var(--primary))]"
              style={{
                width: `${meta.candidateEstimate > 0 ? Math.min(100, (scannedTotal / meta.candidateEstimate) * 100) : 100}%`,
              }}
            />
          </div>
          <span className="font-mono tabular-nums">
            scanned {scannedTotal.toLocaleString()} of ~{meta.candidateEstimate.toLocaleString()} candidates
            {scanPartial ? " · results partial" : " · scan complete"}
          </span>
          <div className="ml-auto flex items-center gap-2">
            {scanPartial && (
              <Button size="sm" variant="outline" className="h-6 text-xs" disabled={continuing} onClick={continueScan}>
                {continuing ? "Scanning…" : "Continue"}
              </Button>
            )}
            {scanPartial && !scanJobId && (
              <Button size="sm" variant="outline" className="h-6 text-xs" onClick={scanAsJob}>
                Scan-to-completion as job
              </Button>
            )}
            {scanJobId && (
              <Link to={paths().OPS} className="text-[hsl(var(--primary))] hover:underline">
                Preview job {scanJobId.slice(0, 8)}… → Operations
              </Link>
            )}
          </div>
        </div>
      )}

      {/* Metadata facet chips — clicking COMPOSES a meta.key=value clause
          into the query text (single source of truth) */}
      <div className="flex flex-wrap items-center gap-1.5">
        <Tag size={13} className="text-[hsl(var(--muted-foreground))]" />
        {view.meta.map((p) => (
          <button
            key={metaId(p)}
            onClick={() => removeFilter(p)}
            className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs bg-[hsl(var(--primary))] text-[hsl(var(--primary-foreground))]"
            title={`Remove meta.${p.key}=${p.value} from the query`}
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
            title={`Add meta.${p.key}=${p.value} to the query`}
          >
            {p.key}: {p.value}
            <span className="opacity-60">{p.count}</span>
          </button>
        ))}
        {view.meta.length === 0 && chips.length === 0 && (
          <span className="text-xs text-[hsl(var(--muted-foreground))]">No metadata to filter on</span>
        )}
      </div>

      {/* Failure analytics cards (rows mode; group mode shows the full table) */}
      {isFailureState && !isGroupMode && (errorGroups.length > 0 || typeGroups.length > 0) && (
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
                  onClick={() => updateView({ q: setClause(view.q, "type", "=", g.label), page: 0 })}
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

      {/* Whole-scope bulk bar (§4.3): every verb opens the bulk-job modal —
          async preview, cost disclosure, throttle, mandatory reason, gated
          execute — and runs as a background job on the Operations screen.
          Visually distinct from row actions and red-guarded for delete. */}
      {!window.READ_ONLY && total > 0 && bulkActions.length > 0 && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-dashed border-[var(--fc-warn)]/50 bg-[var(--fc-warn-bg)]/40 px-4 py-2">
          <span className="text-xs text-[hsl(var(--muted-foreground))]">
            Whole scope — all{" "}
            <span className="font-semibold text-[hsl(var(--foreground))]">
              {isCursorMode ? total.toLocaleString() : truncated ? `${total}+` : total}
            </span>{" "}
            matching, as a background job:
          </span>
          {bulkActions.map((b) => (
            <Button
              key={b.action}
              size="sm"
              variant={b.action === "delete" ? "outline" : "ghost"}
              className={cn("h-7 text-xs", b.action === "delete" && "text-red-500 hover:text-red-600")}
              onClick={() => setBulkVerb(b.action)}
            >
              {b.label} all…
            </Button>
          ))}
        </div>
      )}

      {/* §4.3 bulk-op modal (whole-scope ops only). AQL queries travel as
          the scope's aql field; plain free text stays in q. */}
      {bulkVerb && (
        <BulkJobModal
          open
          verb={bulkVerb}
          scope={{
            queue: view.queue,
            state: view.state,
            q: isAql ? "" : view.q,
            meta: [],
            aql: isAql ? view.q : "",
          }}
          onClose={() => setBulkVerb(null)}
          onStarted={() => {
            setBulkVerb(null);
            applyNextChange(); // mutations invalidate immediately (§4.4)
            fetchTasks();
            fetchFacets();
            fetchAnalytics();
          }}
        />
      )}

      {error && (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      {/* Legacy free-text scans have no resume cursor; the cap is still
          disclosed (never silent truncation). */}
      {meta.mode === "legacy" && truncated && (
        <div className="text-xs text-amber-500">
          Free-text scan hit its cap — counts are a floor. Use AQL clauses (queue=, state=, meta.*) for
          budgeted, resumable scans.
        </div>
      )}

      {/* Selection-scoped action bar (§4.5): x/shift-x build the selection;
          r/e/# (or these buttons) act on it through the confirm dialog.
          Selection-scoped — visually separate from the whole-scope bar. */}
      {!window.READ_ONLY && !isGroupMode && selected.size > 0 && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-[var(--fc-acc)]/40 bg-[var(--fc-acc-bg)]/50 px-4 py-2">
          <span className="text-xs font-medium">
            <span className="font-mono tabular-nums">{selected.size}</span> selected
          </span>
          {acts.run && (
            <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => openSelectionVerb("run")}>
              Run <span className="ml-1 opacity-50">r</span>
            </Button>
          )}
          {acts.archive && (
            <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => openSelectionVerb("archive")}>
              Archive <span className="ml-1 opacity-50">e</span>
            </Button>
          )}
          {acts.delete && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-xs text-red-500 hover:text-red-600"
              onClick={() => openSelectionVerb("delete")}
            >
              Delete <span className="ml-1 opacity-50">#</span>
            </Button>
          )}
          <span className="flex-1" />
          <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={clearSelection}>
            Clear
          </Button>
        </div>
      )}

      {/* Selection verb confirm — r/e/# never act instantly (§4.5). */}
      <ConfirmDialog
        open={selectionVerb !== null}
        title={
          selectionVerb === "run"
            ? "Run selected tasks"
            : selectionVerb === "archive"
              ? "Archive selected tasks"
              : "Delete selected tasks"
        }
        description={
          <>
            {selectionVerb === "run" ? "Run" : selectionVerb === "archive" ? "Archive" : "Delete"}{" "}
            <strong>{selectedTasks.length}</strong> selected task
            {selectedTasks.length === 1 ? "" : "s"} in state <strong>{view.state}</strong>?
            {selectionVerb === "delete" && " This action cannot be undone."}
          </>
        }
        onConfirm={runSelectionVerb}
        onClose={() => setSelectionVerb(null)}
      />

      {isGroupMode ? (
        /* Group-by result mode: the aggregate IS the result set. */
        <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))]">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{view.mode === "group:error" ? "Error" : "Type"}</TableHead>
                <TableHead className="text-right">Count</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {groupRows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={2} className="text-center py-8 text-[hsl(var(--muted-foreground))]">
                    {loading ? "Loading…" : "No groups"}
                  </TableCell>
                </TableRow>
              ) : (
                groupRows.map((g) => (
                  <TableRow
                    key={g.label}
                    className={view.mode === "group:type" ? clickableRowClass : undefined}
                    {...(view.mode === "group:type"
                      ? clickableRowProps(() =>
                          updateView({ q: setClause(view.q, "type", "=", g.label), mode: "rows", page: 0 })
                        )
                      : {})}
                  >
                    <TableCell className={cn("text-xs", view.mode === "group:error" && "text-red-500")}>
                      {g.label || <span className="italic opacity-60">(empty)</span>}
                    </TableCell>
                    <TableCell className="text-right text-xs tabular-nums">{g.count}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
          <div className="px-4 py-2 border-t border-[hsl(var(--border))] text-xs text-[hsl(var(--muted-foreground))]">
            Group counts inherit the scan's coverage — partial until the scan completes.
            {view.mode === "group:type" && " Click a type to open it as rows."}
          </div>
        </div>
      ) : (
        /* Rows result mode. Hover freezes row updates (§4.4). */
        <div
          className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))]"
          onMouseEnter={() => setHoveringTable(true)}
          onMouseLeave={() => setHoveringTable(false)}
        >
          <Table>
            <TableHeader>
              <TableRow>
                {!window.READ_ONLY && <TableHead className="w-8" aria-label="Select" />}
                <TableHead>ID</TableHead>
                <TableHead>Queue</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Payload</TableHead>
                {!window.READ_ONLY && <TableHead className="text-center">Actions</TableHead>}
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading && rows.length === 0 ? (
                // Skeleton rows while the (re)filtered set loads, so the empty
                // state doesn't flash before data arrives.
                Array.from({ length: 5 }, (_, i) => (
                  <TableRow key={`skeleton-${i}`}>
                    {Array.from({ length: window.READ_ONLY ? 4 : 6 }, (_, j) => (
                      <TableCell key={j}>
                        <div className="h-4 animate-pulse rounded bg-[hsl(var(--muted))]" />
                      </TableCell>
                    ))}
                  </TableRow>
                ))
              ) : rows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={window.READ_ONLY ? 4 : 6} className="text-center py-8 text-[hsl(var(--muted-foreground))]">
                    No tasks
                  </TableCell>
                </TableRow>
              ) : (
                rows.map((t, i) => (
                  <TableRow
                    key={`${t.queue}:${t.id}`}
                    data-task-row={i}
                    className={cn(
                      clickableRowClass,
                      peek && peek.id === t.id && peek.queue === t.queue && "bg-[hsl(var(--primary))]/5",
                      selected.has(rowKey(t)) && "bg-[var(--fc-acc-bg)]/50",
                      // Visible keyboard-focus ring (§4.5 j/k).
                      focusIdx === i && "shadow-[inset_0_0_0_1.5px_var(--fc-acc)]"
                    )}
                    // Rows open the peek drawer (deep-linkable) instead of
                    // navigating away — detail is a drawer, never a
                    // context-losing navigation.
                    {...clickableRowProps(() => setPeekParam({ queue: t.queue, id: t.id }))}
                  >
                    {!window.READ_ONLY && (
                      <TableCell className="w-8" onClick={(e) => e.stopPropagation()}>
                        <input
                          type="checkbox"
                          aria-label={`Select task ${uuidPrefix(t.id)}`}
                          checked={selected.has(rowKey(t))}
                          onChange={() => toggleSelect(i)}
                          className="accent-[var(--fc-acc)]"
                        />
                      </TableCell>
                    )}
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
                                <Button size="icon" variant="ghost" className="h-7 w-7 text-red-500" onClick={() => setConfirmDelete(t)}>
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

          <ConfirmDialog
            open={confirmDelete !== null}
            title="Delete Task"
            description={
              <>
                Delete task <strong>{confirmDelete ? uuidPrefix(confirmDelete.id) : ""}</strong> from
                queue <strong>{confirmDelete?.queue}</strong>? This action cannot be undone.
              </>
            }
            onConfirm={() => {
              if (confirmDelete && acts.delete) {
                runAction(acts.delete, confirmDelete.queue, confirmDelete.id);
              }
              setConfirmDelete(null);
            }}
            onClose={() => setConfirmDelete(null)}
          />

          {/* Pagination: cursor-based for exact zset listings, page/size
              otherwise. */}
          {isCursorMode ? (
            <div className="flex items-center justify-between px-4 py-3 border-t border-[hsl(var(--border))]">
              <span className="text-xs text-[hsl(var(--muted-foreground))]">
                {rows.length} rows · <span className="font-mono tabular-nums">{total.toLocaleString()}</span> total · exact
              </span>
              <div className="flex items-center gap-1">
                <Button
                  size="icon"
                  variant="ghost"
                  className="h-7 w-7"
                  disabled={cursorStack.length === 0}
                  onClick={() => setCursorStack((s) => s.slice(0, -1))}
                >
                  <ChevronLeft size={14} />
                </Button>
                <Button
                  size="icon"
                  variant="ghost"
                  className="h-7 w-7"
                  disabled={meta.cursor === ""}
                  onClick={() => setCursorStack((s) => [...s, meta.cursor])}
                >
                  <ChevronRight size={14} />
                </Button>
              </div>
            </div>
          ) : (
            total > 0 && (
              <div className="flex items-center justify-between px-4 py-3 border-t border-[hsl(var(--border))]">
                <div className="flex items-center gap-3">
                  <span className="text-xs text-[hsl(var(--muted-foreground))]">
                    {view.page * view.size + 1}–{Math.min((view.page + 1) * view.size, total)} of{" "}
                    {truncated ? `${total}+` : total}
                    {extraMatches > 0 && ` (+${extraMatches} from continued scan)`}
                  </span>
                  <select
                    value={view.size}
                    onChange={(e) => updateView({ size: Number(e.target.value), page: 0 })}
                    aria-label="Page size"
                    className="h-6 rounded-md border border-[hsl(var(--input))] bg-[hsl(var(--background))] px-1 text-xs"
                  >
                    {PAGE_SIZES.map((s) => (
                      <option key={s} value={s}>{s} / page</option>
                    ))}
                  </select>
                </div>
                <div className="flex items-center gap-1">
                  <Button size="icon" variant="ghost" className="h-7 w-7" disabled={view.page === 0} onClick={() => updateView({ page: view.page - 1 })}>
                    <ChevronLeft size={14} />
                  </Button>
                  <Button size="icon" variant="ghost" className="h-7 w-7" disabled={view.page >= totalPages - 1} onClick={() => updateView({ page: view.page + 1 })}>
                    <ChevronRight size={14} />
                  </Button>
                </div>
              </div>
            )
          )}
        </div>
      )}

      {/* Task peek drawer (deep-linkable via the `peek` param). Prev/next
          walk the FROZEN list (§3.5: cursor carried from the list — no
          reshuffle while the drawer is open). */}
      {peek && (
        <TaskDrawer
          peek={peek}
          resultList={rows}
          onClose={() => setPeekParam(null, { replace: true })}
          onPeek={(target) => setPeekParam(target, { replace: true })}
          onPivot={handlePivot}
        />
      )}

      {/* §4.4 "N updates paused — refresh" pill. */}
      <UpdatesPausedPill count={pausedUpdates} onRefresh={applyUpdates} />

      {/* §4.2 "Save view…" — persists the urlstate-owned console params. */}
      <SaveViewModal
        open={saveViewOpen}
        target="tasks"
        state={consoleViewState}
        onClose={() => setSaveViewOpen(false)}
      />
    </div>
  );
}
