// Pure math + formatting for the §5.8 series (phase 10): window slicing,
// rate-of-change, the §4.3-step-6 burn-down projection ("drains in ~42m at
// current rate"), deploy-marker positioning and learning-badge countdowns.
// Every function takes explicit times — no wall clocks, fully unit-testable.

import type { SeriesPoint } from "../api-series";

/**************************************************************
                        Slicing & shaping
 **************************************************************/

// sliceLastSeconds keeps the points whose period start falls within the last
// `seconds` before nowSec (inclusive), preserving null gaps.
export function sliceLastSeconds(
  points: SeriesPoint[],
  seconds: number,
  nowSec: number
): SeriesPoint[] {
  const cutoff = nowSec - seconds;
  return points.filter((p) => p.t > cutoff && p.t <= nowSec);
}

// lastSample returns the newest non-null point, or null when the series has
// no samples at all.
export function lastSample(points: SeriesPoint[]): SeriesPoint | null {
  for (let i = points.length - 1; i >= 0; i--) {
    if (points[i].v !== null) return points[i];
  }
  return null;
}

// hasAnySample — a series of pure nulls renders as an empty spark, not a
// zero line.
export function hasAnySample(points: SeriesPoint[]): boolean {
  return points.some((p) => p.v !== null);
}

/**************************************************************
                       Linear fit & rates
 **************************************************************/

export interface LinearFit {
  // slope in value units per second; intercept at x = 0 (seconds).
  slope: number;
  intercept: number;
  samples: number;
}

// linearFit computes an ordinary least-squares line over the NON-NULL points
// (x = t seconds, y = v). Needs ≥2 distinct-x samples; returns null otherwise.
export function linearFit(points: SeriesPoint[]): LinearFit | null {
  const xs: number[] = [];
  const ys: number[] = [];
  for (const p of points) {
    if (p.v !== null) {
      xs.push(p.t);
      ys.push(p.v);
    }
  }
  const n = xs.length;
  if (n < 2) return null;
  const meanX = xs.reduce((a, b) => a + b, 0) / n;
  const meanY = ys.reduce((a, b) => a + b, 0) / n;
  let sxx = 0;
  let sxy = 0;
  for (let i = 0; i < n; i++) {
    sxx += (xs[i] - meanX) * (xs[i] - meanX);
    sxy += (xs[i] - meanX) * (ys[i] - meanY);
  }
  if (sxx === 0) return null; // all samples share one instant
  const slope = sxy / sxx;
  return { slope, intercept: meanY - slope * meanX, samples: n };
}

// ratePerMinute is the fitted slope over the given points expressed per
// minute — the KPI tiles' Δ/min. Null when the series can't support a fit.
export function ratePerMinute(points: SeriesPoint[]): number | null {
  const fit = linearFit(points);
  return fit === null ? null : fit.slope * 60;
}

// formatRate renders a Δ/unit figure: "+340/min", "−12/min", "flat".
// Sub-1 magnitudes are flat — sparkline noise, not a trend.
export function formatRate(perUnit: number | null, unit: string): string {
  if (perUnit === null || !Number.isFinite(perUnit)) return "—";
  const rounded = Math.round(perUnit);
  if (Math.abs(rounded) < 1) return "flat";
  const sign = rounded > 0 ? "+" : "−";
  return `${sign}${Math.abs(rounded).toLocaleString("en-US")}/${unit}`;
}

/**************************************************************
              Burn-down projection (§4.3 step 6)
 **************************************************************/

// How many trailing non-null points the projection fits over (contract:
// "linear fit over last 10 points of the scoped state's series").
export const PROJECTION_POINTS = 10;

export interface BurnDownProjection {
  // Fitted slope per minute over the last PROJECTION_POINTS samples.
  slopePerMin: number;
  // Minutes until the fitted line crosses zero from the last sample;
  // null when the state is not draining (slope ≥ 0).
  etaMinutes: number | null;
  // The last observed value the projection starts from.
  current: number;
}

// burnDownProjection fits the last PROJECTION_POINTS non-null samples.
// Returns null when there are not enough samples to fit (the verify panel
// then shows nothing rather than a fabricated trend); etaMinutes is null
// when the slope is non-negative — the UI must say "not draining", never
// invent an ETA (§4.3 step 6: projection only when ring-buffer rates exist
// AND the slope is negative).
export function burnDownProjection(points: SeriesPoint[]): BurnDownProjection | null {
  const samples = points.filter((p) => p.v !== null);
  const recent = samples.slice(-PROJECTION_POINTS);
  const fit = linearFit(recent);
  if (fit === null) return null;
  const last = recent[recent.length - 1];
  const current = last.v as number;
  const slopePerMin = fit.slope * 60;
  if (slopePerMin >= 0) return { slopePerMin, etaMinutes: null, current };
  if (current <= 0) return { slopePerMin, etaMinutes: 0, current };
  return { slopePerMin, etaMinutes: current / -slopePerMin, current };
}

// formatEta renders minutes as the verify panel's phrase: "~42m", "~1h 10m",
// "~3d 4h", "<1m".
export function formatEta(minutes: number): string {
  if (!Number.isFinite(minutes) || minutes < 0) return "—";
  if (minutes < 1) return "<1m";
  const m = Math.round(minutes);
  if (m < 60) return `~${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) {
    const rem = m % 60;
    return rem > 0 ? `~${h}h ${rem}m` : `~${h}h`;
  }
  const d = Math.floor(h / 24);
  const remH = h % 24;
  return remH > 0 ? `~${d}d ${remH}h` : `~${d}d`;
}

// projectionSentence is the exact verify-panel copy (§4.3 step 6).
export function projectionSentence(proj: BurnDownProjection | null): string {
  if (proj === null) return "";
  if (proj.etaMinutes === null) return "not draining at current rate";
  if (proj.etaMinutes === 0 && proj.current <= 0) return "drained";
  return `drains in ${formatEta(proj.etaMinutes)} at current rate`;
}

/**************************************************************
                    Deploy marker placement
 **************************************************************/

// markerFraction maps a marker's RFC3339 time onto a chart's x-domain
// [startSec, endSec] as a 0..1 fraction; null when outside the domain or
// unparsable (an off-chart marker renders nowhere, not at an edge).
export function markerFraction(
  at: string,
  startSec: number,
  endSec: number
): number | null {
  const t = Date.parse(at) / 1000;
  if (!Number.isFinite(t) || endSec <= startSec) return null;
  if (t < startSec || t > endSec) return null;
  return (t - startSec) / (endSec - startSec);
}

/**************************************************************
                     Learning badges (§3.1)
 **************************************************************/

// learningRemaining renders "active in ~Xh" / "~Xm" from an RFC3339
// learning_until; empty string once live (or unparsable — never a lie).
export function learningRemaining(until: string | undefined, nowMs: number): string {
  if (!until) return "";
  const t = Date.parse(until);
  if (Number.isNaN(t) || t <= nowMs) return "";
  const mins = (t - nowMs) / 60_000;
  if (mins < 60) return `~${Math.max(1, Math.round(mins))}m`;
  return `~${Math.round(mins / 60)}h`;
}
