import { SchedulerEntry } from "../api";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Badge } from "./ui/badge";
import { timeAgo } from "../utils";
import SyntaxHighlighter from "./SyntaxHighlighter";

interface Props {
  entries: SchedulerEntry[];
}

export default function SchedulerEntriesTable({ entries }: Props) {
  if (entries.length === 0) {
    return (
      <div className="px-6 py-8 text-center text-sm text-[hsl(var(--muted-foreground))]">
        No scheduler entries found
      </div>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Task Type</TableHead>
          <TableHead>Cron Spec</TableHead>
          <TableHead>Options</TableHead>
          <TableHead>Next Enqueue</TableHead>
          <TableHead>Prev Enqueue</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {entries.map((entry) => (
          <TableRow key={entry.id}>
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
              {timeAgo(entry.next_enqueue_at)}
            </TableCell>
            <TableCell className="text-xs text-[hsl(var(--muted-foreground))]">
              {entry.prev_enqueue_at ? timeAgo(entry.prev_enqueue_at) : "–"}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
