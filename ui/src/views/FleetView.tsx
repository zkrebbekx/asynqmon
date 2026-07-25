// Fleet landing (build contract §3.1, honest v1): "is anything wrong right
// now, and where?" — rendered entirely from the fleet stats cache over one
// SSE stream (polling fallback). Never enumerates queues from the browser.
//
// Deliberately absent until their phases land (no dead placeholders):
//   - deltas / sparklines / retry-ETA mass (ring buffers, phase 10)
//   - operations-in-flight + audit rail (job runner, phase 5)
//   - the failure pulse's 24h bar chart + deploy markers (ring buffers,
//     phase 10) — the top-signatures strip below is the phase-9 lite cut

import { ReactNode, useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import prettyBytes from "pretty-bytes";
import { useFleetEvents } from "../hooks/useFleetEvents";
import { FleetStats, AttentionFinding } from "../api-fleet";
import { ErrorSignatureRow, listErrorSignatures } from "../api-errors";
import { paths, attentionFindingPath } from "../paths";
import { timeAgo } from "../utils";
import { formatErrorRate, formatFindingValue, severityTone } from "../lib/fleet";
import { formatSigCount, trendTone } from "../lib/errsig";
import { FcChip, MicroLabel, SEV_STRIPE } from "../components/FleetBits";
import { cn } from "../lib/utils";

const fmt = (n: number | undefined | null) =>
  n === undefined || n === null || !Number.isFinite(n) ? "—" : n.toLocaleString("en-US");

// Data older than this renders the stale banner (§3.1 error state).
const STALE_FLEET_MS = 30_000;

/**************************************************************
                          KPI tile
 **************************************************************/

interface TileProps {
  label: string;
  value: string;
  to: string;
  sub?: ReactNode;
  badge?: ReactNode;
  gauge?: number | null; // 0..1 fill; null hides the bar
}

function KpiTile({ label, value, to, sub, badge, gauge }: TileProps) {
  return (
    <Link
      to={to}
      className="relative block rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)] px-3 pb-2.5 pt-2.5 no-underline transition-colors hover:border-[var(--fc-ink3)]"
    >
      <MicroLabel>{label}</MicroLabel>
      {badge && <span className="absolute right-2.5 top-2">{badge}</span>}
      <div className="mt-[3px] font-mono text-[19px] font-semibold tabular-nums tracking-[-0.01em] text-[var(--fc-ink)]">
        {value}
      </div>
      {sub !== undefined && (
        <div className="mt-px min-h-[16px] text-[11px] text-[var(--fc-ink2)]">{sub}</div>
      )}
      {gauge !== undefined && gauge !== null && (
        <div className="mt-2 h-[5px] overflow-hidden rounded-[3px] bg-[var(--fc-raise)]">
          <i
            className={cn(
              "block h-full",
              gauge >= 0.9
                ? "bg-[var(--fc-crit)]"
                : gauge >= 0.75
                  ? "bg-[var(--fc-warn)]"
                  : "bg-[var(--fc-acc)]"
            )}
            style={{ width: `${Math.min(100, Math.max(0, gauge * 100))}%` }}
          />
        </div>
      )}
    </Link>
  );
}

/**************************************************************
                       Attention rail row
 **************************************************************/

function FindingRow({ finding }: { finding: AttentionFinding }) {
  const tone = severityTone(finding.severity);
  const chips = Array.isArray(finding.chips) ? finding.chips : [];
  return (
    <div className="grid grid-cols-[14px_1fr_auto] items-center gap-2.5 border-b border-[var(--fc-line2)] px-3 py-2.5 pl-2 last:border-b-0 hover:bg-[var(--fc-raise)]">
      <span
        aria-label={`severity ${finding.severity}`}
        className={cn(
          "h-[34px] w-[3px] justify-self-center rounded-[2px]",
          SEV_STRIPE[tone] ?? SEV_STRIPE.info
        )}
      />
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-1.5 text-[13px] font-semibold text-[var(--fc-ink)]">
          <span className="font-mono">{finding.queue}</span>
          {chips.map((c) => (
            <FcChip key={c} tone={tone}>
              {c}
            </FcChip>
          ))}
        </div>
        <div className="mt-px truncate text-xs text-[var(--fc-ink2)]" title={finding.sentence}>
          {finding.sentence}
        </div>
      </div>
      <div className="flex items-center gap-2.5">
        <div className="text-right">
          <div className="font-mono text-[13px] font-semibold tabular-nums text-[var(--fc-ink)]">
            {formatFindingValue(finding.detector, finding.value)}
          </div>
          <div className="text-[10.5px] text-[var(--fc-ink3)]">since {timeAgo(finding.since)}</div>
        </div>
        <Link
          to={attentionFindingPath(finding.suggested_query)}
          title={`Opens with the system-written query: ${finding.suggested_query}`}
          className="rounded-md border border-[var(--fc-acc)] bg-[var(--fc-acc)] px-2 py-0.5 text-[11px] font-semibold text-[#0B1520] no-underline hover:opacity-90"
        >
          Open →
        </Link>
      </div>
    </div>
  );
}

/**************************************************************
            Region D lite — failure pulse (top signatures)
 **************************************************************/

// Top-5 error signatures from the phase-9 index (§3.1 Region D, lite cut:
// the 24h bar chart + deploy markers need ring buffers, phase 10). Each row
// clicks through to /errors pre-selected. Renders nothing while the index
// is empty or the endpoint is absent — no dead panel on healthy fleets.
function FailurePulse() {
  const [sigs, setSigs] = useState<ErrorSignatureRow[] | null>(null);
  const [trimmedCount, setTrimmedCount] = useState(0);

  const fetchSigs = useCallback(async () => {
    try {
      const r = await listErrorSignatures({ limit: 5, sort: "count" });
      setSigs(r.signatures);
      setTrimmedCount(r.honesty.trimmed_queues.length);
    } catch {
      setSigs(null); // endpoint absent/unhealthy: render nothing, not zeros
    }
  }, []);

  useEffect(() => {
    fetchSigs();
    const id = setInterval(fetchSigs, 30_000);
    return () => clearInterval(id);
  }, [fetchSigs]);

  if (!sigs || sigs.length === 0) return null;

  return (
    <div className="mt-3 rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
      <div className="flex items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
        Failure pulse — top signatures
        <span className="font-normal text-[var(--fc-ink3)]">
          archived tail exact · retry sampled · 24h chart arrives with ring buffers
        </span>
        <span className="flex-1" />
        {trimmedCount > 0 && (
          <FcChip tone="warn">
            lower bounds — {trimmedCount} queue{trimmedCount > 1 ? "s" : ""} at archive cap
          </FcChip>
        )}
        <Link to={paths().ERRORS} className="text-[11px] font-normal text-[var(--fc-acc)] no-underline hover:underline">
          all signatures →
        </Link>
      </div>
      {sigs.map((s) => (
        <Link
          key={s.sig}
          to={`${paths().ERRORS}?sig=${encodeURIComponent(s.sig)}`}
          className="flex items-center gap-2.5 border-b border-[var(--fc-line2)] px-3 py-1.5 no-underline last:border-b-0 hover:bg-[var(--fc-raise)]"
        >
          <FcChip tone={trendTone(s.trend)}>{s.trend}</FcChip>
          <code className="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-nowrap font-mono text-[11.5px] text-[var(--fc-ink)]">
            {s.template}
          </code>
          <b className="font-mono text-xs tabular-nums text-[var(--fc-ink)]">
            {formatSigCount(s.count, s.count_is_lower_bound)}
          </b>
        </Link>
      ))}
    </div>
  );
}

/**************************************************************
                          FleetView
 **************************************************************/

export default function FleetView() {
  const { overview, attention, source, updatedAt, error } = useFleetEvents();
  const appPaths = paths();

  // Re-render each second so freshness labels and the stale banner track time.
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  const stale = updatedAt > 0 && now - updatedAt > STALE_FLEET_MS;

  // Error state: nothing ever received and the transport is failing. We show
  // the honest empty state — no numbers are better than invented ones (§2).
  if (!overview) {
    return (
      <div className="mx-auto max-w-[1560px] px-4 py-4">
        <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)] p-8 text-center">
          {error ? (
            <>
              <div className="text-sm font-semibold text-[var(--fc-ink)]">
                Fleet data unavailable
              </div>
              <p className="mx-auto mt-2 max-w-md text-xs text-[var(--fc-ink2)]">
                The fleet stats endpoints are not answering ({error}). The stats
                engine may not be deployed on this server yet — queue and task
                views in the nav keep working against the classic endpoints.
              </p>
            </>
          ) : (
            <div className="text-sm text-[var(--fc-ink3)]">Waiting for fleet data…</div>
          )}
        </div>
      </div>
    );
  }

  const F: FleetStats = overview.fleet;
  const cov = overview.coverage;
  const busyPct =
    F.workers_total > 0 ? Math.round((F.workers_busy / F.workers_total) * 100) : 0;
  const memMax = F.redis_memory_max_bytes;
  const memUsed = F.redis_memory_used_bytes;

  return (
    <div className="mx-auto max-w-[1560px] px-4 py-4">
      {/* Stale-data banner (§3.1 error state) */}
      {stale && (
        <div className="mb-3 flex items-center gap-2 rounded-md border border-[var(--fc-warn)]/45 bg-[var(--fc-warn-bg)] px-3 py-2 text-xs text-[var(--fc-warn)]">
          <span className="font-semibold">
            Fleet data is {Math.round((now - updatedAt) / 1000)}s stale
          </span>
          <span className="text-[var(--fc-ink2)]">
            — stream disconnected or stats poller unhealthy; retrying in the background.
          </span>
        </div>
      )}

      {/* Region A — KPI strip, 8 tiles, each a click-through */}
      <div className="mb-1.5 grid grid-cols-4 gap-2 xl:grid-cols-8">
        <KpiTile label="pending" value={fmt(F.pending)} to={appPaths.TASKS} sub="" />
        <KpiTile
          label="active"
          value={fmt(F.active)}
          to={`${appPaths.TASKS}?state=active`}
          badge={
            F.orphan_candidates > 0 ? (
              <FcChip tone="warn">orphans {fmt(F.orphan_candidates)}</FcChip>
            ) : undefined
          }
          sub={`of ${fmt(F.workers_total)} slots`}
        />
        <KpiTile label="retry" value={fmt(F.retry)} to={`${appPaths.TASKS}?state=retry`} sub="" />
        <KpiTile
          label="archived"
          value={fmt(F.archived)}
          to={`${appPaths.TASKS}?state=archived`}
          sub={`${fmt(F.failed_today)} failed today · ${formatErrorRate(F.error_rate)}`}
        />
        <KpiTile
          label="scheduled"
          value={fmt(F.scheduled)}
          to={`${appPaths.TASKS}?state=scheduled`}
          sub={
            F.past_due_scheduled > 0 ? (
              <span className="font-semibold text-[var(--fc-crit)]">
                past-due: {fmt(F.past_due_scheduled)}
              </span>
            ) : (
              "none past-due"
            )
          }
        />
        <KpiTile
          label="workers"
          value={`${fmt(F.servers)} srv`}
          to={appPaths.SERVERS}
          sub={`${fmt(F.workers_total)} slots · ${busyPct}% busy`}
        />
        <KpiTile
          label="queues"
          value={fmt(F.queues_total)}
          to={appPaths.QUEUES}
          sub={
            <>
              {fmt(F.paused_queues)} paused ·{" "}
              <span
                className={
                  F.zero_consumer_queues > 0
                    ? "font-semibold text-[var(--fc-crit)]"
                    : undefined
                }
              >
                {fmt(F.zero_consumer_queues)} zero-consumer
              </span>
            </>
          }
        />
        <KpiTile
          label="redis"
          value={
            memMax > 0
              ? `${prettyBytes(memUsed)} / ${prettyBytes(memMax)}`
              : prettyBytes(memUsed)
          }
          to={appPaths.REDIS}
          sub={memMax > 0 ? undefined : "no maxmemory limit"}
          gauge={memMax > 0 ? memUsed / memMax : null}
        />
      </div>

      {/* Coverage stamp (honest numbers, §5.1) */}
      <div className="mb-3 px-0.5 text-[10.5px] text-[var(--fc-ink3)]">
        totals cover {cov ? `${cov.refreshed_pct_5m}%` : "—"} of queues refreshed &lt;5m
        {cov ? ` · tier ${cov.tier}` : ""} · processed today {fmt(F.processed_today)} ·{" "}
        {source === "sse" ? "live stream" : "polling"} · updated{" "}
        {updatedAt > 0 ? `${Math.max(0, Math.round((now - updatedAt) / 1000))}s ago` : "never"}
      </div>

      {/* Region B — Needs attention rail */}
      <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
        <div className="flex items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
          Needs attention
          {attention && (
            <span className="font-normal text-[var(--fc-ink3)]">
              {attention.detectors_live} detectors live
              {attention.detectors_learning > 0 &&
                ` · ${attention.detectors_learning} learning`}
            </span>
          )}
          <span className="flex-1" />
          <span className="text-[10.5px] font-normal text-[var(--fc-ink3)]">
            ranked · auto-clearing
          </span>
        </div>

        {!attention ? (
          <div className="px-3 py-6 text-center text-xs text-[var(--fc-ink3)]">
            Attention feed unavailable — the detector engine hasn’t reported yet.
          </div>
        ) : attention.findings.length === 0 ? (
          <div className="px-3 py-8 text-center">
            <div className="text-sm font-semibold text-[var(--fc-good)]">
              Fleet healthy — {fmt(F.queues_total)} queues quiet.
            </div>
            <div className="mt-1 text-[11px] text-[var(--fc-ink3)]">
              {attention.detectors_live} detectors live
              {attention.detectors_learning > 0 &&
                ` · ${attention.detectors_learning} learning`}
            </div>
          </div>
        ) : (
          attention.findings.map((f) => (
            <FindingRow key={`${f.queue}:${f.detector}`} finding={f} />
          ))
        )}
      </div>

      {/* Region D lite — failure pulse (phase 9; chart lands in phase 10) */}
      <FailurePulse />
    </div>
  );
}
