import { LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer } from "recharts";
import dayjs from "dayjs";
import { MetricsResponse, Metrics } from "../api";

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
    <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
      <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
        {title}
      </div>
      <div className="h-52 px-2 py-2">
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={data}>
            <CartesianGrid strokeDasharray="3 3" stroke="var(--fc-line2)" />
            <XAxis
              dataKey="timestamp"
              tickFormatter={(ts) => dayjs.unix(ts).format("HH:mm")}
              stroke="var(--fc-ink3)"
              tick={{ fontSize: 10 }}
            />
            <YAxis stroke="var(--fc-ink3)" tick={{ fontSize: 10 }} tickFormatter={formatter} />
            <Tooltip
              labelFormatter={(ts) => dayjs.unix(ts as number).format("HH:mm:ss")}
              contentStyle={{ backgroundColor: "var(--fc-panel)", border: "1px solid var(--fc-line)", borderRadius: "6px", fontSize: "12px" }}
              labelStyle={{ color: "var(--fc-ink)" }}
            />
            <Legend wrapperStyle={{ fontSize: "11px" }} />
            {queues.map((q, i) => (
              <Line key={q} type="monotone" dataKey={q} stroke={COLORS[i % COLORS.length]} dot={false} strokeWidth={1.5} isAnimationActive={false} />
            ))}
          </LineChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}

function getResults(resp: { data?: { result: Metrics[] } }): Metrics[] {
  return resp?.data?.result ?? [];
}

export default function QueueMetricsChart({ metrics, queues }: Props) {
  return (
    <div className="grid grid-cols-1 gap-2.5 lg:grid-cols-2">
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
