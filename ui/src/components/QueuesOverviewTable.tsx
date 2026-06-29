import { useState } from "react";
import { Link } from "react-router-dom";
import prettyBytes from "pretty-bytes";
import { Pause, Play, Trash2, MoreHorizontal } from "lucide-react";
import { Queue } from "../api";
import { queueDetailsPath } from "../paths";
import { percentage } from "../utils";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Button } from "./ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import DeleteQueueConfirmationDialog from "./DeleteQueueConfirmationDialog";
import { cn } from "../lib/utils";

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
        <span className={cn("text-xs font-medium", q.paused ? "text-red-500" : "text-emerald-600")}>
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

  if (queues.length === 0) {
    return (
      <div className="px-6 py-8 text-center text-sm text-[hsl(var(--muted-foreground))]">
        No queues found
      </div>
    );
  }

  return (
    <>
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
          {queues.map((q) => (
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
