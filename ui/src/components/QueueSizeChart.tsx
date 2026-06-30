import { BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer } from "recharts";
import { useNavigate } from "react-router-dom";
import { queueDetailsPath } from "../paths";

interface TaskBreakdown {
  queue: string;
  active: number;
  pending: number;
  aggregating: number;
  scheduled: number;
  retry: number;
  archived: number;
  completed: number;
}

interface Props {
  data: TaskBreakdown[];
}

export default function QueueSizeChart({ data }: Props) {
  const navigate = useNavigate();

  const handleClick = (params: any) => {
    if (params?.activeLabel && data.some((d: TaskBreakdown) => d.queue === params.activeLabel)) {
      navigate(queueDetailsPath(params.activeLabel));
    }
  };

  return (
    <ResponsiveContainer width="100%" height="100%">
      <BarChart data={data} maxBarSize={120} onClick={handleClick} style={{ cursor: "pointer" }}>
        <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
        <XAxis dataKey="queue" stroke="hsl(var(--muted-foreground))" tick={{ fontSize: 12 }} />
        <YAxis stroke="hsl(var(--muted-foreground))" tick={{ fontSize: 12 }} />
        <Tooltip contentStyle={{ backgroundColor: "hsl(var(--popover))", border: "1px solid hsl(var(--border))", borderRadius: "6px" }} />
        <Legend />
        <Bar dataKey="active" stackId="a" fill="#1967d2" isAnimationActive={false} />
        <Bar dataKey="pending" stackId="a" fill="#669df6" isAnimationActive={false} />
        <Bar dataKey="aggregating" stackId="a" fill="#e69138" isAnimationActive={false} />
        <Bar dataKey="scheduled" stackId="a" fill="#fdd663" isAnimationActive={false} />
        <Bar dataKey="retry" stackId="a" fill="#f28b82" isAnimationActive={false} />
        <Bar dataKey="archived" stackId="a" fill="#9aa0a6" isAnimationActive={false} />
        <Bar dataKey="completed" stackId="a" fill="#81c995" isAnimationActive={false} />
      </BarChart>
    </ResponsiveContainer>
  );
}
