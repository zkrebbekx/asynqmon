import { useState, KeyboardEvent } from "react";
import { useSelector } from "react-redux";
import { useNavigate } from "react-router-dom";
import { Search } from "lucide-react";
import { AppState } from "../store";
import { queueDetailsPath, taskDetailsPath } from "../paths";
import { Input } from "./ui/input";
import { cn } from "../lib/utils";
import ActiveTasksTable from "./ActiveTasksTable";
import PendingTasksTable from "./PendingTasksTable";
import ScheduledTasksTable from "./ScheduledTasksTable";
import RetryTasksTable from "./RetryTasksTable";
import ArchivedTasksTable from "./ArchivedTasksTable";
import CompletedTasksTable from "./CompletedTasksTable";
import AggregatingTasksTableContainer from "./AggregatingTasksTableContainer";

interface Props {
  queue: string;
  selected: string;
}

interface TabChip {
  key: string;
  label: string;
  count: number;
}

export default function TasksTableContainer({ queue, selected }: Props) {
  const navigate = useNavigate();
  const [searchQuery, setSearchQuery] = useState("");

  const queueInfo = useSelector((s: AppState) =>
    s.queues.data.find((q) => q.name === queue)
  );
  const stats = queueInfo?.currentStats ?? {
    queue,
    paused: false,
    size: 0,
    groups: 0,
    active: 0,
    pending: 0,
    aggregating: 0,
    scheduled: 0,
    retry: 0,
    archived: 0,
    completed: 0,
    processed: 0,
    failed: 0,
    timestamp: "",
    latency_msec: 0,
    display_latency: "",
    memory_usage_bytes: 0,
  };

  const chips: TabChip[] = [
    { key: "active", label: "Active", count: stats.active },
    { key: "pending", label: "Pending", count: stats.pending },
    { key: "aggregating", label: "Aggregating", count: stats.aggregating },
    { key: "scheduled", label: "Scheduled", count: stats.scheduled },
    { key: "retry", label: "Retry", count: stats.retry },
    { key: "archived", label: "Archived", count: stats.archived },
    { key: "completed", label: "Completed", count: stats.completed },
  ];

  const handleSearchKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" && searchQuery.trim()) {
      navigate(taskDetailsPath(queue, searchQuery.trim()));
    }
  };

  return (
    <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))]">
      {/* Header */}
      <div className="flex items-center justify-between px-4 py-3 border-b border-[hsl(var(--border))]">
        <h2 className="text-base font-semibold text-[hsl(var(--foreground))]">Tasks</h2>
        <div className="flex items-center gap-3">
          {/* Tab chips */}
          <div className="flex items-center gap-1 flex-wrap">
            {chips.map((c) => (
              <button
                key={c.key}
                onClick={() => navigate(queueDetailsPath(queue, c.key))}
                className={cn(
                  "inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-xs font-medium border transition-colors",
                  selected === c.key
                    ? "bg-[hsl(var(--primary))] text-[hsl(var(--primary-foreground))] border-[hsl(var(--primary))]"
                    : "border-[hsl(var(--border))] text-[hsl(var(--muted-foreground))] hover:border-[hsl(var(--primary))] hover:text-[hsl(var(--foreground))]"
                )}
              >
                {c.label}
                <span className={cn(
                  "text-xs",
                  selected === c.key ? "opacity-80" : "text-[hsl(var(--muted-foreground))]"
                )}>
                  {c.count}
                </span>
              </button>
            ))}
          </div>
          {/* Search */}
          <div className="relative">
            <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[hsl(var(--muted-foreground))]" />
            <Input
              placeholder="Search by ID"
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              onKeyDown={handleSearchKeyDown}
              className="pl-7 h-8 w-52 text-xs"
            />
          </div>
        </div>
      </div>

      {/* Tab content. Keyed by queue so page, filter, and selection state
          reset when switching queues instead of leaking across them. */}
      {selected === "active" && <ActiveTasksTable key={queue} queue={queue} totalTaskCount={stats.active} />}
      {selected === "pending" && <PendingTasksTable key={queue} queue={queue} totalTaskCount={stats.pending} />}
      {selected === "aggregating" && <AggregatingTasksTableContainer key={queue} queue={queue} />}
      {selected === "scheduled" && <ScheduledTasksTable key={queue} queue={queue} totalTaskCount={stats.scheduled} />}
      {selected === "retry" && <RetryTasksTable key={queue} queue={queue} totalTaskCount={stats.retry} />}
      {selected === "archived" && <ArchivedTasksTable key={queue} queue={queue} totalTaskCount={stats.archived} />}
      {selected === "completed" && <CompletedTasksTable key={queue} queue={queue} totalTaskCount={stats.completed} />}
    </div>
  );
}
