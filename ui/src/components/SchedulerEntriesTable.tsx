import React, { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight, ChevronDown, Loader2 } from "lucide-react";
import { SchedulerEntry, SchedulerEnqueueEvent, listSchedulerEnqueueEvents } from "../api";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Badge } from "./ui/badge";
import { timeAgo, durationBefore, uuidPrefix } from "../utils";
import { taskDetailsPath } from "../paths";
import SyntaxHighlighter from "./SyntaxHighlighter";

interface Props {
  entries: SchedulerEntry[];
}

// asynq renders options like Queue("critical"), MaxRetry(5). Pull the queue out
// so enqueue-history task ids can deep-link to the task detail view.
function queueFromOptions(options: string[] | undefined): string | null {
  for (const o of options ?? []) {
    const m = o.match(/Queue\("?([^")]+)"?\)/);
    if (m) return m[1];
  }
  return null;
}

function EnqueueHistory({ entryId, queue }: { entryId: string; queue: string | null }) {
  const navigate = useNavigate();
  const [events, setEvents] = useState<SchedulerEnqueueEvent[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);

  useEffect(() => {
    let active = true;
    listSchedulerEnqueueEvents(entryId)
      .then((r) => active && setEvents(r.events ?? []))
      .catch(() => active && setError(true))
      .finally(() => active && setLoading(false));
    return () => {
      active = false;
    };
  }, [entryId]);

  if (loading) {
    return (
      <div className="flex items-center gap-2 py-2 text-xs text-[hsl(var(--muted-foreground))]">
        <Loader2 size={13} className="animate-spin" /> Loading enqueue history…
      </div>
    );
  }
  if (error) {
    return <div className="py-2 text-xs text-red-500">Could not load enqueue history.</div>;
  }
  if (!events || events.length === 0) {
    return <div className="py-2 text-xs text-[hsl(var(--muted-foreground))]">No enqueue history yet.</div>;
  }

  return (
    <div className="rounded-md border border-[hsl(var(--border))] bg-[hsl(var(--background))]">
      <div className="px-3 py-1.5 text-xs font-medium text-[hsl(var(--muted-foreground))] border-b border-[hsl(var(--border))]">
        Recent enqueues ({events.length})
      </div>
      <div className="divide-y divide-[hsl(var(--border))]">
        {events.map((ev) => (
          <div key={ev.task_id} className="flex items-center justify-between gap-4 px-3 py-1.5 text-xs">
            {queue ? (
              <button
                onClick={() => navigate(taskDetailsPath(queue, ev.task_id))}
                className="font-mono text-[hsl(var(--primary))] hover:underline"
              >
                {uuidPrefix(ev.task_id)}
              </button>
            ) : (
              <span className="font-mono">{uuidPrefix(ev.task_id)}</span>
            )}
            <span className="text-[hsl(var(--muted-foreground))]">{timeAgo(ev.enqueued_at)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

export default function SchedulerEntriesTable({ entries }: Props) {
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  if (entries.length === 0) {
    return (
      <div className="px-6 py-8 text-center text-sm text-[hsl(var(--muted-foreground))]">
        No scheduler entries found
      </div>
    );
  }

  const toggle = (id: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead className="w-8" />
          <TableHead>Task Type</TableHead>
          <TableHead>Cron Spec</TableHead>
          <TableHead>Options</TableHead>
          <TableHead>Next Enqueue</TableHead>
          <TableHead>Prev Enqueue</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {entries.map((entry) => {
          const isOpen = expanded.has(entry.id);
          const queue = queueFromOptions(entry.options);
          return (
            <React.Fragment key={entry.id}>
              <TableRow className="cursor-pointer" onClick={() => toggle(entry.id)}>
                <TableCell className="w-8 pr-0 text-[hsl(var(--muted-foreground))]">
                  {isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                </TableCell>
                <TableCell>
                  <div>
                    <span className="font-mono text-xs font-medium text-[hsl(var(--foreground))]">
                      {entry.task_type}
                    </span>
                    <div className="mt-1">
                      <SyntaxHighlighter>{entry.task_payload || "{}"}</SyntaxHighlighter>
                    </div>
                  </div>
                </TableCell>
                <TableCell>
                  <Badge variant="secondary" className="font-mono text-xs">{entry.spec}</Badge>
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1">
                    {entry.options?.map((opt) => (
                      <Badge key={opt} variant="outline" className="text-xs">{opt}</Badge>
                    ))}
                  </div>
                </TableCell>
                <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">
                  {durationBefore(entry.next_enqueue_at)}
                </TableCell>
                <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">
                  {entry.prev_enqueue_at ? timeAgo(entry.prev_enqueue_at) : "–"}
                </TableCell>
              </TableRow>
              {isOpen && (
                <TableRow className="hover:bg-transparent">
                  <TableCell />
                  <TableCell colSpan={5} className="py-2">
                    <EnqueueHistory entryId={entry.id} queue={queue} />
                  </TableCell>
                </TableRow>
              )}
            </React.Fragment>
          );
        })}
      </TableBody>
    </Table>
  );
}
