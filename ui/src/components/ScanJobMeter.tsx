// ScanJobMeter — the /tasks scan meter's live-job form. When the operator
// promotes a partial scan to a scan-to-completion preview job the console
// STAYS here: this meter tracks the job's server-side enumeration live
// (`jobs` SSE events with a 2s poll fallback — useJobProgress feeds it),
// counting scanned/candidates up, showing an observed-rate ETA when one is
// computable, and offering cancel. On completion it states the exact match
// count. Presentational only — all transport lives in the parent's hook.

import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { X } from "lucide-react";
import { JobInfo } from "../api";
import {
  ScanSample,
  fmtEta,
  isTerminalJobState,
  scanEtaSeconds,
} from "../lib/bulkjob";
import { paths } from "../paths";
import { Button } from "./ui/button";
import { cn } from "../lib/utils";

interface Props {
  // The tracked job; null while attaching (first fetch in flight).
  job: JobInfo | null;
  // Whether the meter is fed by the live stream (vs the poll fallback) —
  // rendered with the same vocabulary as Fleet's source pill.
  live: boolean;
  // The console's candidate estimate for the scanned state set (the
  // enumeration bound); 0 = unknown, the bar then animates indeterminately.
  candidateEstimate: number;
  // Cancel the running job (wired to POST /api/jobs/:id/cancel).
  onCancel: () => void;
  // Dismiss the meter (drops the scanjob URL param; parent cancels a
  // still-running job best-effort).
  onDismiss: () => void;
}

export default function ScanJobMeter({
  job,
  live,
  candidateEstimate,
  onCancel,
  onDismiss,
}: Props) {
  // Observed-rate ETA: anchor the first sample per job, rate = scanned delta
  // over elapsed time. Honest-numbers rule: no rate or no bound → no ETA.
  const anchorRef = useRef<{ id: string; sample: ScanSample } | null>(null);
  const [eta, setEta] = useState<number | null>(null);
  useEffect(() => {
    if (!job || job.preview_complete || isTerminalJobState(job.state)) {
      setEta(null);
      return;
    }
    const sample: ScanSample = { t: Date.now(), scanned: job.counts.scanned };
    if (!anchorRef.current || anchorRef.current.id !== job.id) {
      anchorRef.current = { id: job.id, sample };
      setEta(null);
      return;
    }
    setEta(scanEtaSeconds(anchorRef.current.sample, sample, candidateEstimate));
  }, [job, candidateEstimate]);

  const scanned = job?.counts.scanned ?? 0;
  const matches = job?.counts.candidates ?? 0;
  const complete = job?.preview_complete ?? false;
  const canceled = job?.state === "canceled";
  const failed = job?.state === "failed";
  const paused = job?.state === "paused";
  const settled = complete || (job !== null && isTerminalJobState(job.state));

  const pct =
    complete || canceled || failed
      ? 100
      : candidateEstimate > 0
        ? Math.min(100, (scanned / candidateEstimate) * 100)
        : 0;

  return (
    <div
      data-testid="scan-job-meter"
      className={cn(
        "flex flex-wrap items-center gap-3 rounded-lg border bg-[var(--fc-panel)] px-4 py-2 text-xs",
        failed ? "border-[var(--fc-crit)]/50" : "border-[var(--fc-line)]"
      )}
    >
      <span className="text-[var(--fc-ink3)]">scan job</span>
      <div className="h-1.5 w-40 overflow-hidden rounded-full bg-[var(--fc-raise)]">
        <div
          className={cn(
            "h-full transition-all",
            failed
              ? "bg-[var(--fc-crit)]"
              : canceled
                ? "bg-[var(--fc-ink3)]"
                : complete
                  ? "bg-[var(--fc-good)]"
                  : "bg-[var(--fc-acc)]",
            !settled && candidateEstimate <= 0 && "w-2/5 animate-pulse"
          )}
          style={
            settled || candidateEstimate > 0 ? { width: `${pct}%` } : undefined
          }
        />
      </div>

      <span className="font-mono tabular-nums">
        {job === null ? (
          "attaching to scan job…"
        ) : failed ? (
          <>scan failed{job.error ? ` — ${job.error}` : ""}</>
        ) : canceled ? (
          <>
            scan canceled — {matches.toLocaleString()} matches (partial,{" "}
            {scanned.toLocaleString()} scanned)
          </>
        ) : complete ? (
          <>scan complete — {matches.toLocaleString()} matches (exact)</>
        ) : (
          <>
            scanned {scanned.toLocaleString()}
            {candidateEstimate > 0 &&
              ` of ~${candidateEstimate.toLocaleString()}`}{" "}
            · {matches.toLocaleString()} matches
            {paused && " · paused"}
            {eta !== null && ` · ETA ${fmtEta(eta)}`}
          </>
        )}
      </span>

      {job !== null && !settled && (
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          {live ? "live stream" : "polling"}
        </span>
      )}

      <div className="ml-auto flex items-center gap-2">
        {job !== null && (
          <Link
            to={paths().OPS}
            className="text-[var(--fc-acc)] hover:underline"
          >
            {job.id.slice(0, 11)}… → Operations
          </Link>
        )}
        {job !== null && !settled && !window.READ_ONLY && (
          <Button
            size="sm"
            variant="outline"
            className="h-6 text-xs"
            onClick={onCancel}
          >
            Cancel scan
          </Button>
        )}
        {settled && (
          <Button
            size="icon"
            variant="ghost"
            className="h-6 w-6"
            aria-label="Dismiss scan job meter"
            onClick={onDismiss}
          >
            <X size={13} />
          </Button>
        )}
      </div>
    </div>
  );
}
