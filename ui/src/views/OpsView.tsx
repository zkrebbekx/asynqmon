// Operations screen (build contract §3.9): every bulk job — verb, scope,
// state, progress (acted / skipped / failed), throttle, actor, reason,
// timestamps — with row expand into the per-item failure report + preview
// sample, a live verify panel for finished jobs (scope re-counted against
// the job's final numbers), and the audit log. Row progress rides the `jobs`
// SSE events when the stream is up; the tight 2s poll remains the fallback
// while any job is non-terminal.

import { useCallback, useEffect, useMemo, useState } from "react";
import { AlertCircle, ChevronDown, ChevronRight, Pause, Play, X } from "lucide-react";
import { useSelector } from "react-redux";
import { Link } from "react-router-dom";
import { AppState } from "../store";
import * as api from "../api";
import { AuditEntry, JobDetail, JobInfo } from "../api";
import { SeriesResponse, getSeries } from "../api-series";
import { HealthRolesResponse, getHealthRoles } from "../api-fleet";
import { HygieneCard, listHygiene } from "../api-hygiene";
import { formatInterval } from "../lib/hygiene";
import { paths } from "../paths";
import { usePolling } from "../hooks";
import { useJobsEvents } from "../hooks/useJobsEvents";
import {
  isTerminalJobState,
  jobProgress,
  scopeLabel,
  verifyVerdict,
} from "../lib/bulkjob";
import { burnDownProjection, projectionSentence, sliceLastSeconds } from "../lib/series";
import { timeAgo, toErrorString, uuidPrefix } from "../utils";
import { cn } from "../lib/utils";
import { BurnDown } from "../components/charts";
import PageShell, { panelClass, theadClass } from "../components/PageShell";
import { Button } from "../components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../components/ui/table";

const stateChip: Record<string, string> = {
  previewing: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  preview_ready: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  running: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  paused: "bg-[var(--fc-warn-bg)] text-[var(--fc-warn)]",
  done: "bg-[var(--fc-good-bg)] text-[var(--fc-good)]",
  canceled: "bg-[var(--fc-raise)] text-[var(--fc-ink3)]",
  failed: "bg-[var(--fc-crit-bg)] text-[var(--fc-crit)]",
};

const verbChip: Record<string, string> = {
  run: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  archive: "bg-[var(--fc-acc-bg)] text-[var(--fc-acc)]",
  delete: "bg-[var(--fc-crit-bg)] text-[var(--fc-crit)]",
  cancel: "bg-[var(--fc-warn-bg)] text-[var(--fc-warn)]",
};

function fmtWhen(iso: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

interface VerifyState {
  loading: boolean;
  verdict: string;
  error: string;
}

/**************************************************************
        Verify burn-down (§4.3 step 6, ring buffers phase 10)
 **************************************************************/

// The §5.8 depth metric a job scope's state maps onto (the state whose
// burn-down proves the remediation). States without a collected series
// (scheduled, completed, aggregating) render no chart — absent, not faked.
const STATE_METRIC: Record<string, string> = {
  pending: "pending",
  active: "active",
  retry: "retry",
  archived: "archived",
};

// VerifyBurnDown fetches the scoped state's hot series and renders the
// burn-down chart + the "drains in ~42m at current rate" projection —
// linear fit over the last 10 samples, ETA ONLY when the slope is negative,
// otherwise the honest "not draining" (§4.3 step 6).
function VerifyBurnDown({ job }: { job: JobInfo }) {
  const metric = STATE_METRIC[job.scope.state ?? ""];
  const scope =
    job.scope.queue && job.scope.queue !== "all" ? `queue:${job.scope.queue}` : "fleet";
  const [series, setSeries] = useState<SeriesResponse | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    if (!metric) return;
    let disposed = false;
    const fetchSeries = async () => {
      try {
        const s = await getSeries({ scope, metric, window: "3h" });
        if (!disposed) {
          setSeries(s);
          setFailed(false);
        }
      } catch {
        if (!disposed) setFailed(true);
      }
    };
    fetchSeries();
    const id = setInterval(fetchSeries, 30_000);
    return () => {
      disposed = true;
      clearInterval(id);
    };
  }, [scope, metric]);

  if (!metric) return null;
  if (failed || series === null) return null;

  // Chart shows the last 30 minutes; the projection fits the last 10 samples.
  const nowSec = Math.floor(Date.now() / 1000);
  const recent = sliceLastSeconds(series.points, 30 * 60, nowSec);
  const proj = burnDownProjection(series.points);
  if (proj === null) {
    return (
      <div className="mt-2 text-[10.5px] text-[var(--fc-ink3)]">
        burn-down: not enough ring-buffer samples yet to project a drain rate
      </div>
    );
  }
  const sentence = projectionSentence(proj);
  const draining = proj.etaMinutes !== null;
  return (
    <div className="mt-2" data-testid="verify-burndown">
      <div className="mb-1 flex items-baseline gap-2 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
        {scope === "fleet" ? "all queues" : job.scope.queue} · {metric} · last 30m
        <span
          className={cn(
            "normal-case tracking-normal",
            draining ? "font-semibold text-[var(--fc-good)]" : "text-[var(--fc-warn)]"
          )}
        >
          {sentence}
        </span>
      </div>
      <BurnDown
        points={recent}
        slopePerMin={proj.slopePerMin}
        etaMinutes={proj.etaMinutes}
        height={72}
      />
    </div>
  );
}

/**************************************************************
          System activity (§3.12 roles + §3.10 hygiene last-runs)
 **************************************************************/

// Same fixed cadence as Settings › Fleet Console Health: a diagnostics
// readout, independent of the global poll slider.
const SYSTEM_ACTIVITY_REFRESH_MS = 15000;

// SystemActivityPanel is the read-only "where does the console's own
// background work show" answer on Operations: the §5.13 singleton role
// leases (stats/errsig/hygiene sweepers) and each hygiene report's last run.
// Renders nothing when neither endpoint answers (backend predates the
// roles/hygiene phases, or is down) — absent, never faked.
function SystemActivityPanel() {
  const [health, setHealth] = useState<HealthRolesResponse | null>(null);
  const [hygiene, setHygiene] = useState<HygieneCard[] | null>(null);

  const load = useCallback(() => {
    getHealthRoles()
      .then(setHealth)
      .catch(() => setHealth(null));
    listHygiene()
      .then((r) => setHygiene(r.reports))
      .catch(() => setHygiene(null));
  }, []);

  useEffect(() => {
    load();
    const id = setInterval(load, SYSTEM_ACTIVITY_REFRESH_MS);
    return () => clearInterval(id);
  }, [load]);

  if (health === null && hygiene === null) return null;

  const sweep = health?.stats?.last_sweep_local;

  return (
    <section className={panelClass} data-testid="system-activity">
      <div className="flex flex-wrap items-baseline gap-2 border-b border-[var(--fc-line2)] px-4 py-2">
        <span className="text-sm font-semibold">System activity</span>
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          Background maintenance run by the console itself — not operator jobs.
        </span>
      </div>
      <div className="grid gap-4 p-4 md:grid-cols-2">
        {/* (a) Singleton role leases (§5.13) */}
        {health && (
          <div>
            <div className="mb-1.5 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
              background roles — lease holders
            </div>
            <div className="overflow-x-auto rounded-md border border-[var(--fc-line)]">
              <table className="w-full border-collapse text-left">
                <thead>
                  <tr>
                    <th className={theadClass}>Role</th>
                    <th className={theadClass}>Holder</th>
                    <th className={theadClass}>Fence</th>
                  </tr>
                </thead>
                <tbody>
                  {health.roles.map((r) => (
                    <tr key={r.role} className="border-b border-[var(--fc-line2)] last:border-b-0">
                      <td className="px-2.5 py-1.5 text-xs text-[var(--fc-ink)]">{r.title}</td>
                      <td className="px-2.5 py-1.5">
                        {r.holder ? (
                          <span className="font-mono text-[11px] text-[var(--fc-ink2)]">
                            {r.holder}
                            {r.held_by_this_replica && (
                              <span className="ml-1.5 rounded-[4px] bg-[var(--fc-good-bg)] px-1 py-px font-sans text-[10.5px] font-semibold text-[var(--fc-good)]">
                                this replica
                              </span>
                            )}
                          </span>
                        ) : (
                          <span className="text-[11px] text-[var(--fc-crit)]">unheld</span>
                        )}
                      </td>
                      <td className="px-2.5 py-1.5 font-mono text-[11px] tabular-nums text-[var(--fc-ink2)]">
                        #{r.fence}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {sweep && (
              <p className="mt-2 text-[11px] text-[var(--fc-ink2)]">
                Last sweep (this replica):{" "}
                <span className="font-mono tabular-nums text-[var(--fc-ink)]">
                  {sweep.read_cmds} reads · {sweep.write_cmds} writes · {sweep.duration_ms}ms ·{" "}
                  {sweep.refreshed}/{sweep.queues} queues refreshed
                </span>
              </p>
            )}
          </div>
        )}

        {/* (b) Hygiene report last-runs (§3.10) */}
        {hygiene && (
          <div>
            <div className="mb-1.5 flex items-baseline gap-2 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
              <span>hygiene reports — last runs</span>
              <span className="flex-1" />
              <Link
                to={paths().HYGIENE}
                className="text-[11px] normal-case tracking-normal text-[var(--fc-acc)] hover:underline"
              >
                Hygiene →
              </Link>
            </div>
            <div className="rounded-md border border-[var(--fc-line)]">
              {hygiene.map((c) => (
                <div
                  key={c.kind}
                  className="flex flex-wrap items-baseline gap-x-2 border-b border-[var(--fc-line2)] px-2.5 py-1.5 text-xs last:border-b-0"
                >
                  <span className="font-medium text-[var(--fc-ink)]">{c.title}</span>
                  <span className="text-[var(--fc-ink3)]">
                    {c.enabled ? formatInterval(c.interval_seconds) : "disabled"}
                  </span>
                  <span className="ml-auto font-mono text-[10.5px] tabular-nums text-[var(--fc-ink3)]">
                    {c.last_generated_at ? timeAgo(c.last_generated_at) : "never"}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </section>
  );
}

export default function OpsView() {
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const [jobs, setJobs] = useState<JobInfo[]>([]);
  const [audit, setAudit] = useState<AuditEntry[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [detail, setDetail] = useState<JobDetail | null>(null);
  const [verify, setVerify] = useState<VerifyState | null>(null);

  const fetchAll = useCallback(async () => {
    try {
      const [j, a] = await Promise.all([api.listJobs(100), api.listAudit(50)]);
      setJobs(j.jobs);
      setAudit(a.entries);
      setError("");
    } catch (e) {
      setError(toErrorString(e));
    }
    setLoading(false);
  }, []);

  usePolling(fetchAll, pollInterval, []);

  // Live row progress: the `jobs` SSE events (on-connect snapshot + per-job
  // ticks). Merged OVER the polled list, never regressing a terminal row;
  // jobs born after the last poll are prepended so they appear instantly.
  const { jobs: liveJobs, source: liveSource } = useJobsEvents(true);
  const rows = useMemo(() => {
    const liveList = Object.values(liveJobs);
    if (liveList.length === 0) return jobs;
    const known = new Set(jobs.map((j) => j.id));
    const merged = jobs.map((j) => {
      const live = liveJobs[j.id];
      if (!live) return j;
      if (isTerminalJobState(j.state) && !isTerminalJobState(live.state)) return j;
      return live;
    });
    const fresh = liveList
      .filter((j) => !known.has(j.id))
      .sort((a, b) => (a.created_at < b.created_at ? 1 : -1));
    return [...fresh, ...merged];
  }, [jobs, liveJobs]);

  // §3.9: poll tightly while any job is non-terminal so progress bars and
  // the operations-in-flight picture stay live — but only as the FALLBACK:
  // while the SSE stream is delivering, the ticks are the transport.
  const hasActive = rows.some((j) => !isTerminalJobState(j.state));
  useEffect(() => {
    if (!hasActive || liveSource === "sse") return;
    const id = setInterval(fetchAll, 2000);
    return () => clearInterval(id);
  }, [hasActive, liveSource, fetchAll]);

  const fetchDetail = useCallback(async (jobId: string) => {
    try {
      setDetail(await api.getJob(jobId, 0, 200));
    } catch (e) {
      setError(toErrorString(e));
    }
  }, []);

  // Keep the expanded row's detail fresh while it is non-terminal. This poll
  // stays even when SSE is live: the failure report and sample it fetches
  // are not part of the jobs events.
  useEffect(() => {
    if (!expanded) {
      setDetail(null);
      setVerify(null);
      return;
    }
    fetchDetail(expanded);
    const row = rows.find((j) => j.id === expanded);
    if (row && !isTerminalJobState(row.state)) {
      const id = setInterval(() => fetchDetail(expanded), 2000);
      return () => clearInterval(id);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expanded, rows.find((j) => j.id === expanded)?.state]);

  // Verify panel (§3.9): re-run the scope against the live search endpoint
  // and compare with the job's final counts.
  const runVerify = useCallback(async (job: JobInfo) => {
    setVerify({ loading: true, verdict: "", error: "" });
    try {
      const resp = await api.searchTasks({
        queue: job.scope.queue || "all",
        state: job.scope.state,
        q: job.scope.q,
        meta: job.scope.meta,
        page: 1,
        size: 1,
      });
      setVerify({
        loading: false,
        verdict: verifyVerdict(resp.total, resp.truncated, job.scope, job.counts),
        error: "",
      });
    } catch (e) {
      setVerify({ loading: false, verdict: "", error: toErrorString(e) });
    }
  }, []);

  // Auto-verify when a done job is expanded.
  useEffect(() => {
    if (!expanded) return;
    const row = rows.find((j) => j.id === expanded);
    if (row && row.state === "done" && verify === null) {
      runVerify(row);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expanded, rows]);

  const ctl = async (fn: (id: string) => Promise<unknown>, id: string) => {
    try {
      await fn(id);
      setError("");
    } catch (e) {
      setError(toErrorString(e));
    }
    fetchAll();
  };

  return (
    <PageShell
      className="space-y-4"
      title="Operations"
      sub="every bulk verb is a reviewable, query-scoped background job — §4.3"
    >
      {error && (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      {/* Jobs table */}
      <div className="overflow-x-auto rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
        <div className="flex items-baseline gap-2 border-b border-[var(--fc-line2)] px-4 py-2 text-sm font-semibold">
          Bulk jobs
          {/* Transport indicator — same vocabulary as Fleet's coverage line */}
          <span className="text-[10.5px] font-normal text-[var(--fc-ink3)]">
            {liveSource === "sse" ? "live stream" : "polling"}
          </span>
        </div>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-6" />
              <TableHead>Job</TableHead>
              <TableHead>Verb</TableHead>
              <TableHead>Scope</TableHead>
              <TableHead>State</TableHead>
              <TableHead className="text-right">Acted</TableHead>
              <TableHead className="text-right">Skipped</TableHead>
              <TableHead className="text-right">Failed</TableHead>
              <TableHead className="text-right">Throttle</TableHead>
              <TableHead>Actor</TableHead>
              <TableHead>Reason</TableHead>
              <TableHead>Started / Finished</TableHead>
              {!window.READ_ONLY && <TableHead className="text-center">Controls</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.length === 0 ? (
              <TableRow>
                <TableCell
                  colSpan={window.READ_ONLY ? 12 : 13}
                  className="py-10 text-center text-[var(--fc-ink3)]"
                >
                  {loading
                    ? "Loading…"
                    : "No bulk jobs yet. Whole-scope verbs in the Tasks console land here."}
                </TableCell>
              </TableRow>
            ) : (
              rows.map((j) => {
                const progress = jobProgress(j.counts);
                const isOpen = expanded === j.id;
                return (
                  <>
                    <TableRow
                      key={j.id}
                      className="cursor-pointer hover:bg-[var(--fc-raise)]/50"
                      onClick={() => setExpanded(isOpen ? null : j.id)}
                    >
                      <TableCell className="pr-0 text-[var(--fc-ink3)]">
                        {isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                      </TableCell>
                      <TableCell className="font-mono text-xs">{uuidPrefix(j.id)}</TableCell>
                      <TableCell>
                        <span
                          className={cn(
                            "rounded px-1.5 py-0.5 text-[10px] font-bold uppercase",
                            verbChip[j.verb]
                          )}
                        >
                          {j.verb}
                        </span>
                      </TableCell>
                      <TableCell className="max-w-[260px] truncate font-mono text-[11px]" title={scopeLabel(j.scope)}>
                        {scopeLabel(j.scope)}
                      </TableCell>
                      <TableCell>
                        {j.state === "running" || j.state === "previewing" ? (
                          <div className="flex items-center gap-2">
                            <div className="h-1.5 w-16 overflow-hidden rounded bg-[var(--fc-raise)]">
                              <div
                                className="h-full bg-[var(--fc-acc)] transition-all"
                                style={{
                                  width:
                                    j.state === "running" && progress !== null
                                      ? `${Math.round(progress * 100)}%`
                                      : "35%",
                                }}
                              />
                            </div>
                            <span className="font-mono text-[10.5px] tabular-nums">
                              {j.state === "running" && progress !== null
                                ? `${Math.round(progress * 100)}%`
                                : j.state === "previewing"
                                  ? `≥${j.counts.candidates.toLocaleString()}`
                                  : "…"}
                            </span>
                          </div>
                        ) : (
                          <span
                            className={cn(
                              "rounded px-1.5 py-0.5 text-[10px] font-bold uppercase",
                              stateChip[j.state]
                            )}
                            title={j.error || undefined}
                          >
                            {j.state.replace("_", " ")}
                          </span>
                        )}
                      </TableCell>
                      <TableCell className="text-right text-xs tabular-nums">
                        {j.counts.acted.toLocaleString()}
                      </TableCell>
                      <TableCell className="text-right text-xs tabular-nums text-[var(--fc-ink2)]">
                        {j.counts.skipped.toLocaleString()}
                      </TableCell>
                      <TableCell
                        className={cn(
                          "text-right text-xs tabular-nums",
                          j.counts.failed > 0 && "font-semibold text-[var(--fc-crit)]"
                        )}
                      >
                        {j.counts.failed.toLocaleString()}
                      </TableCell>
                      <TableCell className="text-right text-xs tabular-nums">
                        {j.throttle}/s
                      </TableCell>
                      <TableCell className="text-xs">{j.actor}</TableCell>
                      <TableCell
                        className="max-w-[180px] truncate text-xs text-[var(--fc-ink2)]"
                        title={j.reason}
                      >
                        {j.reason}
                      </TableCell>
                      <TableCell className="whitespace-nowrap text-[10.5px] text-[var(--fc-ink3)]">
                        {fmtWhen(j.started_at)}
                        <br />
                        {fmtWhen(j.finished_at)}
                      </TableCell>
                      {!window.READ_ONLY && (
                        <TableCell className="text-center" onClick={(e) => e.stopPropagation()}>
                          <div className="flex items-center justify-center gap-1">
                            {(j.state === "running" || j.state === "previewing") && (
                              <Button
                                size="icon"
                                variant="ghost"
                                className="h-6 w-6"
                                title="Pause between batches"
                                onClick={() => ctl(api.pauseJob, j.id)}
                              >
                                <Pause size={12} />
                              </Button>
                            )}
                            {(j.state === "paused" || j.ctl_pending === "pause") && (
                              <Button
                                size="icon"
                                variant="ghost"
                                className="h-6 w-6"
                                title="Resume"
                                onClick={() => ctl(api.resumeJob, j.id)}
                              >
                                <Play size={12} />
                              </Button>
                            )}
                            {!isTerminalJobState(j.state) && (
                              <Button
                                size="icon"
                                variant="ghost"
                                className="h-6 w-6 text-[var(--fc-crit)]"
                                title="Cancel job"
                                onClick={() => ctl(api.cancelJob, j.id)}
                              >
                                <X size={12} />
                              </Button>
                            )}
                          </div>
                        </TableCell>
                      )}
                    </TableRow>

                    {isOpen && (
                      <TableRow key={`${j.id}-detail`}>
                        <TableCell colSpan={window.READ_ONLY ? 12 : 13} className="bg-[var(--fc-bg)] p-0">
                          <div className="grid gap-4 p-4 md:grid-cols-2">
                            {/* Per-item failure report + sample */}
                            <div className="space-y-3">
                              <div>
                                <div className="mb-1 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
                                  per-item failures ({detail?.failures_total ?? 0}
                                  {j.failures_overflow > 0 &&
                                    ` recorded + ${j.failures_overflow.toLocaleString()} beyond the 10k cap`}
                                  )
                                </div>
                                {detail && detail.failures.length > 0 ? (
                                  <div className="max-h-48 overflow-y-auto rounded border border-[var(--fc-line)] bg-[var(--fc-panel)] p-2 font-mono text-[11px] leading-relaxed">
                                    {detail.failures.map((f, i) => (
                                      <div key={`${f.id}-${i}`}>
                                        <span className="text-[var(--fc-ink3)]">
                                          {uuidPrefix(f.id)}… {f.queue} —{" "}
                                        </span>
                                        <span className="text-[var(--fc-crit)]">{f.error}</span>
                                      </div>
                                    ))}
                                    {detail.failures_total > detail.failures.length && (
                                      <div className="text-[var(--fc-ink3)]">
                                        (+{detail.failures_total - detail.failures.length} more)
                                      </div>
                                    )}
                                  </div>
                                ) : (
                                  <div className="text-xs text-[var(--fc-ink3)]">
                                    No per-item failures recorded.
                                  </div>
                                )}
                              </div>
                              <div>
                                <div className="mb-1 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
                                  preview sample ({detail?.sample.length ?? 0} of{" "}
                                  {j.counts.candidates.toLocaleString()} candidates)
                                </div>
                                {detail && detail.sample.length > 0 ? (
                                  <div className="max-h-48 overflow-y-auto rounded border border-[var(--fc-line)] bg-[var(--fc-panel)] p-2 font-mono text-[11px] leading-relaxed">
                                    {detail.sample.map((s) => (
                                      <div key={s.id} className="truncate" title={s.payload}>
                                        <span className="text-[var(--fc-ink3)]">
                                          {uuidPrefix(s.id)}…
                                        </span>{" "}
                                        <span className="font-medium">{s.type}</span>{" "}
                                        <span className="text-[var(--fc-ink2)]">{s.payload}</span>
                                      </div>
                                    ))}
                                  </div>
                                ) : (
                                  <div className="text-xs text-[var(--fc-ink3)]">No sample.</div>
                                )}
                              </div>
                            </div>

                            {/* Verify panel (done jobs) */}
                            <div>
                              <div className="mb-1 flex items-center gap-2 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
                                verify — scope re-counted live
                                {j.state === "done" && (
                                  <Button
                                    size="sm"
                                    variant="outline"
                                    className="h-5 px-2 text-[10px]"
                                    onClick={() => runVerify(j)}
                                  >
                                    Re-check
                                  </Button>
                                )}
                              </div>
                              {j.state !== "done" ? (
                                <div className="text-xs text-[var(--fc-ink3)]">
                                  Verification runs once the job is done — remediation ends with
                                  evidence, not a toast.
                                </div>
                              ) : verify?.loading ? (
                                <div className="text-xs text-[var(--fc-ink3)]">Re-counting…</div>
                              ) : verify?.error ? (
                                <div className="text-xs text-[var(--fc-crit)]">{verify.error}</div>
                              ) : verify?.verdict ? (
                                <div
                                  className={cn(
                                    "rounded border p-2.5 text-xs leading-relaxed",
                                    verify.verdict.includes("drained")
                                      ? "border-[var(--fc-good)]/40 bg-[var(--fc-good-bg)] text-[var(--fc-good)]"
                                      : "border-[var(--fc-warn)]/40 bg-[var(--fc-warn-bg)] text-[var(--fc-ink)]"
                                  )}
                                >
                                  {verify.verdict}
                                </div>
                              ) : null}
                              {/* Burn-down of the scoped state (§4.3 step 6) */}
                              {j.state === "done" && <VerifyBurnDown job={j} />}
                              {j.error && (
                                <div className="mt-2 rounded border border-[var(--fc-crit)]/40 bg-[var(--fc-crit-bg)] p-2 text-xs text-[var(--fc-crit)]">
                                  {j.error}
                                </div>
                              )}
                            </div>
                          </div>
                        </TableCell>
                      </TableRow>
                    )}
                  </>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>

      {/* Audit log */}
      <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
        <div className="border-b border-[var(--fc-line2)] px-4 py-2 text-sm font-semibold">
          Audit log{" "}
          <span className="text-xs font-normal text-[var(--fc-ink3)]">
            capped stream · actor · verb · scope · counts · reason
          </span>
        </div>
        <div className="max-h-72 overflow-y-auto">
          {audit.length === 0 ? (
            <div className="px-4 py-6 text-center text-xs text-[var(--fc-ink3)]">
              No audit entries yet.
            </div>
          ) : (
            audit.map((e) => (
              <div
                key={e.id}
                className="flex flex-wrap items-baseline gap-x-2 border-b border-[var(--fc-line2)] px-4 py-1.5 text-xs last:border-0"
              >
                <span className="font-semibold text-[var(--fc-acc)]">{e.actor}</span>
                <span className="text-[var(--fc-ink2)]">
                  {e.event === "job_finished"
                    ? `${e.verb} finished — ${e.acted.toLocaleString()} acted, ${e.skipped.toLocaleString()} skipped, ${e.failed.toLocaleString()} failed (preview ${e.preview_count.toLocaleString()})`
                    : `${e.verb} job created`}
                </span>
                <span className="font-mono text-[10.5px] text-[var(--fc-ink3)]">
                  {scopeLabel(e.scope)}
                </span>
                {e.reason && <span className="italic text-[var(--fc-ink3)]">"{e.reason}"</span>}
                <span className="ml-auto font-mono text-[10.5px] text-[var(--fc-ink3)]">
                  {uuidPrefix(e.job_id)} · {fmtWhen(e.at)}
                </span>
              </div>
            ))
          )}
        </div>
      </div>

      {/* System activity — the console's own background maintenance */}
      <SystemActivityPanel />
    </PageShell>
  );
}
