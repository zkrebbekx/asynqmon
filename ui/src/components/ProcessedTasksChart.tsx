import { BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer } from "recharts";

interface ProcessedStats {
  queue: string;
  succeeded: number;
  failed: number;
}

interface Props {
  data: ProcessedStats[];
}

export default function ProcessedTasksChart({ data }: Props) {
  return (
    <ResponsiveContainer width="100%" height="100%">
      <BarChart data={data} maxBarSize={120}>
        <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
        <XAxis dataKey="queue" stroke="hsl(var(--muted-foreground))" tick={{ fontSize: 12 }} />
        <YAxis stroke="hsl(var(--muted-foreground))" tick={{ fontSize: 12 }} />
        <Tooltip contentStyle={{ backgroundColor: "hsl(var(--popover))", border: "1px solid hsl(var(--border))", borderRadius: "6px" }} />
        <Legend />
        <Bar dataKey="succeeded" stackId="a" fill="#81c995" />
        <Bar dataKey="failed" stackId="a" fill="#f28b82" />
      </BarChart>
    </ResponsiveContainer>
  );
}
