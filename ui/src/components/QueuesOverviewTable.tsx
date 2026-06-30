import { useState } from "react";
import { Link } from "react-router-dom";
import prettyBytes from "pretty-bytes";
import { Pause, Play, Trash2, MoreHorizontal, Search } from "lucide-react";
import { Queue } from "../api";
import { queueDetailsPath } from "../paths";
import { percentage } from "../utils";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import DeleteQueueConfirmationDialog from "./DeleteQueueConfirmationDialog";
import { cn } from "../lib/utils";
import { matchesQuery } from "../lib/filter";

interface QueueWithMetadata extends Queue {
  requestPending: boolean;
}

interface Props {
  queues: QueueWithMetadata[];
  onPauseClick: (qname: string) => Promise<void>;
  onResumeClick: (qname: string) => Promise<void>;
  onDeleteClick: (qname: string) => Promise<void>;
}

function QueueRow({ queue: q, onPauseClick, onResumeClick, onDeleteClick }: {
  queue: QueueWithMetadata;
  onPauseClick: () => void;
  onResumeClick: () => void;
  onDeleteClick: () => void;
}) {
  const [showActions, setShowActions] = useState(false);

  return (
    <TableRow
      onMouseEnter={() => setShowActions(true)}
      onMouseLeave={() => setShowActions(false)}
    >
      <TableCell className="font-medium sticky left-0 bg-[hsl(var(--card))]">
        <Link
          to={queueDetailsPath(q.queue)}
          className="text-[hsl(var(--foreground))] hover:underline"
        >
          {q.queue}
        </Link>
      </TableCell>
      <TableCell>
        <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", q.paused ? "text-amber-500" : "text-emerald-600")}>
          <span className={cn("h-1.5 w-1.5 rounded-full", q.paused ? "bg-amber-500" : "bg-emerald-500 animate-pulse")} />
          {q.paused ? "paused" : "running"}
        </span>
      </TableCell>
      <TableCell className="text-right">{q.size}</TableCell>
      <TableCell className="text-right">{prettyBytes(q.memory_usage_bytes)}</TableCell>
      <TableCell className="text-right">{q.display_latency}</TableCell>
      <TableCell className="text-right">{q.processed.toLocaleString()}</TableCell>
      <TableCell className="text-right text-red-500">{q.failed.toLocaleString()}</TableCell>
      <TableCell className="text-right">{percentage(q.failed, q.processed)}</TableCell>
      {!window.READ_ONLY && (
        <TableCell className="text-center w-28">
          {showActions ? (
            <TooltipProvider>
              <div className="flex items-center justify-center gap-1">
                {q.paused ? (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <Button size="icon" variant="ghost" className="h-7 w-7" onClick={onResumeClick} disabled={q.requestPending}>
                        <Play size={14} />
                      </Button>
                    </TooltipTrigger>
                    <TooltipContent>Resume</TooltipContent>
                  </Tooltip>
                ) : (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <Button size="icon" variant="ghost" className="h-7 w-7" onClick={onPauseClick} disabled={q.requestPending}>
                        <Pause size={14} />
                      </Button>
                    </TooltipTrigger>
                    <TooltipContent>Pause</TooltipContent>
                  </Tooltip>
                )}
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Button size="icon" variant="ghost" className="h-7 w-7 text-red-500 hover:text-red-600" onClick={onDeleteClick}>
                      <Trash2 size={14} />
                    </Button>
                  </TooltipTrigger>
                  <TooltipContent>Delete</TooltipContent>
                </Tooltip>
              </div>
            </TooltipProvider>
          ) : (
            <Button size="icon" variant="ghost" className="h-7 w-7 opacity-30">
              <MoreHorizontal size={14} />
            </Button>
          )}
        </TableCell>
      )}
    </TableRow>
  );
}

export default function QueuesOverviewTable({ queues, onPauseClick, onResumeClick, onDeleteClick }: Props) {
  const [queueToDelete, setQueueToDelete] = useState<Queue | null>(null);
  const [filter, setFilter] = useState("");

  const filtered = queues.filter((q) => matchesQuery(q.queue, filter));

  const header = (
    <div className="flex items-center justify-between px-4 py-3 border-b border-[hsl(var(--border))]">
      <h2 className="text-base font-semibold text-[hsl(var(--foreground))]">
        Queues
        <span className="ml-2 text-xs font-normal text-[hsl(var(--muted-foreground))]">
          {filter.trim() ? `${filtered.length} of ${queues.length}` : queues.length}
        </span>
      </h2>
      {queues.length > 0 && (
        <div className="relative">
          <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[hsl(var(--muted-foreground))]" />
          <Input
            placeholder="Filter queues"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="pl-7 h-8 w-48 text-xs"
          />
        </div>
      )}
    </div>
  );

  if (queues.length === 0) {
    return (
      <>
        {header}
        <div className="px-6 py-8 text-center text-sm text-[hsl(var(--muted-foreground))]">
          No queues found
        </div>
      </>
    );
  }

  if (filtered.length === 0) {
    return (
      <>
        {header}
        <div className="px-6 py-8 text-center text-sm text-[hsl(var(--muted-foreground))]">
          No queues match "{filter}"
        </div>
      </>
    );
  }

  return (
    <>
      {header}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="sticky left-0 bg-[hsl(var(--card))]">Queue</TableHead>
            <TableHead>State</TableHead>
            <TableHead className="text-right">Size</TableHead>
            <TableHead className="text-right">Memory</TableHead>
            <TableHead className="text-right">Latency</TableHead>
            <TableHead className="text-right">Processed</TableHead>
            <TableHead className="text-right">Failed</TableHead>
            <TableHead className="text-right">Error rate</TableHead>
            {!window.READ_ONLY && <TableHead className="text-center">Actions</TableHead>}
          </TableRow>
        </TableHeader>
        <TableBody>
          {filtered.map((q) => (
            <QueueRow
              key={q.queue}
              queue={q}
              onPauseClick={() => onPauseClick(q.queue)}
              onResumeClick={() => onResumeClick(q.queue)}
              onDeleteClick={() => setQueueToDelete(q)}
            />
          ))}
        </TableBody>
      </Table>
      <DeleteQueueConfirmationDialog
        queue={queueToDelete}
        onClose={() => setQueueToDelete(null)}
      />
    </>
  );
}
