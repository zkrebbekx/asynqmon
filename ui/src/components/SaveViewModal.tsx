// "Save view…" modal (build contract §4.2/§4.5): names the current console
// or directory state and POSTs it to the server-side view store, making it a
// team asset launchable from the ⌘K palette. The state params are captured
// by the caller (urlstate-owned params only, page/cursor stripped).

import { useEffect, useState } from "react";
import { toast } from "sonner";
import { createView, ViewTarget } from "../api-views";
import { toErrorString } from "../utils";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "./ui/dialog";
import { Button } from "./ui/button";
import { Input } from "./ui/input";

interface Props {
  open: boolean;
  target: ViewTarget;
  state: Record<string, string>;
  onClose: () => void;
}

export default function SaveViewModal({ open, target, state, onClose }: Props) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (open) {
      setName("");
      setError("");
      setBusy(false);
    }
  }, [open]);

  const stateEcho = Object.entries(state)
    .map(([k, v]) => `${k}=${v}`)
    .join("  ");

  const save = async () => {
    if (name.trim() === "" || busy) return;
    setBusy(true);
    try {
      await createView({ name: name.trim(), target, state });
      toast(`View saved — “${name.trim()}” is now in the ⌘K palette`);
      onClose();
    } catch (e) {
      setError(toErrorString(e));
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>Save view</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          <p className="text-xs text-[var(--fc-ink2)]">
            Saved views are stored server-side — a team asset for on-call
            handoff, launchable from the command palette (⌘K).
          </p>
          <div className="rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-3 py-2 font-mono text-[11px] text-[var(--fc-ink2)]">
            {stateEcho || <span className="italic">default {target} view (no filters)</span>}
          </div>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") save();
            }}
            placeholder="Name — e.g. gw-timeout blast"
            maxLength={120}
            autoFocus
          />
          {error && <p className="text-xs text-[var(--fc-crit)]">{error}</p>}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" size="sm" onClick={onClose}>
              Cancel
            </Button>
            <Button size="sm" disabled={name.trim() === "" || busy} onClick={save}>
              {busy ? "Saving…" : "Save view"}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
