import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";

interface QueueLocation {
  queue: string;
  keyslot: number;
  nodes: string[];
}

interface Props {
  queueLocations: QueueLocation[];
}

export default function QueueLocationTable({ queueLocations }: Props) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Queue</TableHead>
          <TableHead>Key Slot</TableHead>
          <TableHead>Nodes</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {queueLocations.map((loc) => (
          <TableRow key={loc.queue}>
            <TableCell className="font-medium">{loc.queue}</TableCell>
            <TableCell>{loc.keyslot}</TableCell>
            <TableCell className="font-mono text-xs">{loc.nodes?.join(", ")}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
