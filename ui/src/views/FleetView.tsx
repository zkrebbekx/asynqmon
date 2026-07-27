// Fleet landing (build contract §3.1): "is anything wrong right now, and
// where?" — rendered entirely from the fleet stats cache over one SSE stream
// (polling fallback) plus the §5.8 ring-buffer series (phase 10): KPI
// sparklines + Δ/min, attention-rail 30m sparks, learning badges, and the
// Region D failure pulse (24h fleet failed_delta bars with deploy markers,
// top-5 signature strip beside it).
//
// Deliberately absent until their phases land (no dead placeholders):
//   - operations-in-flight + audit rail (job runner, phase 5)

import { ReactNode, useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import prettyBytes from "pretty-bytes";
import { useFleetEvents } from "../hooks/useFleetEvents";
import { FleetStats, AttentionFinding, LearningDetector } from "../api-fleet";
import { ErrorSignatureRow, listErrorSignatures } from "../api-errors";
import {
  DeployMarker,
  SeriesResponse,
  SeriesSpec,
  createMarker,
  getSeriesBatch,
  listMarkers,
} from "../api-series";
import { paths, attentionFindingPath } from "../paths";
import { timeAgo } from "../utils";
import { formatErrorRate, formatFindingValue, severityTone } from "../lib/fleet";
import { formatRate, learningRemaining, ratePerMinute } from "../lib/series";
import { formatSigCount, trendTone } from "../lib/errsig";
import { FcChip, MicroLabel, SEV_STRIPE } from "../components/FleetBits";
import PageShell from "../components/PageShell";
import { RingBars, Sparkline } from "../components/charts";
import { cn } from "../lib/utils";

const fmt = (n: number | undefined | null) =>
  n === undefined || n === null || !Number.isFinite(n) ? "—" : n.toLocaleString("en-US");

// Data older than this renders the stale banner (§3.1 error state).
const STALE_FLEET_MS = 30_000;

// Series refresh cadence: the hot ring gains a slot every 30s — polling
// faster reads the same bytes.
const SERIES_REFRESH_MS = 30_000;

// KPI tiles + rail sparks show the last 30 minutes (60 hot slots, §3.1).
const SPARK_POINTS = 60;

/**************************************************************
              Fleet series + markers (one batch)
 **************************************************************/

const seriesKeyOf = (scope: string, metric: string, window: string) =>
  `${scope}|${metric}|${window}`;

// KPI strip series: always requested.
const KPI_SPECS: SeriesSpec[] = [
  { scope: "fleet", metric: "pending", window: "3h", points: SPARK_POINTS },
  { scope: "fleet", metric: "active", window: "3h", points: SPARK_POINTS },
  { scope: "fleet", metric: "retry", window: "3h", points: SPARK_POINTS },
  { scope: "fleet", metric: "archived", window: "3h", points: SPARK_POINTS },
  // Region D: 24h fleet failure pulse (rollup merged with hot).
  { scope: "fleet", metric: "failed_delta", window: "24h" },
];

// Batch cap (server enforces 60): KPI specs + up to (60 − KPI) finding sparks.
const MAX_FINDING_SPECS = 60 - KPI_SPECS.length;

// useFleetSeries fetches the KPI + finding series in ONE batch call per
// refresh, plus the deploy markers. Finding specs follow the attention rail.
function useFleetSeries(findings: AttentionFinding[] | undefined) {
  const [series, setSeries] = useState<Record<string, SeriesResponse>>({});
  const [markers, setMarkers] = useState<DeployMarker[]>([]);

  // Stable identity of the rail's series refs so the effect refires only
  // when the set of sparks actually changes.
  const findingKey = useMemo(() => {
    const refs = new Set<string>();
    for (const f of findings ?? []) {
      if (f.series_scope && f.series_metric) refs.add(`${f.series_scope}|${f.series_metric}`);
    }
    return Array.from(refs).sort().slice(0, MAX_FINDING_SPECS).join(",");
  }, [findings]);

  const refresh = useCallback(async () => {
    const specs: SeriesSpec[] = [...KPI_SPECS];
    for (const ref of findingKey === "" ? [] : findingKey.split(",")) {
      const [scope, metric] = ref.split("|");
      specs.push({ scope, metric, window: "3h", points: SPARK_POINTS });
    }
    try {
      const [batch, mk] = await Promise.all([getSeriesBatch(specs), listMarkers("24h")]);
      const next: Record<string, SeriesResponse> = {};
      for (const s of batch.series) next[seriesKeyOf(s.scope, s.metric, s.window)] = s;
      setSeries(next);
      setMarkers(mk.markers);
    } catch {
      // Endpoint absent/unhealthy: keep the last data; tiles degrade to
      // numbers-only — absent sparks, never fabricated ones.
    }
  }, [findingKey]);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, SERIES_REFRESH_MS);
    return () => clearInterval(id);
  }, [refresh]);

  return { series, markers, refreshMarkers: refresh };
}

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
  // Phase 10: 30m trend from the §5.8 fleet series (absent = numbers-only).
  spark?: SeriesResponse;
  // Rendered beside the spark: "+340/min" / "flat" (lib/series formatRate).
  delta?: string;
}

function KpiTile({ label, value, to, sub, badge, gauge, spark, delta }: TileProps) {
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
      {spark && (
        <div className="mt-1.5 flex items-end gap-2">
          <Sparkline points={spark.points} width={84} height={20} label={`${label} — last 30m`} />
          {delta !== undefined && (
            <span className="pb-px font-mono text-[10.5px] tabular-nums text-[var(--fc-ink2)]">
              {delta}
            </span>
          )}
        </div>
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

function FindingRow({
  finding,
  spark,
}: {
  finding: AttentionFinding;
  // The finding's 30m series (§3.1: each row carries a 30-min sparkline).
  spark?: SeriesResponse;
}) {
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
        {spark && (
          <Sparkline
            points={spark.points}
            width={72}
            height={20}
            tone={tone === "crit" ? "crit" : tone === "warn" ? "warn" : "mut"}
            label={`${finding.queue} ${finding.series_metric ?? ""} — last 30m`}
          />
        )}
        <div className="text-right">
          <div className="font-mono text-[13px] font-semibold tabular-nums text-[var(--fc-ink)]">
            {formatFindingValue(finding.detector, finding.value)}
          </div>
          <div className="text-[10.5px] text-[var(--fc-ink3)]">since {timeAgo(finding.since)}</div>
        </div>
        <Link
          to={attentionFindingPath(finding.suggested_query)}
          title={`Opens with the system-written query: ${finding.suggested_query}`}
          className="rounded-md border border-[var(--fc-acc)] bg-[var(--fc-acc)] px-2 py-0.5 text-[11px] font-semibold text-[var(--fc-acc-fg)] no-underline hover:opacity-90"
        >
          Open →
        </Link>
      </div>
    </div>
  );
}

/**************************************************************
                Learning badges (§3.1, phase 10)
 **************************************************************/

// LearningBadges renders "learning baseline — active in ~Xh" chips for the
// gated detectors (report.learning). Silent once everything is live.
function LearningBadges({
  learning,
  now,
}: {
  learning: LearningDetector[] | undefined;
  now: number;
}) {
  if (!learning || learning.length === 0) return null;
  return (
    <>
      {learning.map((l) => {
        const remaining = learningRemaining(l.until, now);
        if (remaining === "") return null;
        return (
          <FcChip
            key={l.detector}
            tone="mut"
            title={`${l.detector} is building its baseline from the ring buffers and goes live in ${remaining}`}
          >
            {l.detector.replace(/_/g, " ")} · learning — active in {remaining}
          </FcChip>
        );
      })}
    </>
  );
}

/**************************************************************
        Region D — failure pulse (24h bars + deploy markers
                    + top-5 signature strip beside)
 **************************************************************/

// AddMarkerButton: paint a deploy line on every chart (§5.14). Hidden in
// read-only mode; the actor is resolved server-side, the write audit-logged.
function AddMarkerButton({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false);
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(false);

  if (window.READ_ONLY) return null;
  if (!open) {
    return (
      <button
        onClick={() => setOpen(true)}
        className="rounded border border-[var(--fc-line)] bg-[var(--fc-raise)] px-2 py-0.5 text-[10.5px] font-semibold text-[var(--fc-ink2)] hover:text-[var(--fc-ink)]"
        title="Drop a deploy marker at the current time — rendered on every chart, audit-logged"
      >
        + mark deploy
      </button>
    );
  }
  const submit = async () => {
    const trimmed = label.trim();
    if (trimmed === "" || busy) return;
    setBusy(true);
    try {
      await createMarker(trimmed);
      setLabel("");
      setOpen(false);
      onCreated();
    } catch {
      // Leave the input open so the label is not lost; the next submit retries.
    } finally {
      setBusy(false);
    }
  };
  return (
    <span className="flex items-center gap-1">
      <input
        autoFocus
        value={label}
        onChange={(e) => setLabel(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") submit();
          if (e.key === "Escape") setOpen(false);
        }}
        placeholder="deploy v2.4.1"
        className="w-36 rounded border border-[var(--fc-line)] bg-[var(--fc-panel)] px-1.5 py-0.5 font-mono text-[11px] text-[var(--fc-ink)] outline-none placeholder:text-[var(--fc-ink3)]"
      />
      <button
        onClick={submit}
        disabled={busy || label.trim() === ""}
        className="rounded border border-[var(--fc-acc)] bg-[var(--fc-acc)] px-1.5 py-0.5 text-[10.5px] font-semibold text-[var(--fc-acc-fg)] disabled:opacity-50"
      >
        mark
      </button>
    </span>
  );
}

// Region D (§3.1): 24h fleet failed_delta bars (daily-counter magnitude via
// the ring buffers — hot ring merged over the un-flushed rollup hours) with
// deploy-marker overlays, and the phase-9 top-5 signature strip beside it.
function FailurePulse({
  pulse,
  markers,
  onMarkerCreated,
}: {
  pulse?: SeriesResponse;
  markers: DeployMarker[];
  onMarkerCreated: () => void;
}) {
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

  const hourFmt = (t: number) =>
    new Date(t * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  return (
    <div className="mt-3 rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
      <div className="flex items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
        Failure pulse — 24h
        <span className="font-normal text-[var(--fc-ink3)]">
          fleet failures/hr from daily counters · signatures: archived tail exact, retry sampled
        </span>
        <span className="flex-1" />
        {trimmedCount > 0 && (
          <FcChip tone="warn">
            lower bounds — {trimmedCount} queue{trimmedCount > 1 ? "s" : ""} at archive cap
          </FcChip>
        )}
        <AddMarkerButton onCreated={onMarkerCreated} />
        <Link to={paths().ERRORS} className="text-[11px] font-normal text-[var(--fc-acc)] no-underline hover:underline">
          all signatures →
        </Link>
      </div>
      <div className="grid gap-0 md:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        {/* 24h bar chart + deploy markers */}
        <div className="px-3 py-2">
          {pulse ? (
            <>
              <RingBars
                points={pulse.points}
                markers={markers}
                height={84}
                tone="crit"
                titleFor={(p) =>
                  `${(p.v ?? 0).toLocaleString("en-US")} failed · hour of ${hourFmt(p.t)}`
                }
              />
              <div className="mt-1 flex justify-between text-[10px] text-[var(--fc-ink3)]">
                <span>{pulse.points.length > 0 ? hourFmt(pulse.points[0].t) : ""}</span>
                <span>
                  {markers.length > 0 &&
                    `${markers.length} deploy marker${markers.length > 1 ? "s" : ""} · `}
                  now
                </span>
              </div>
            </>
          ) : (
            <div className="py-6 text-center text-[11px] text-[var(--fc-ink3)]">
              Failure series unavailable — the ring-buffer sampler hasn’t published yet.
            </div>
          )}
        </div>
        {/* Top-5 signature strip beside the chart */}
        <div className="border-t border-[var(--fc-line2)] md:border-l md:border-t-0">
          {!sigs || sigs.length === 0 ? (
            <div className="px-3 py-6 text-center text-[11px] text-[var(--fc-ink3)]">
              No error signatures indexed.
            </div>
          ) : (
            sigs.map((s) => (
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
            ))
          )}
        </div>
      </div>
    </div>
  );
}

/**************************************************************
                          FleetView
 **************************************************************/

export default function FleetView() {
  const { overview, attention, source, updatedAt, error } = useFleetEvents();
  const appPaths = paths();

  // §5.8 series (KPI sparks, rail sparks, failure pulse) + deploy markers.
  const { series, markers, refreshMarkers } = useFleetSeries(attention?.findings);
  const fleetSpark = (metric: string) => series[seriesKeyOf("fleet", metric, "3h")];
  const sparkFor = (f: AttentionFinding) =>
    f.series_scope && f.series_metric
      ? series[seriesKeyOf(f.series_scope, f.series_metric, "3h")]
      : undefined;
  // Δ rates from the fitted slope of the 30m spark (lib/series).
  const rate = (metric: string, perHour = false) => {
    const s = fleetSpark(metric);
    if (!s) return undefined;
    const perMin = ratePerMinute(s.points);
    if (perMin === null) return undefined;
    return perHour ? formatRate(perMin * 60, "hr") : formatRate(perMin, "min");
  };

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
      <PageShell>
        <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)] p-8 text-center">
          {error ? (
            <>
              <div className="text-sm font-semibold text-[var(--fc-ink)]">
                Overview data unavailable
              </div>
              <p className="mx-auto mt-2 max-w-md text-xs text-[var(--fc-ink2)]">
                The fleet stats endpoints are not answering ({error}). The stats
                engine may not be deployed on this server yet — queue and task
                views in the nav keep working against the classic endpoints.
              </p>
            </>
          ) : (
            <div className="text-sm text-[var(--fc-ink3)]">Waiting for overview data…</div>
          )}
        </div>
      </PageShell>
    );
  }

  const F: FleetStats = overview.fleet;
  const cov = overview.coverage;
  const busyPct =
    F.workers_total > 0 ? Math.round((F.workers_busy / F.workers_total) * 100) : 0;
  const memMax = F.redis_memory_max_bytes;
  const memUsed = F.redis_memory_used_bytes;

  return (
    <PageShell>
      {/* Stale-data banner (§3.1 error state) */}
      {stale && (
        <div className="mb-3 flex items-center gap-2 rounded-md border border-[var(--fc-warn)]/45 bg-[var(--fc-warn-bg)] px-3 py-2 text-xs text-[var(--fc-warn)]">
          <span className="font-semibold">
            Overview data is {Math.round((now - updatedAt) / 1000)}s stale
          </span>
          <span className="text-[var(--fc-ink2)]">
            — stream disconnected or stats poller unhealthy; retrying in the background.
          </span>
        </div>
      )}

      {/* Region A — KPI strip, 8 tiles, each a click-through; the four
          state tiles carry 30m sparklines + fitted Δ rates (phase 10) */}
      <div className="mb-1.5 grid grid-cols-3 gap-2 xl:grid-cols-9">
        <KpiTile
          label="pending"
          value={fmt(F.pending)}
          to={appPaths.TASKS}
          sub=""
          spark={fleetSpark("pending")}
          delta={rate("pending")}
        />
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
          spark={fleetSpark("active")}
        />
        <KpiTile
          label="retry"
          value={fmt(F.retry)}
          to={`${appPaths.TASKS}?state=retry`}
          sub=""
          spark={fleetSpark("retry")}
          delta={rate("retry")}
        />
        <KpiTile
          label="archived"
          value={fmt(F.archived)}
          to={`${appPaths.TASKS}?state=archived`}
          sub={`${fmt(F.failed_today)} failed today · ${formatErrorRate(F.error_rate)}`}
          spark={fleetSpark("archived")}
          delta={rate("archived", true)}
        />
        {/* No sparkline: completed depth is not in the series metric set
            (§5.8) — the tile shows the live count and today's throughput. */}
        <KpiTile
          label="completed"
          value={fmt(F.completed)}
          to={`${appPaths.TASKS}?state=completed`}
          sub={`${fmt(F.processed_today)} processed today`}
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
        <div className="flex flex-wrap items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
          Needs attention
          {attention && (
            <span className="font-normal text-[var(--fc-ink3)]">
              {attention.detectors_live} detectors live
              {attention.detectors_learning > 0 &&
                ` · ${attention.detectors_learning} learning`}
            </span>
          )}
          <LearningBadges learning={attention?.learning} now={now} />
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
              All healthy — {fmt(F.queues_total)} queues quiet.
            </div>
            <div className="mt-1 text-[11px] text-[var(--fc-ink3)]">
              {attention.detectors_live} detectors live
              {attention.detectors_learning > 0 && attention.learning?.length
                ? ` · ${attention.detectors_learning} learning (${attention.learning
                    .map((l) => learningRemaining(l.until, now))
                    .filter((s) => s !== "")
                    .join(", ")} remaining)`
                : attention.detectors_learning > 0
                  ? ` · ${attention.detectors_learning} learning`
                  : ""}
            </div>
          </div>
        ) : (
          attention.findings.map((f) => (
            <FindingRow key={`${f.queue}:${f.detector}`} finding={f} spark={sparkFor(f)} />
          ))
        )}
      </div>

      {/* Region D — failure pulse: 24h bars + deploy markers + signatures */}
      <FailurePulse
        pulse={series[seriesKeyOf("fleet", "failed_delta", "24h")]}
        markers={markers}
        onMarkerCreated={refreshMarkers}
      />
    </PageShell>
  );
}
