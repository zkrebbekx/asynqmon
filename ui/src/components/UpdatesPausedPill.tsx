// The §4.4 freeze-on-interaction pill: floats over the frozen table while
// updates are suspended ("N updates paused — refresh"); clicking applies the
// buffered data. Rendered only when there is something to apply.

import { RefreshCw } from "lucide-react";

interface Props {
  count: number;
  onRefresh: () => void;
}

export default function UpdatesPausedPill({ count, onRefresh }: Props) {
  if (count <= 0) return null;
  return (
    <div className="pointer-events-none fixed bottom-6 left-1/2 z-40 -translate-x-1/2" aria-live="polite">
      <button
        onClick={onRefresh}
        className="pointer-events-auto flex items-center gap-2 rounded-full border border-[var(--fc-line)] bg-[var(--fc-panel)] px-3.5 py-1.5 text-xs font-medium text-[var(--fc-ink)] shadow-lg hover:border-[var(--fc-acc)]"
      >
        <RefreshCw size={12} aria-hidden className="text-[var(--fc-acc)]" />
        <span className="tabular-nums">
          {count} update{count === 1 ? "" : "s"} paused
        </span>
        <span className="text-[var(--fc-acc)]">— refresh</span>
      </button>
    </div>
  );
}
