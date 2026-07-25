import { useEffect, useMemo, useState } from "react";
import { useSelector } from "react-redux";
import { Link, useNavigate } from "react-router-dom";
import { X, Server } from "lucide-react";
import { AppState, useAppDispatch } from "../store";
import {
  listActiveTasksAsync, cancelActiveTaskAsync,
  batchCancelActiveTasksAsync, cancelAllActiveTasksAsync,
} from "../actions/tasksActions";
import { taskRowsPerPageChange } from "../actions/settingsActions";
import { paths, taskDetailsPath } from "../paths";
import { prettifyPayload, uuidPrefix, formatTimestamp } from "../utils";
import { activeTaskBudget, sortByBudgetPct } from "../lib/workspace";
import { budgetTone } from "../lib/workers";
import { humanizeMs } from "../lib/fleet";
import TasksTable, { RowProps } from "./TasksTable";
import { TableCell, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { FcChip } from "./FleetBits";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import SyntaxHighlighter from "./SyntaxHighlighter";
import { cn, clickableRowClass, clickableRowProps } from "../lib/utils";

interface Props {
  queue: string;
  totalTaskCount: number;
  // Routes whole-scope verbs into the §4.3 bulk-job flow (Queue Workspace).
  onWholeScopeVerb?: (verb: "run" | "archive" | "delete" | "cancel") => void;
}

const columns = [
  { key: "id", label: "ID", align: "left" as const },
  { key: "type", label: "Type", align: "left" as const },
  { key: "payload", label: "Payload", align: "left" as const },
  { key: "status", label: "Status", align: "left" as const },
  { key: "budget", label: "Budget · elapsed/deadline", align: "left" as const },
  { key: "running-for", label: "Running", align: "right" as const },
  ...(!window.READ_ONLY ? [{ key: "actions", label: "Actions", align: "center" as const }] : []),
];

// BudgetBar renders elapsed-vs-timeout as a bar + % (§3.3 active rows). The
// BAR clamps at 100%, the number does not; ≥90% is crit, ≥75% warn. Tasks
// without a computable budget render an honest "—", never a fake full bar.
function BudgetBar({ pct }: { pct: number | null }) {
  if (pct === null) {
    return <span className="text-xs text-[var(--fc-ink3)]">no deadline</span>;
  }
  const tone = budgetTone(pct);
  const color =
    tone === "crit" ? "bg-[var(--fc-crit)]" : tone === "warn" ? "bg-[var(--fc-warn)]" : "bg-[var(--fc-acc)]";
  return (
    <span className="inline-flex items-center gap-2">
      <span className="h-1.5 w-24 overflow-hidden rounded bg-[var(--fc-raise)]">
        <span
          className={cn("block h-full rounded", color)}
          style={{ width: `${Math.min(100, Math.round(pct * 100))}%` }}
        />
      </span>
      <span
        className={cn(
          "font-mono text-[11px] tabular-nums",
          tone === "crit" && "font-semibold text-[var(--fc-crit)]",
          tone === "warn" && "text-[var(--fc-warn)]"
        )}
      >
        {Math.round(pct * 100)}%
      </span>
    </span>
  );
}

function Row({ task, isSelected, onSelectChange }: RowProps) {
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const budget = activeTaskBudget(task, Date.now());
  return (
    <TableRow
      className={cn(
        clickableRowClass,
        isSelected && "bg-[hsl(var(--muted))]/60",
        task.is_orphaned && "opacity-60"
      )}
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
      <TableCell onClick={(e) => e.stopPropagation()}>
        <span className="inline-flex items-center gap-1.5">
          {task.is_orphaned ? (
            <FcChip
              tone="warn"
              title="lease expired beyond grace — no live worker holds this task; asynq's recoverer will requeue it"
            >
              orphaned
            </FcChip>
          ) : (
            <FcChip tone="info">active</FcChip>
          )}
          {!task.is_orphaned && (
            <Link
              to={paths().SERVERS}
              title="Open Workers — the stuck-worker view ranks this task's worker by % of deadline"
              className="text-[var(--fc-ink3)] hover:text-[var(--fc-acc)]"
            >
              <Server size={12} />
            </Link>
          )}
        </span>
      </TableCell>
      <TableCell>
        {task.is_orphaned ? (
          <span className="text-xs text-[var(--fc-ink3)]">no live worker</span>
        ) : (
          <BudgetBar pct={budget.pct} />
        )}
      </TableCell>
      <TableCell
        className="text-right font-mono text-xs tabular-nums text-[hsl(var(--muted-foreground))]"
        title={formatTimestamp(task.start_time)}
      >
        {budget.elapsedMs !== null ? humanizeMs(budget.elapsedMs) : "—"}
        {budget.budgetMs !== null && (
          <span className="text-[var(--fc-ink3)]"> / {humanizeMs(budget.budgetMs)}</span>
        )}
      </TableCell>
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

export default function ActiveTasksTable({ queue, totalTaskCount, onWholeScopeVerb }: Props) {
  const dispatch = useAppDispatch();
  const { loading, error, data: tasks, batchActionPending, allActionPending } = useSelector(
    (s: AppState) => s.tasks.activeTasks
  );
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const pageSize = useSelector((s: AppState) => s.settings.taskRowsPerPage);

  // §3.3: sortable by %-of-budget so the wedged worker is row one. Default
  // ON; the toggle restores arrival order. Re-ranked once per poll (nowMs is
  // frozen between fetches so rows don't reshuffle under the cursor).
  const [sortByBudget, setSortByBudget] = useState(true);
  const [rankedAt, setRankedAt] = useState(() => Date.now());
  useEffect(() => {
    setRankedAt(Date.now());
  }, [tasks]);
  const visibleTasks = useMemo(
    () => (sortByBudget ? sortByBudgetPct(tasks, rankedAt) : tasks),
    [tasks, sortByBudget, rankedAt]
  );

  return (
    <div>
      <div className="flex items-center justify-between border-b border-[hsl(var(--border))] px-4 py-1.5">
        <label className="flex cursor-pointer items-center gap-2 text-xs text-[hsl(var(--muted-foreground))]">
          <input
            type="checkbox"
            checked={sortByBudget}
            onChange={(e) => setSortByBudget(e.target.checked)}
            className="h-3.5 w-3.5 accent-[hsl(var(--primary))]"
          />
          Sort by % of deadline — wedged workers first
        </label>
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          budget = elapsed vs worker deadline (timeout fallback)
        </span>
      </div>
      <TasksTable
        queue={queue}
        totalTaskCount={totalTaskCount}
        taskState="active"
        loading={loading}
        error={error}
        tasks={visibleTasks}
        batchActionPending={batchActionPending}
        allActionPending={allActionPending}
        pollInterval={pollInterval}
        pageSize={pageSize}
        columns={columns}
        listTasks={(q, pgn) => dispatch(listActiveTasksAsync(q, pgn))}
        batchCancelTasks={(q, ids) => dispatch(batchCancelActiveTasksAsync(q, ids))}
        cancelAllTasks={(q) => dispatch(cancelAllActiveTasksAsync(q))}
        onWholeScopeVerb={onWholeScopeVerb}
        taskRowsPerPageChange={(n) => dispatch(taskRowsPerPageChange(n))}
        renderRow={(rp) => <Row key={rp.task.id} {...rp} />}
      />
    </div>
  );
}
