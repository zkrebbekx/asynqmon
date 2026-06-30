import { useState } from "react";
import { Link } from "react-router-dom";
import { ChevronDown, ChevronUp } from "lucide-react";
import { ServerInfo } from "../api";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { timeAgo, prettifyPayload } from "../utils";
import { queueDetailsPath } from "../paths";
import SyntaxHighlighter from "./SyntaxHighlighter";

interface Props {
  servers: ServerInfo[];
}

function serverStatusVariant(status: string): "success" | "warning" | "secondary" {
  if (status === "active") return "success";
  if (status === "idle") return "secondary";
  return "warning";
}

function ServerRow({ server }: { server: ServerInfo }) {
  const [expanded, setExpanded] = useState(false);

  const queues = Object.entries(server.queue_priorities)
    .sort((a, b) => b[1] - a[1])
    .map(([q, p]) => `${q}:${p}`)
    .join(", ");

  return (
    <>
      <TableRow>
        <TableCell>
          <Button
            variant="ghost"
            size="icon"
            className="h-6 w-6"
            onClick={() => setExpanded(!expanded)}
          >
            {expanded ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
          </Button>
        </TableCell>
        <TableCell className="font-mono text-xs">
          {server.host}:{server.pid}
        </TableCell>
        <TableCell>
          <Badge variant={serverStatusVariant(server.status)}>{server.status}</Badge>
        </TableCell>
        <TableCell className="text-right">{server.active_workers.length}/{server.concurrency}</TableCell>
        <TableCell>{queues}</TableCell>
        <TableCell className="text-[hsl(var(--muted-foreground))] text-xs">{timeAgo(server.start_time)}</TableCell>
      </TableRow>
      {expanded && server.active_workers.length > 0 && (
        <TableRow>
          <TableCell colSpan={6} className="bg-[hsl(var(--muted))]/30 p-4">
            <p className="text-xs font-medium text-[hsl(var(--muted-foreground))] mb-2">Active Workers</p>
            <div className="space-y-2">
              {server.active_workers.map((w) => (
                <div key={w.task_id} className="text-xs border border-[hsl(var(--border))] rounded p-2">
                  <div className="flex gap-4 mb-1">
                    <span className="font-mono text-[hsl(var(--primary))]">{w.task_type}</span>
                    <Link to={queueDetailsPath(w.queue)} className="text-[hsl(var(--muted-foreground))] hover:underline">
                      {w.queue}
                    </Link>
                    <span className="text-[hsl(var(--muted-foreground))]">started {timeAgo(w.start_time)}</span>
                  </div>
                  <SyntaxHighlighter>{prettifyPayload(w.task_payload)}</SyntaxHighlighter>
                </div>
              ))}
            </div>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

export default function ServersTable({ servers }: Props) {
  if (servers.length === 0) {
    return (
      <div className="px-6 py-8 text-center text-sm text-[hsl(var(--muted-foreground))]">
        No servers connected
      </div>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead className="w-10" />
          <TableHead>Host:PID</TableHead>
          <TableHead>Status</TableHead>
          <TableHead className="text-right">Workers</TableHead>
          <TableHead>Queues</TableHead>
          <TableHead>Started</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {servers.map((s) => (
          <ServerRow key={s.id} server={s} />
        ))}
      </TableBody>
    </Table>
  );
}
