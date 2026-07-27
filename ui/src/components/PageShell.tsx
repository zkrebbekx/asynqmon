// PageShell — the single page container for every routed view, plus the
// shared fc surface classes (see ui/src/DESIGN.md). Every route renders
// inside the same shell so switching nav items never changes how the app
// fills the screen: full-width up to 1560px, centered beyond, px-4 py-4.
//
// The optional title row is the standard page header: a MicroLabel-style
// eyebrow line and/or a compact 15px semibold title, with right-aligned
// controls. Dense dashboard views (Fleet, Queues, Errors…) omit it — the
// topbar breadcrumb owns page identity there.

import { ReactNode } from "react";
import { cn } from "../lib/utils";
import { MicroLabel } from "./FleetBits";

// Card/panel surface — the only sanctioned card chrome.
export const panelClass =
  "rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]";

// Table header cell — letterspaced caps, per the fc table convention.
export const theadClass =
  "whitespace-nowrap border-b border-[var(--fc-line)] px-2.5 py-[7px] text-left text-[10.5px] font-semibold uppercase tracking-[0.07em] text-[var(--fc-ink3)]";

// Table body cell (numeric cells add "text-right font-mono tabular-nums").
export const cellClass =
  "border-b border-[var(--fc-line2)] px-2.5 py-1.5 align-middle";

interface Props {
  // Letterspaced-caps eyebrow above the title (MicroLabel style).
  eyebrow?: ReactNode;
  // Compact page title. Prefer the topbar breadcrumb for identity; use this
  // only when the page needs an in-page header row (e.g. to host controls).
  title?: ReactNode;
  // Muted annotation rendered under the title.
  sub?: ReactNode;
  // Right-aligned controls in the title row.
  actions?: ReactNode;
  // Merged onto the container; use for vertical rhythm ("space-y-3") or a
  // deliberate narrow page ("max-w-2xl", Settings-style form pages).
  className?: string;
  children: ReactNode;
}

export default function PageShell({
  eyebrow,
  title,
  sub,
  actions,
  className,
  children,
}: Props) {
  const hasHeader = eyebrow !== undefined || title !== undefined || actions !== undefined;
  return (
    <div className={cn("mx-auto w-full max-w-[1560px] px-4 py-4", className)}>
      {hasHeader && (
        <div className="mb-3 flex flex-wrap items-center gap-x-3 gap-y-1">
          <div className="min-w-0">
            {eyebrow !== undefined && <MicroLabel>{eyebrow}</MicroLabel>}
            {title !== undefined && (
              <h1 className="text-[15px] font-semibold leading-tight text-[var(--fc-ink)]">
                {title}
              </h1>
            )}
            {sub !== undefined && (
              <div className="mt-0.5 text-[11px] text-[var(--fc-ink2)]">{sub}</div>
            )}
          </div>
          {actions !== undefined && (
            <>
              <span className="flex-1" />
              <div className="flex flex-wrap items-center gap-2">{actions}</div>
            </>
          )}
        </div>
      )}
      {children}
    </div>
  );
}
