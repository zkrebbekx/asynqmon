import { LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer } from "recharts";
import { DailyStat } from "../api";

interface Props {
  data: { [qname: string]: DailyStat[] };
  numDays: number;
}

interface ChartData {
  succeeded: number;
  failed: number;
  date: string;
}

function makeChartData(queueStats: { [qname: string]: DailyStat[] }, numDays: number): ChartData[] {
  const byDate: { [date: string]: ChartData } = {};
  for (const qname in queueStats) {
    for (const stat of queueStats[qname]) {
      if (!byDate[stat.date]) {
        byDate[stat.date] = { succeeded: 0, failed: 0, date: stat.date };
      }
      byDate[stat.date].succeeded += stat.processed - stat.failed;
      byDate[stat.date].failed += stat.failed;
    }
  }
  return Object.values(byDate)
    .sort((a, b) => Date.parse(a.date) - Date.parse(b.date))
    .slice(-numDays);
}

export default function DailyStatsChart({ data, numDays }: Props) {
  const chartData = makeChartData(data, numDays);
  return (
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={chartData}>
        <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
        <XAxis dataKey="date" minTickGap={10} stroke="hsl(var(--muted-foreground))" tick={{ fontSize: 11 }} />
        <YAxis stroke="hsl(var(--muted-foreground))" tick={{ fontSize: 11 }} />
        <Tooltip contentStyle={{ backgroundColor: "hsl(var(--popover))", border: "1px solid hsl(var(--border))", borderRadius: "6px" }} />
        <Legend />
        <Line type="monotone" dataKey="succeeded" stroke="#81c995" strokeWidth={2} dot={false} />
        <Line type="monotone" dataKey="failed" stroke="#f28b82" strokeWidth={2} dot={false} />
      </LineChart>
    </ResponsiveContainer>
  );
}
