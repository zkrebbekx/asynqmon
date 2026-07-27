// The §4.3 bulk-op confirm modal: scope echo, streaming preview count (a
// preview job created on open, fed live by the `jobs` SSE events with the
// hook's 2s poll fallback), cost disclosure for LIST removals, explicit verb
// semantics, throttle + mandatory reason, typed confirmation for large
// deletes, and the execute gate. Executing hands off to the background job
// runner — the modal then STAYS OPEN showing live execute progress (closing
// is fine too; Operations has the job either way).

import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { AlertTriangle } from "lucide-react";
import * as api from "../api";
import { JobDetail, JobInfo, JobVerb } from "../api";
import { paths } from "../paths";
import {
  DEFAULT_THROTTLE,
  THROTTLE_OPTIONS,
  archiveTrimWarning,
  costEstimateLabel,
  executeEtaSeconds,
  fmtEta,
  gateExecute,
  isTerminalJobState,
  jobProgress,
  scopeLabel,
  typedConfirmPhrase,
  verbSemantics,
} from "../lib/bulkjob";
import { useJobProgress } from "../hooks/useJobProgress";
import { toErrorString } from "../utils";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "./ui/dialog";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { cn } from "../lib/utils";

export interface BulkJobScope {
  queue: string;
  state: string;
  q: string;
  meta: string[];
  // AQL predicate (phase 6) — ANDed with the legacy fields server-side.
  aql?: string;
}

interface Props {
  open: boolean;
  verb: JobVerb;
  scope: BulkJobScope;
  onClose: () => void;
  // Called after execution is accepted (job handed to the runner).
  onStarted?: (jobId: string) => void;
}

const verbTitles: Record<JobVerb, string> = {
  run: "Bulk run — paced re-drain",
  archive: "Bulk archive — recoverable",
  delete: "Bulk delete — gone forever, not recoverable",
  cancel: "Bulk cancel — cooperative signal",
};

const verbChipClass: Record<JobVerb, string> = {
  run: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  archive: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  delete: "bg-[var(--fc-crit-bg)] text-[var(--fc-crit)]",
  cancel: "bg-[var(--fc-warn-bg)] text-[var(--fc-warn)]",
};

export default function BulkJobModal({ open, verb, scope, onClose, onStarted }: Props) {
  const [job, setJob] = useState<JobDetail | null>(null);
  const [reason, setReason] = useState("");
  const [throttle, setThrottle] = useState<number>(DEFAULT_THROTTLE);
  const [typedConfirm, setTypedConfirm] = useState("");
  const [proceedOnPartial, setProceedOnPartial] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [previewedAt, setPreviewedAt] = useState<Date | null>(null);
  // The job survives in state after handoff; started guards double-execute,
  // flips the modal into its live execute-progress form, and makes
  // close-after-start skip the cleanup cancel.
  const [started, setStarted] = useState(false);
  const startedRef = useRef(false);
  const jobIdRef = useRef<string | null>(null);
  // The preview/execute job id as STATE so the live tracker re-subscribes.
  const [jobId, setJobId] = useState<string | null>(null);

  // Live counts: `jobs` SSE events with the hook's own 2s poll fallback —
  // the modal renders fine on polling alone. onSettled announces the
  // execute-phase outcome if the operator kept the modal open.
  const onSettled = useCallback(
    (j: JobInfo) => {
      if (j.phase !== "execute") return; // a canceled orphan preview is not news
      if (j.state === "done") {
        toast.success(
          `Bulk ${verb} complete — ${j.counts.acted.toLocaleString()} acted, ` +
            `${j.counts.skipped.toLocaleString()} skipped, ${j.counts.failed.toLocaleString()} failed`
        );
      } else if (j.state === "failed") {
        toast.error(`Bulk ${verb} failed${j.error ? ` — ${j.error}` : ""}`);
      }
    },
    [verb]
  );
  const { job: liveJob, source: liveSource } = useJobProgress(
    open ? jobId : null,
    { onSettled }
  );
  useEffect(() => {
    if (!liveJob) return;
    setJob((prev) =>
      prev
        ? { ...prev, ...liveJob }
        : { sample: [], failures: [], failures_total: 0, ...liveJob }
    );
  }, [liveJob]);

  // Create the preview job when the modal opens (§4.3 step 2: enumeration
  // is an async preview job, never an inline blocking call — the streaming
  // count is the feature). The reason field is still empty at this point,
  // so the preview is created with an explicit placeholder; the operator's
  // real reason travels in the execute request and replaces it (the
  // completion audit entry therefore carries the final text). A preview
  // that is never executed is canceled on close.
  const createPreview = useCallback(async () => {
    const j = await api.createJob({
      verb,
      scope: {
        queue: scope.queue,
        state: scope.state,
        q: scope.q || undefined,
        meta: scope.meta.length ? scope.meta : undefined,
        aql: scope.aql || undefined,
      },
      reason: "(preview only — not yet confirmed)",
      throttle: DEFAULT_THROTTLE,
    });
    jobIdRef.current = j.id;
    setJobId(j.id);
    setPreviewedAt(new Date());
    const detail = await api.getJob(j.id);
    setJob(detail);
    return j.id;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [verb, scope.queue, scope.state, scope.q, scope.meta.join("|"), scope.aql]);

  // Open: reset and start the preview. Ongoing freshness comes from the
  // useJobProgress feed above (SSE first, 2s poll fallback).
  useEffect(() => {
    if (!open) return;
    setJob(null);
    setReason("");
    setThrottle(DEFAULT_THROTTLE);
    setTypedConfirm("");
    setProceedOnPartial(false);
    setError("");
    setBusy(false);
    setStarted(false);
    startedRef.current = false;
    jobIdRef.current = null;
    setJobId(null);
    createPreview().catch((e) => setError(toErrorString(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // Closing without executing cancels the orphan preview job (best-effort).
  const handleClose = () => {
    const cur = jobIdRef.current;
    if (cur && !startedRef.current) {
      api.cancelJob(cur).catch(() => {});
    }
    onClose();
  };

  const counts = job?.counts ?? { candidates: 0, acted: 0, skipped: 0, failed: 0, scanned: 0 };
  const previewComplete = job?.preview_complete ?? false;
  const isListRemoval = job?.cost_class === "list_removal";
  const confirmPhrase = previewComplete ? typedConfirmPhrase(verb, counts.candidates) : null;
  const trimWarning = archiveTrimWarning(verb, counts.candidates);

  const gate = gateExecute({
    verb,
    previewComplete,
    candidates: counts.candidates,
    reason,
    typedConfirm,
    proceedOnPartial,
  });

  const handleExecute = async () => {
    const id = jobIdRef.current;
    if (!gate.ok || busy || !id) return;
    setBusy(true);
    setError("");
    try {
      // Execute the SAME job whose preview streamed into this modal — the
      // delete gate depends on that preview's completeness. The operator's
      // final reason and throttle travel with the execute request; the
      // server re-checks the whole gate.
      await api.executeJob(id, {
        proceed_on_partial: proceedOnPartial,
        throttle,
        reason,
      });
      startedRef.current = true;
      setStarted(true);
      toast.success(`Bulk ${verb} handed to the job runner`, {
        description:
          "Live progress right here while the modal is open; per-item failures and the verify panel live in Operations.",
        action: { label: "Open Ops", onClick: () => (window.location.href = paths().OPS) },
      });
      // The modal STAYS OPEN, tracking execute progress live. Closing is
      // fine at any point — the job runs server-side either way.
      onStarted?.(id);
    } catch (e) {
      setError(toErrorString(e));
    }
    setBusy(false);
  };

  // Execute-phase live progress (started + kept open).
  const execFraction = jobProgress(counts);
  const execDone = counts.acted + counts.skipped + counts.failed;
  const execEta =
    job && !isTerminalJobState(job.state)
      ? executeEtaSeconds(counts, job.throttle || throttle)
      : null;

  return (
    <Dialog open={open} onOpenChange={(o) => !o && handleClose()}>
      <DialogContent className="max-w-2xl" onClick={(e) => e.stopPropagation()}>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-sm">
            <span
              className={cn(
                "rounded px-1.5 py-0.5 text-[10.5px] font-bold uppercase tracking-wide",
                verbChipClass[verb]
              )}
            >
              {verb}
            </span>
            {verbTitles[verb]}
          </DialogTitle>
        </DialogHeader>

        <div className="space-y-3 text-xs">
          {/* Scope echo */}
          <div>
            <div className="mb-1 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
              scope
            </div>
            <div className="rounded border border-[var(--fc-line)] bg-[var(--fc-raise)] px-2.5 py-1.5 font-mono text-[11.5px]">
              {scopeLabel(scope)}
            </div>
          </div>

          {/* Streaming preview meter (§4.3 step 2) — fed by the `jobs` SSE
              events (2s poll fallback). Swaps to the execute progress meter
              once the job is handed to the runner. */}
          {!started ? (
            <div className="flex flex-wrap items-center gap-2" data-testid="preview-meter">
              <span className="text-[var(--fc-ink3)]">preview</span>
              <div className="h-1.5 w-32 overflow-hidden rounded bg-[var(--fc-raise)]">
                <div
                  className={cn(
                    "h-full bg-[var(--fc-acc)] transition-all",
                    !previewComplete && "animate-pulse"
                  )}
                  style={{ width: previewComplete ? "100%" : "45%" }}
                />
              </div>
              <span className="font-mono tabular-nums">
                {job
                  ? previewComplete
                    ? `${counts.candidates.toLocaleString()} tasks — exact`
                    : `≥${counts.candidates.toLocaleString()} and counting…`
                  : "starting preview…"}
              </span>
              {previewedAt && (
                <span className="text-[10.5px] text-[var(--fc-ink3)]">
                  as of {previewedAt.toLocaleTimeString()} — the live set changes; execution
                  re-verifies each task
                </span>
              )}
            </div>
          ) : (
            <div className="flex flex-wrap items-center gap-2" data-testid="execute-meter">
              <span className="text-[var(--fc-ink3)]">progress</span>
              <div className="h-1.5 w-32 overflow-hidden rounded bg-[var(--fc-raise)]">
                <div
                  className={cn(
                    "h-full transition-all",
                    job?.state === "failed"
                      ? "bg-[var(--fc-crit)]"
                      : job?.state === "done"
                        ? "bg-[var(--fc-good)]"
                        : "bg-[var(--fc-acc)]"
                  )}
                  style={{
                    width: `${Math.round((job && isTerminalJobState(job.state) ? 1 : (execFraction ?? 0)) * 100)}%`,
                  }}
                />
              </div>
              <span className="font-mono tabular-nums">
                {job && isTerminalJobState(job.state)
                  ? `${job.state} — ${counts.acted.toLocaleString()} acted, ` +
                    `${counts.skipped.toLocaleString()} skipped, ${counts.failed.toLocaleString()} failed`
                  : `${execDone.toLocaleString()} of ${counts.candidates.toLocaleString()}` +
                    `${job?.state === "paused" ? " · paused" : ""}` +
                    `${execEta !== null ? ` · ETA ${fmtEta(execEta)}` : ""}`}
              </span>
              {job !== null && !isTerminalJobState(job.state) && (
                <span className="text-[10.5px] text-[var(--fc-ink3)]">
                  {liveSource === "sse" ? "live stream" : "polling"}
                </span>
              )}
            </div>
          )}

          {/* Cost disclosure (§4.3 step 3) */}
          {!started && isListRemoval && (
            <div className="rounded border border-[var(--fc-warn)]/40 bg-[var(--fc-warn-bg)] p-2.5 leading-relaxed">
              <b>⚠ Cost class: LIST removal.</b> Selective {verb} from a{" "}
              {(job?.cost_list_len ?? 0).toLocaleString()}-entry {scope.state} list is{" "}
              <span className="font-mono">LREM</span> — O(list length) <i>per task</i> ≈{" "}
              <b>
                {costEstimateLabel(
                  Math.max(counts.candidates, 1),
                  job?.cost_list_len ?? 0
                )}{" "}
                of single-threaded Redis time
              </b>{" "}
              at this size (order of magnitude). Alternatives:
              <ol className="ml-4 mt-1 list-decimal space-y-0.5">
                <li>
                  <b>Pause the queue first</b> — the list stops churning, then drain (recommended)
                </li>
                <li>
                  <b>Whole-state operation</b> — a single chunked drain, O(n) once
                </li>
                <li>
                  <b>Archive-by-signature later</b> — after tasks fail naturally
                </li>
              </ol>
              The runner additionally throttles LIST-removal batches harder.
            </div>
          )}

          {/* Verb semantics (§4.3 step 4) */}
          {!started && (
          <div
            className={cn(
              "rounded border p-2.5",
              verb === "delete"
                ? "border-[var(--fc-crit)]/40 bg-[var(--fc-crit-bg)] text-[var(--fc-crit)]"
                : "border-[var(--fc-line)] bg-[var(--fc-raise)] text-[var(--fc-ink2)]"
            )}
          >
            {verbSemantics(verb)}
            {verb === "delete" && !previewComplete && (
              <div className="mt-1 font-semibold">
                Delete requires a <b>completed</b> preview. The button unlocks when the count is
                exact.
              </div>
            )}
            {trimWarning && (
              <div className="mt-1 flex items-start gap-1.5 text-[var(--fc-warn)]">
                <AlertTriangle size={13} className="mt-0.5 shrink-0" />
                {trimWarning}
              </div>
            )}
          </div>
          )}

          {/* Throttle + reason (§4.3 step 4) */}
          {!started && (
          <div className="grid grid-cols-[160px_1fr] gap-3">
            <label className="block">
              <span className="mb-1 block text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
                Throttle
              </span>
              <select
                value={throttle}
                onChange={(e) => setThrottle(Number(e.target.value))}
                aria-label="Throttle"
                className="h-8 w-full rounded-md border border-[hsl(var(--input))] bg-[hsl(var(--background))] px-2 text-xs"
              >
                {THROTTLE_OPTIONS.map((t) => (
                  <option key={t} value={t}>
                    {t} tasks/sec{t === DEFAULT_THROTTLE ? " (default)" : ""}
                  </option>
                ))}
              </select>
            </label>
            <label className="block">
              <span className="mb-1 block text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
                Reason — required, goes to audit
              </span>
              <Input
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="e.g. flatten gw-timeout retry storm"
                className="h-8 text-xs"
              />
            </label>
          </div>
          )}

          {/* Partial-count acknowledgment (non-delete verbs) */}
          {!started && !previewComplete && verb !== "delete" && (
            <label className="flex items-start gap-2">
              <input
                type="checkbox"
                checked={proceedOnPartial}
                onChange={(e) => setProceedOnPartial(e.target.checked)}
                className="mt-0.5"
              />
              <span className="text-[var(--fc-ink2)]">
                Proceed on the partial count — execution completes its own enumeration server-side
                and reports the true final count.
              </span>
            </label>
          )}

          {/* Typed confirmation for large deletes */}
          {!started && confirmPhrase && (
            <label className="block">
              <span className="mb-1 block text-[10px] uppercase tracking-[0.1em] text-[var(--fc-crit)]">
                Type {confirmPhrase} to confirm (count &gt; 1,000)
              </span>
              <Input
                value={typedConfirm}
                onChange={(e) => setTypedConfirm(e.target.value)}
                placeholder={confirmPhrase}
                className="h-8 font-mono text-xs"
              />
            </label>
          )}

          {error && <div className="text-[var(--fc-crit)]">{error}</div>}
          {!started && !gate.ok && !error && (
            <div className="text-[var(--fc-ink3)]" data-testid="gate-hint">
              {gate.blockedBecause}
            </div>
          )}
        </div>

        <div className="flex items-center justify-end gap-2">
          <Link
            to={paths().OPS}
            className="mr-auto text-xs text-[var(--fc-acc)] hover:underline"
            onClick={handleClose}
          >
            View job history in Operations →
          </Link>
          <Button variant="outline" size="sm" onClick={handleClose} disabled={busy}>
            {started ? "Close" : "Cancel"}
          </Button>
          {!started && (
            <Button
              size="sm"
              variant={verb === "delete" ? "destructive" : "default"}
              disabled={!gate.ok || busy}
              onClick={handleExecute}
            >
              {busy ? "Starting…" : `${verb.charAt(0).toUpperCase() + verb.slice(1)} as background job`}
            </Button>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
