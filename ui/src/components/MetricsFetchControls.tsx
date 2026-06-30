import { useNavigate } from "react-router-dom";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "./ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./ui/select";
import { currentUnixtime } from "../utils";

interface Props {
  endTime: number;
  duration: number;
}

const durationOptions = [
  { label: "30 min", value: 30 * 60 },
  { label: "1 hour", value: 60 * 60 },
  { label: "3 hours", value: 3 * 60 * 60 },
  { label: "6 hours", value: 6 * 60 * 60 },
  { label: "12 hours", value: 12 * 60 * 60 },
  { label: "24 hours", value: 24 * 60 * 60 },
];

export default function MetricsFetchControls({ endTime, duration }: Props) {
  const navigate = useNavigate();

  const updateParams = (newEndTime: number, newDuration: number) => {
    const params = new URLSearchParams();
    params.set("end_time", String(newEndTime));
    params.set("duration", String(newDuration));
    navigate(`?${params.toString()}`, { replace: true });
  };

  return (
    <div className="flex items-center gap-2">
      <Button
        size="icon"
        variant="outline"
        className="h-8 w-8"
        onClick={() => updateParams(endTime - duration, duration)}
      >
        <ChevronLeft size={14} />
      </Button>
      <Select
        value={String(duration)}
        onValueChange={(v) => updateParams(endTime, Number(v))}
      >
        <SelectTrigger className="h-8 w-28 text-xs">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {durationOptions.map((opt) => (
            <SelectItem key={opt.value} value={String(opt.value)}>{opt.label}</SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Button
        size="icon"
        variant="outline"
        className="h-8 w-8"
        disabled={endTime >= currentUnixtime()}
        onClick={() => updateParams(Math.min(endTime + duration, currentUnixtime()), duration)}
      >
        <ChevronRight size={14} />
      </Button>
    </div>
  );
}
