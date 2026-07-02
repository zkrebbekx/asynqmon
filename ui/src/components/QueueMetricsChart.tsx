import { LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer } from "recharts";
import dayjs from "dayjs";
import { MetricsResponse, Metrics } from "../api";
import { Card, CardContent, CardHeader, CardTitle } from "./ui/card";

interface Props {
  metrics: MetricsResponse;
  queues: string[];
}

interface ChartData {
  timestamp: number;
  [qname: string]: number;
}

const COLORS = ["#1967d2", "#669df6", "#81c995", "#f28b82", "#e69138", "#fdd663", "#9aa0a6", "#d7aefb"];

function toChartData(metricsList: Metrics[]): ChartData[] {
  const byTimestamp: { [key: number]: ChartData } = {};
  for (const m of metricsList) {
    for (const [ts, val] of m.values) {
      if (!byTimestamp[ts]) byTimestamp[ts] = { timestamp: ts };
      const qname = m.metric.queue;
      if (qname) byTimestamp[ts][qname] = parseFloat(val);
    }
  }
  return Object.values(byTimestamp).sort((a, b) => a.timestamp - b.timestamp);
}

function MetricChart({ title, metrics, queues, formatter }: {
  title: string;
  metrics: Metrics[];
  queues: string[];
  formatter?: (v: number) => string;
}) {
  const data = toChartData(metrics);
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-semibold text-[hsl(var(--foreground))]">{title}</CardTitle>
      </CardHeader>
      <CardContent className="h-52">
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={data}>
            <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
            <XAxis
              dataKey="timestamp"
              tickFormatter={(ts) => dayjs.unix(ts).format("HH:mm")}
              stroke="hsl(var(--muted-foreground))"
              tick={{ fontSize: 10 }}
            />
            <YAxis stroke="hsl(var(--muted-foreground))" tick={{ fontSize: 10 }} tickFormatter={formatter} />
            <Tooltip
              labelFormatter={(ts) => dayjs.unix(ts as number).format("HH:mm:ss")}
              contentStyle={{ backgroundColor: "hsl(var(--popover))", border: "1px solid hsl(var(--border))", borderRadius: "6px", fontSize: "12px" }}
              labelStyle={{ color: "hsl(var(--popover-foreground))" }}
            />
            <Legend />
            {queues.map((q, i) => (
              <Line key={q} type="monotone" dataKey={q} stroke={COLORS[i % COLORS.length]} dot={false} strokeWidth={1.5} isAnimationActive={false} />
            ))}
          </LineChart>
        </ResponsiveContainer>
      </CardContent>
    </Card>
  );
}

function getResults(resp: { data?: { result: Metrics[] } }): Metrics[] {
  return resp?.data?.result ?? [];
}

export default function QueueMetricsChart({ metrics, queues }: Props) {
  return (
    <div className="grid grid-cols-2 gap-4">
      <MetricChart title="Queue Size" metrics={getResults(metrics.queue_size)} queues={queues} />
      <MetricChart title="Queue Latency (seconds)" metrics={getResults(metrics.queue_latency_seconds)} queues={queues} />
      <MetricChart title="Memory Usage" metrics={getResults(metrics.queue_memory_usage_approx_bytes)} queues={queues} formatter={(v) => `${(v / 1024 / 1024).toFixed(1)}MB`} />
      <MetricChart title="Tasks Processed/sec" metrics={getResults(metrics.tasks_processed_per_second)} queues={queues} />
      <MetricChart title="Tasks Failed/sec" metrics={getResults(metrics.tasks_failed_per_second)} queues={queues} />
      <MetricChart title="Error Rate" metrics={getResults(metrics.error_rate)} queues={queues} formatter={(v) => `${(v * 100).toFixed(1)}%`} />
      <MetricChart title="Pending Tasks" metrics={getResults(metrics.pending_tasks_by_queue)} queues={queues} />
      <MetricChart title="Retry Tasks" metrics={getResults(metrics.retry_tasks_by_queue)} queues={queues} />
    </div>
  );
}
