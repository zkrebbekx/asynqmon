// The clone-and-edit-then-enqueue modal (build contract §3.5 → §5.10):
// opened from the task drawer's "Clone & edit…" action, prefilled from the
// source task where asynq actually stores the value, submitted to the
// flag-gated POST /api/queues/{qname}/tasks. Success lands a toast with a
// link that peeks the freshly created task.

import { useCallback, useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import * as api from "../api";
import { TaskInfo } from "../api";
import {
  CloneDraft,
  buildEnqueueRequest,
  draftErrors,
  payloadJsonState,
  prefillFromTask,
} from "../lib/enqueue";
import { toErrorString } from "../utils";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "./ui/dialog";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { cn } from "../lib/utils";

interface Props {
  open: boolean;
  // The source task the draft is prefilled from.
  sourceTask: TaskInfo;
  onClose: () => void;
  // Called with the created task so the drawer can peek it.
  onEnqueued: (queue: string, id: string) => void;
}

function FieldLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="mb-1 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
      {children}
    </div>
  );
}

export default function CloneEnqueueModal({ open, sourceTask, onClose, onEnqueued }: Props) {
  const [draft, setDraft] = useState<CloneDraft>(() => prefillFromTask(sourceTask));
  const [queues, setQueues] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  // (Re)prefill whenever the modal opens — possibly for a different task.
  useEffect(() => {
    if (!open) return;
    setDraft(prefillFromTask(sourceTask));
    setError("");
    setBusy(false);
    // Queue typeahead options; best-effort.
    api
      .listQueues()
      .then((r) => setQueues(r.queues.map((q) => q.queue).sort()))
      .catch(() => setQueues([]));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, sourceTask.id, sourceTask.queue]);

  const set = useCallback(
    <K extends keyof CloneDraft>(key: K, value: CloneDraft[K]) =>
      setDraft((d) => ({ ...d, [key]: value })),
    []
  );

  const errs = useMemo(() => draftErrors(draft), [draft]);
  const jsonState = payloadJsonState(draft.payload);

  const submit = async () => {
    if (busy || Object.keys(errs).length > 0) return;
    setBusy(true);
    setError("");
    try {
      const created = await api.enqueueTask(draft.queue.trim(), buildEnqueueRequest(draft));
      toast.success(`Task enqueued into ${created.queue}`, {
        description: <span className="font-mono text-[11px]">{created.id}</span>,
        action: {
          label: "View task",
          onClick: () => onEnqueued(created.queue, created.id),
        },
      });
      onClose();
    } catch (e) {
      setError(toErrorString(e));
      setBusy(false);
    }
  };

  const numberField = (
    label: string,
    key: "maxRetry" | "timeoutSeconds" | "retentionSeconds" | "uniqueTtlSeconds" | "processInSeconds",
    placeholder: string
  ) => (
    <div>
      <FieldLabel>{label}</FieldLabel>
      <Input
        inputMode="numeric"
        value={draft[key]}
        placeholder={placeholder}
        aria-label={label}
        onChange={(e) => set(key, e.target.value)}
        className={cn("h-8 font-mono text-xs", errs[key] && "border-[var(--fc-crit)]")}
      />
      {errs[key] && <div className="mt-0.5 text-[10.5px] text-[var(--fc-crit)]">{errs[key]}</div>}
    </div>
  );

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        className="max-w-2xl"
        onClick={(e) => e.stopPropagation()}
        aria-describedby={undefined}
      >
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-sm">
            <span className="rounded bg-[var(--fc-acc-bg)] px-1.5 py-0.5 text-[10.5px] font-bold uppercase tracking-wide text-[var(--fc-acc)]">
              enqueue
            </span>
            Clone &amp; edit — new task from{" "}
            <span className="max-w-[180px] truncate font-mono text-xs">{sourceTask.id}</span>
          </DialogTitle>
        </DialogHeader>

        <div className="space-y-3 text-xs">
          <div className="grid grid-cols-2 gap-3">
            <div>
              <FieldLabel>type</FieldLabel>
              <Input
                value={draft.type}
                aria-label="type"
                onChange={(e) => set("type", e.target.value)}
                className={cn("h-8 font-mono text-xs", errs.type && "border-[var(--fc-crit)]")}
              />
              {errs.type && (
                <div className="mt-0.5 text-[10.5px] text-[var(--fc-crit)]">{errs.type}</div>
              )}
            </div>
            <div>
              <FieldLabel>queue</FieldLabel>
              <Input
                value={draft.queue}
                aria-label="queue"
                list="clone-enqueue-queues"
                onChange={(e) => set("queue", e.target.value)}
                className={cn("h-8 font-mono text-xs", errs.queue && "border-[var(--fc-crit)]")}
              />
              <datalist id="clone-enqueue-queues">
                {queues.map((q) => (
                  <option key={q} value={q} />
                ))}
              </datalist>
              {errs.queue && (
                <div className="mt-0.5 text-[10.5px] text-[var(--fc-crit)]">{errs.queue}</div>
              )}
            </div>
          </div>

          <div>
            <div className="mb-1 flex items-center justify-between">
              <FieldLabel>payload</FieldLabel>
              <label className="flex cursor-pointer items-center gap-1.5 text-[10.5px] text-[var(--fc-ink3)]">
                <input
                  type="checkbox"
                  checked={draft.payloadBase64}
                  onChange={(e) => set("payloadBase64", e.target.checked)}
                  aria-label="payload is base64"
                />
                base64 (binary payload)
              </label>
            </div>
            <textarea
              value={draft.payload}
              aria-label="payload"
              rows={7}
              spellCheck={false}
              onChange={(e) => set("payload", e.target.value)}
              className={cn(
                "w-full resize-y rounded-md border border-[var(--fc-line)] bg-[var(--fc-bg)] p-2 font-mono text-[11.5px] leading-relaxed",
                "focus-visible:outline focus-visible:outline-2 focus-visible:outline-[var(--fc-acc)]",
                errs.payload && "border-[var(--fc-crit)]"
              )}
            />
            {/* Validate-on-type note (§3.5): JSON status in text mode, base64
                validity in binary mode; empty payloads are fine. */}
            {draft.payloadBase64 ? (
              errs.payload ? (
                <div className="text-[10.5px] text-[var(--fc-crit)]">{errs.payload}</div>
              ) : (
                <div className="text-[10.5px] text-[var(--fc-ink3)]">
                  will be decoded and submitted as binary
                </div>
              )
            ) : jsonState === "valid" ? (
              <div className="text-[10.5px] text-[var(--fc-good)]">valid JSON</div>
            ) : jsonState === "invalid" ? (
              <div className="text-[10.5px] text-[var(--fc-warn)]">
                invalid JSON — will submit as raw string
              </div>
            ) : (
              <div className="text-[10.5px] text-[var(--fc-ink3)]">empty payload</div>
            )}
          </div>

          <div className="grid grid-cols-3 gap-3">
            {numberField("max retry", "maxRetry", "25 (default)")}
            {numberField("timeout (s)", "timeoutSeconds", "1800 (default)")}
            {numberField("retention (s)", "retentionSeconds", "none")}
            {numberField("unique TTL (s)", "uniqueTtlSeconds", "none")}
            {numberField("process in (s)", "processInSeconds", "now")}
            <div>
              <FieldLabel>reason (audit log)</FieldLabel>
              <Input
                value={draft.reason}
                aria-label="reason"
                placeholder="optional"
                onChange={(e) => set("reason", e.target.value)}
                className="h-8 text-xs"
              />
            </div>
          </div>

          <div className="text-[10.5px] leading-relaxed text-[var(--fc-ink3)]">
            Prefilled from the source task where asynq stores the value (type, queue, payload,
            retry, timeout). Retention, uniqueness and schedule are not recoverable from a task
            record and start blank.
          </div>

          {error && (
            <div
              role="alert"
              className="rounded border border-[var(--fc-crit)] bg-[var(--fc-crit-bg)] px-2.5 py-1.5 text-[11.5px] text-[var(--fc-crit)]"
            >
              {error}
            </div>
          )}

          <div className="flex items-center justify-end gap-2 border-t border-[var(--fc-line2)] pt-3">
            <Button variant="ghost" size="sm" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button
              size="sm"
              onClick={submit}
              disabled={busy || Object.keys(errs).length > 0}
              className="bg-[var(--fc-acc)] text-white hover:bg-[var(--fc-acc)]/90"
            >
              {busy ? "Enqueuing…" : "Enqueue task"}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
