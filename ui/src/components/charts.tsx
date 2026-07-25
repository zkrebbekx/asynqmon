// Shared chart primitives for the §5.8 ring-buffer series (phase 10):
//
//   <Sparkline>  — single-series inline trend: 1.5px line, ~10% area wash,
//                  endpoint dot with a surface ring. Null-gap aware: a
//                  no-sample slot BREAKS the path — it never renders as zero.
//   <RingBars>   — delta-series bars (failure pulse): thin columns, 2px
//                  surface gaps, rounded data-end/square baseline, deploy
//                  marker overlays, per-bar native tooltips.
//   <BurnDown>   — verify-panel drain chart: the observed line + a DASHED
//                  projection segment to the zero crossing (dashed = not
//                  data), endpoint dot.
//   <LazyMount>  — IntersectionObserver gate for viewport-only rendering
//                  (§3.2: directory sparklines render only for visible rows).
//
// All colors are --fc-* tokens so both themes validate; text never wears the
// series color; single-series charts carry no legend (the label beside them
// names the series — dataviz method).

import { ReactNode, useEffect, useRef, useState } from "react";
import type { DeployMarker, SeriesPoint } from "../api-series";
import { hasAnySample, markerFraction } from "../lib/series";

export type ChartTone = "acc" | "crit" | "warn" | "good" | "mut";

const TONE_VAR: Record<ChartTone, string> = {
  acc: "var(--fc-acc)",
  crit: "var(--fc-crit)",
  warn: "var(--fc-warn)",
  good: "var(--fc-good)",
  mut: "var(--fc-ink3)",
};

/**************************************************************
                         Scale helpers
 **************************************************************/

interface XY {
  x: number;
  y: number;
  v: number;
  t: number;
}

// scalePoints maps non-null samples into pixel space; index-based x (slots
// are equal-width periods), zero-anchored y (depths and rates are counts —
// a non-zero baseline would exaggerate noise).
function scalePoints(
  points: SeriesPoint[],
  width: number,
  height: number,
  pad: number
): { pts: (XY | null)[]; max: number } {
  let max = 1;
  for (const p of points) {
    if (p.v !== null && p.v > max) max = p.v;
  }
  const innerW = width - pad * 2;
  const innerH = height - pad * 2;
  const step = points.length > 1 ? innerW / (points.length - 1) : 0;
  const pts = points.map((p, i) => {
    if (p.v === null) return null;
    return {
      x: pad + i * step,
      y: pad + innerH - (p.v / max) * innerH,
      v: p.v,
      t: p.t,
    };
  });
  return { pts, max };
}

// segmentPaths builds one SVG path per contiguous non-null run — the null
// gap IS the honesty (no line across missing data).
function segmentPaths(pts: (XY | null)[]): string[] {
  const paths: string[] = [];
  let d = "";
  let n = 0;
  for (const p of pts) {
    if (p === null) {
      if (n > 0) paths.push(d);
      d = "";
      n = 0;
      continue;
    }
    d += (n === 0 ? "M" : "L") + p.x.toFixed(2) + " " + p.y.toFixed(2);
    n++;
  }
  if (n > 0) paths.push(d);
  return paths;
}

// areaPaths closes each line segment down to the baseline for the wash fill.
function areaPaths(pts: (XY | null)[], baselineY: number): string[] {
  const paths: string[] = [];
  let seg: XY[] = [];
  const flush = () => {
    if (seg.length < 2) {
      seg = [];
      return;
    }
    let d = `M${seg[0].x.toFixed(2)} ${baselineY.toFixed(2)}`;
    for (const p of seg) d += `L${p.x.toFixed(2)} ${p.y.toFixed(2)}`;
    d += `L${seg[seg.length - 1].x.toFixed(2)} ${baselineY.toFixed(2)}Z`;
    paths.push(d);
    seg = [];
  };
  for (const p of pts) {
    if (p === null) flush();
    else seg.push(p);
  }
  flush();
  return paths;
}

/**************************************************************
                          Sparkline
 **************************************************************/

export interface SparklineProps {
  points: SeriesPoint[];
  width?: number;
  height?: number;
  tone?: ChartTone;
  // Accessible name; also the hover tooltip.
  label?: string;
  className?: string;
}

export function Sparkline({
  points,
  width = 96,
  height = 24,
  tone = "acc",
  label,
  className,
}: SparklineProps) {
  if (!hasAnySample(points)) {
    // All-null (never sampled / tier-guarded): an explicit dash, not a flat
    // zero line pretending to be data.
    return (
      <span
        className={className}
        data-testid="sparkline-empty"
        title={label ? `${label}: no samples yet` : "no samples yet"}
        style={{ display: "inline-block", width, height, lineHeight: `${height}px` }}
      >
        <span style={{ color: "var(--fc-ink3)", fontSize: 10 }}>·····</span>
      </span>
    );
  }
  const color = TONE_VAR[tone];
  const pad = 3;
  const { pts } = scalePoints(points, width, height, pad);
  const lines = segmentPaths(pts);
  const areas = areaPaths(pts, height - pad);
  let last: XY | null = null;
  for (const p of pts) if (p !== null) last = p;

  return (
    <svg
      role="img"
      aria-label={label}
      data-testid="sparkline"
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      style={{ display: "block", overflow: "visible" }}
    >
      {label && <title>{label}</title>}
      {areas.map((d, i) => (
        <path key={`a${i}`} d={d} fill={color} fillOpacity={0.1} stroke="none" />
      ))}
      {lines.map((d, i) => (
        <path
          key={`l${i}`}
          d={d}
          fill="none"
          stroke={color}
          strokeWidth={1.5}
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      ))}
      {last && (
        // Endpoint dot with the 2px surface ring so it stays legible where
        // it sits on the line (dataviz mark spec).
        <circle
          data-testid="sparkline-dot"
          cx={last.x}
          cy={last.y}
          r={2.5}
          fill={color}
          stroke="var(--fc-panel)"
          strokeWidth={2}
          paintOrder="stroke"
        />
      )}
    </svg>
  );
}

/**************************************************************
                    RingBars (failure pulse)
 **************************************************************/

export interface RingBarsProps {
  points: SeriesPoint[];
  markers?: DeployMarker[];
  width?: number;
  height?: number;
  tone?: ChartTone;
  // Bar label formatter for the native tooltip (value, unix seconds).
  titleFor?: (p: SeriesPoint) => string;
  className?: string;
}

export function RingBars({
  points,
  markers = [],
  width = 560,
  height = 72,
  tone = "crit",
  titleFor,
  className,
}: RingBarsProps) {
  const color = TONE_VAR[tone];
  const padX = 2;
  const padTop = 10; // room for marker flags
  const baseline = height - 14; // room for nothing below — axis text lives outside
  const n = Math.max(points.length, 1);
  const gap = 2; // the 2px surface gap between adjacent bars (dataviz spec)
  const slot = (width - padX * 2) / n;
  const barW = Math.min(24, Math.max(2, slot - gap));

  let max = 1;
  for (const p of points) if (p.v !== null && p.v > max) max = p.v;

  const startSec = points.length > 0 ? points[0].t : 0;
  const endSec = points.length > 0 ? points[points.length - 1].t : 1;

  return (
    <svg
      role="img"
      data-testid="ringbars"
      width="100%"
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={className}
      style={{ display: "block" }}
    >
      {/* hairline baseline, recessive */}
      <line
        x1={padX}
        y1={baseline}
        x2={width - padX}
        y2={baseline}
        stroke="var(--fc-line)"
        strokeWidth={1}
      />
      {points.map((p, i) => {
        const x = padX + i * slot + (slot - barW) / 2;
        if (p.v === null) {
          // No-sample: a muted tick at the baseline — visibly "unknown",
          // clearly distinct from a zero-height (absent) bar.
          return (
            <line
              key={p.t}
              data-testid="ringbar-nosample"
              x1={x + barW / 2}
              y1={baseline - 1.5}
              x2={x + barW / 2}
              y2={baseline}
              stroke="var(--fc-ink3)"
              strokeWidth={1}
              strokeDasharray="1 2"
            />
          );
        }
        const h = p.v <= 0 ? 0 : Math.max(1.5, ((baseline - padTop) * p.v) / max);
        const r = Math.min(2, barW / 2);
        return (
          <g key={p.t} data-testid="ringbar">
            <title>
              {titleFor ? titleFor(p) : `${p.v.toLocaleString("en-US")} @ ${new Date(p.t * 1000).toLocaleTimeString()}`}
            </title>
            {/* rounded data-end, square baseline: top-rounded rect */}
            <path
              d={
                h <= 0
                  ? `M${x} ${baseline}h${barW}` // zero: nothing visible, hit target only
                  : `M${x} ${baseline}` +
                    `v${-(h - r)}` +
                    `q0 ${-r} ${r} ${-r}` +
                    `h${barW - 2 * r}` +
                    `q${r} 0 ${r} ${r}` +
                    `v${h - r}Z`
              }
              fill={color}
              fillOpacity={0.85}
            />
            {/* invisible full-height hit target so tiny bars still hover */}
            <rect x={x - gap / 2} y={padTop} width={barW + gap} height={baseline - padTop} fill="transparent" />
          </g>
        );
      })}
      {markers.map((m) => {
        const f = markerFraction(m.at, startSec, endSec);
        if (f === null) return null;
        const x = padX + f * (width - padX * 2);
        return (
          <g key={m.id} data-testid="deploy-marker">
            <title>{`${m.label} — ${m.actor} @ ${new Date(m.at).toLocaleString()}`}</title>
            <line x1={x} y1={4} x2={x} y2={baseline} stroke="var(--fc-ink2)" strokeWidth={1} />
            <path d={`M${x} 1l4 4-4 4z`} fill="var(--fc-ink2)" />
          </g>
        );
      })}
    </svg>
  );
}

/**************************************************************
                    BurnDown (verify panel)
 **************************************************************/

export interface BurnDownProps {
  points: SeriesPoint[];
  // Fitted drain line (lib/series burnDownProjection); when present and
  // negative-sloped, a dashed projection extends to the zero crossing.
  slopePerMin: number | null;
  etaMinutes: number | null;
  width?: number;
  height?: number;
  className?: string;
}

export function BurnDown({
  points,
  slopePerMin,
  etaMinutes,
  width = 340,
  height = 88,
  className,
}: BurnDownProps) {
  if (!hasAnySample(points)) return null;
  const pad = 6;
  const color = TONE_VAR.acc;

  // Domain: observed samples plus the projected zero crossing when draining.
  const samples = points.filter((p) => p.v !== null);
  const t0 = samples[0].t;
  const tLast = samples[samples.length - 1].t;
  const vLast = samples[samples.length - 1].v as number;
  const projecting = etaMinutes !== null && etaMinutes > 0 && slopePerMin !== null && slopePerMin < 0;
  const tEnd = projecting ? tLast + etaMinutes * 60 : tLast;
  const spanT = Math.max(tEnd - t0, 1);

  let max = 1;
  for (const s of samples) if ((s.v as number) > max) max = s.v as number;

  const X = (t: number) => pad + ((t - t0) / spanT) * (width - pad * 2);
  const Y = (v: number) => pad + (1 - v / max) * (height - pad * 2 - 12);
  const baseline = Y(0);

  // Observed path (null-gap aware over the raw points array).
  const segs: string[] = [];
  let d = "";
  let n = 0;
  for (const p of points) {
    if (p.v === null) {
      if (n > 0) segs.push(d);
      d = "";
      n = 0;
      continue;
    }
    d += (n === 0 ? "M" : "L") + X(p.t).toFixed(2) + " " + Y(p.v).toFixed(2);
    n++;
  }
  if (n > 0) segs.push(d);

  return (
    <svg
      role="img"
      data-testid="burndown"
      width="100%"
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={className}
      style={{ display: "block" }}
    >
      <line x1={pad} y1={baseline} x2={width - pad} y2={baseline} stroke="var(--fc-line)" strokeWidth={1} />
      {segs.map((s, i) => (
        <path key={i} d={s} fill="none" stroke={color} strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" />
      ))}
      {projecting && (
        // Dashed = projection, not data.
        <line
          data-testid="burndown-projection"
          x1={X(tLast)}
          y1={Y(vLast)}
          x2={X(tEnd)}
          y2={baseline}
          stroke={color}
          strokeWidth={1.5}
          strokeDasharray="4 3"
          strokeLinecap="round"
        />
      )}
      <circle
        cx={X(tLast)}
        cy={Y(vLast)}
        r={3}
        fill={color}
        stroke="var(--fc-panel)"
        strokeWidth={2}
        paintOrder="stroke"
      />
    </svg>
  );
}

/**************************************************************
                          LazyMount
 **************************************************************/

// LazyMount renders a fixed-size placeholder until the element scrolls into
// the viewport, then mounts children permanently (§3.2 viewport-only
// sparklines). SSR/test environments without IntersectionObserver mount
// immediately.
export function LazyMount({
  width,
  height,
  children,
}: {
  width: number;
  height: number;
  children: ReactNode;
}) {
  const ref = useRef<HTMLSpanElement>(null);
  const [visible, setVisible] = useState(
    typeof IntersectionObserver === "undefined"
  );

  useEffect(() => {
    if (visible || ref.current === null) return;
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setVisible(true);
          io.disconnect();
        }
      },
      { rootMargin: "120px" }
    );
    io.observe(ref.current);
    return () => io.disconnect();
  }, [visible]);

  return (
    <span ref={ref} style={{ display: "inline-block", width, height }}>
      {visible ? children : null}
    </span>
  );
}
