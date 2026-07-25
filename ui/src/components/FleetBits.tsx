// Tiny shared presentational bits for the Fleet Console surfaces (FleetView,
// QueuesDirectoryView): status chips and the letterspaced-caps micro-label.
// All styling is --fc-* token based; tone is always paired with a form cue
// (chip text / stripe / dot), never color alone.

import { ReactNode } from "react";
import { cn } from "../lib/utils";

export type FcTone = "crit" | "warn" | "good" | "info" | "mut";

const CHIP_TONE: Record<FcTone, string> = {
  crit: "bg-[var(--fc-crit-bg)] text-[var(--fc-crit)]",
  warn: "bg-[var(--fc-warn-bg)] text-[var(--fc-warn)]",
  good: "bg-[var(--fc-good-bg)] text-[var(--fc-good)]",
  info: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  mut: "bg-[var(--fc-raise)] text-[var(--fc-ink2)]",
};

export function FcChip({
  tone,
  children,
  className,
  title,
}: {
  tone: FcTone;
  children: ReactNode;
  className?: string;
  title?: string;
}) {
  return (
    <span
      title={title}
      className={cn(
        "inline-flex items-center gap-1 whitespace-nowrap rounded px-[7px] py-[2px] text-[10.5px] font-semibold uppercase tracking-[0.04em]",
        CHIP_TONE[tone],
        className
      )}
    >
      {children}
    </span>
  );
}

export function MicroLabel({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        "text-[10.5px] uppercase tracking-[0.08em] text-[var(--fc-ink3)]",
        className
      )}
    >
      {children}
    </div>
  );
}

// Keyboard-key chip, per the mockup's .kbd (palette hint, cheat-sheet).
export function Kbd({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <kbd
      className={cn(
        "rounded border border-b-2 border-[var(--fc-line)] bg-[var(--fc-panel)] px-[5px] font-sans text-[10.5px] text-[var(--fc-ink2)]",
        className
      )}
    >
      {children}
    </kbd>
  );
}

export const SEV_STRIPE: Record<string, string> = {
  crit: "bg-[var(--fc-crit)]",
  warn: "bg-[var(--fc-warn)]",
  info: "bg-[var(--fc-acc-dim)]",
};
