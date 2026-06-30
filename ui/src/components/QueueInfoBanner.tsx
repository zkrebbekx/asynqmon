import { useSelector } from "react-redux";
import prettyBytes from "pretty-bytes";
import { AppState } from "../store";
import { percentage } from "../utils";

interface Props {
  qname: string;
}

function BannerItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex-1 border-l border-[hsl(var(--border))] first:border-l-0 px-4">
      <p className="text-xs font-medium text-[hsl(var(--foreground))] mb-0.5">{label}</p>
      <p className="text-sm text-[hsl(var(--muted-foreground))]">{value}</p>
    </div>
  );
}

export default function QueueInfoBanner({ qname }: Props) {
  const queueInfo = useSelector((s: AppState) =>
    s.queues.data.find((q) => q.name === qname)
  );
  const queue = queueInfo?.currentStats;

  return (
    <div className="flex py-3 px-2">
      <BannerItem label="Queue name" value={qname} />
      <BannerItem label="Queue state" value={queue ? (queue.paused ? "paused" : "running") : "-"} />
      <BannerItem label="Queue size" value={queue ? String(queue.size) : "-"} />
      <BannerItem label="Task groups" value={queue ? String(queue.groups) : "-"} />
      <BannerItem
        label="Memory usage"
        value={queue ? prettyBytes(queue.memory_usage_bytes) : "-"}
      />
      <BannerItem
        label="Error rate"
        value={queue ? percentage(queue.failed, queue.processed) : "-"}
      />
    </div>
  );
}
