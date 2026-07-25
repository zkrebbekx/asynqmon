// Keyboard cheat-sheet overlay (build contract §4.5, "?"). Layout and rows
// match the approved mockup's cheatsheet exactly: a two-column grid of
// action → key chips. Esc or backdrop click closes; the panel takes focus on
// open so Esc lands here (capture phase, so an open task drawer underneath
// does not also close).

import { useEffect, useRef } from "react";
import { Kbd } from "./FleetBits";

const ROWS: Array<[string, string]> = [
  ["navigate rows", "j / k"],
  ["select · range", "x · ⇧x"],
  ["run", "r"],
  ["archive", "e"],
  ["delete (confirms)", "#"],
  ["peek drawer", "p"],
  ["prev / next in drawer", "[ · ]"],
  ["focus query", "/"],
  ["command palette", "⌘K"],
  ["this sheet", "?"],
];

interface Props {
  open: boolean;
  onClose: () => void;
}

export default function KeyboardCheatsheet({ open, onClose }: Props) {
  const boxRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    boxRef.current?.focus();
    // Capture phase: claim Esc before the drawer's document-level listener.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-[60] grid place-items-center bg-[rgba(4,8,14,0.6)]"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={boxRef}
        role="dialog"
        aria-modal="true"
        aria-label="Keyboard shortcuts"
        tabIndex={-1}
        className="w-[520px] max-w-[92vw] rounded-[10px] border border-[var(--fc-line)] bg-[var(--fc-panel)] px-[22px] py-[18px] text-[var(--fc-ink)] shadow-xl outline-none"
      >
        <b className="text-sm font-semibold">Keyboard</b>
        <div className="mt-3 grid grid-cols-2 gap-x-[26px] gap-y-1.5 text-[12.5px]">
          {ROWS.map(([action, key]) => (
            <div key={action} className="flex justify-between gap-3 text-[var(--fc-ink2)]">
              <span>{action}</span>
              <Kbd>{key}</Kbd>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
