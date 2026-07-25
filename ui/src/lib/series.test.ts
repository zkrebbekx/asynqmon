// Unit tests for the §5.8 series math (lib/series.ts). Pure functions with
// explicit times — no wall clock anywhere, no flakes.

import { describe, it, expect } from "vitest";
import {
  sliceLastSeconds,
  lastSample,
  hasAnySample,
  linearFit,
  ratePerMinute,
  formatRate,
  burnDownProjection,
  formatEta,
  projectionSentence,
  markerFraction,
  learningRemaining,
  PROJECTION_POINTS,
} from "./series";
import type { SeriesPoint } from "../api-series";

const pts = (...vs: Array<[number, number | null]>): SeriesPoint[] =>
  vs.map(([t, v]) => ({ t, v }));

describe("sliceLastSeconds", () => {
  const series = pts([100, 1], [130, null], [160, 3], [190, 4]);

  it("keeps points inside the trailing window, preserving null gaps", () => {
    expect(sliceLastSeconds(series, 60, 190)).toEqual(pts([160, 3], [190, 4]));
    expect(sliceLastSeconds(series, 90, 190)).toEqual(pts([130, null], [160, 3], [190, 4]));
  });

  it("excludes points after now (no future samples)", () => {
    expect(sliceLastSeconds(series, 300, 160)).toEqual(pts([100, 1], [130, null], [160, 3]));
  });

  it("returns empty for an empty window", () => {
    expect(sliceLastSeconds(series, 0, 190)).toEqual([]);
  });
});

describe("lastSample / hasAnySample", () => {
  it("finds the newest non-null point past trailing nulls", () => {
    expect(lastSample(pts([1, 5], [2, 9], [3, null]))).toEqual({ t: 2, v: 9 });
  });
  it("returns null for all-null series (no-sample is not zero)", () => {
    expect(lastSample(pts([1, null], [2, null]))).toBeNull();
    expect(hasAnySample(pts([1, null]))).toBe(false);
    expect(hasAnySample(pts([1, null], [2, 0]))).toBe(true); // zero IS a sample
  });
});

describe("linearFit", () => {
  it("fits an exact line", () => {
    // y = 2x + 1 over x = 0, 30, 60
    const fit = linearFit(pts([0, 1], [30, 61], [60, 121]));
    expect(fit).not.toBeNull();
    expect(fit!.slope).toBeCloseTo(2, 10);
    expect(fit!.intercept).toBeCloseTo(1, 10);
    expect(fit!.samples).toBe(3);
  });

  it("skips null gaps", () => {
    const fit = linearFit(pts([0, 0], [30, null], [60, 120]));
    expect(fit!.slope).toBeCloseTo(2, 10);
    expect(fit!.samples).toBe(2);
  });

  it("needs at least two samples", () => {
    expect(linearFit(pts([0, 5]))).toBeNull();
    expect(linearFit(pts([0, null], [30, null]))).toBeNull();
    expect(linearFit([])).toBeNull();
  });

  it("refuses a vertical fit (all samples at one instant)", () => {
    expect(linearFit(pts([10, 1], [10, 9]))).toBeNull();
  });
});

describe("ratePerMinute / formatRate", () => {
  it("expresses the fitted slope per minute", () => {
    // +1 per 30s = +2/min
    expect(ratePerMinute(pts([0, 10], [30, 11], [60, 12]))).toBeCloseTo(2, 10);
  });
  it("formats signed, thousands-separated rates", () => {
    expect(formatRate(340.4, "min")).toBe("+340/min");
    expect(formatRate(-12, "min")).toBe("−12/min");
    expect(formatRate(1234.6, "hr")).toBe("+1,235/hr");
  });
  it("reads sub-1 magnitudes as flat and null as absent", () => {
    expect(formatRate(0.4, "min")).toBe("flat");
    expect(formatRate(-0.49, "min")).toBe("flat");
    expect(formatRate(null, "min")).toBe("—");
    expect(formatRate(Number.NaN, "min")).toBe("—");
  });
});

describe("burnDownProjection (§4.3 step 6)", () => {
  it("projects the drain ETA from a negative slope", () => {
    // 1000 → 900 → 800 … one point per 30s: −200/min; current 800 → 4m.
    const proj = burnDownProjection(pts([0, 1000], [30, 900], [60, 800]));
    expect(proj).not.toBeNull();
    expect(proj!.slopePerMin).toBeCloseTo(-200, 6);
    expect(proj!.etaMinutes).toBeCloseTo(4, 6);
    expect(projectionSentence(proj)).toBe("drains in ~4m at current rate");
  });

  it("says NOT draining when the slope is non-negative — never an invented ETA", () => {
    const proj = burnDownProjection(pts([0, 100], [30, 150], [60, 220]));
    expect(proj!.etaMinutes).toBeNull();
    expect(projectionSentence(proj)).toBe("not draining at current rate");
    const flat = burnDownProjection(pts([0, 100], [30, 100], [60, 100]));
    expect(flat!.etaMinutes).toBeNull();
  });

  it("fits over only the last N points (an old plateau must not dilute the current drain)", () => {
    // 15 samples: 5 flat at 1000, then 10 falling 100 per 30s.
    const flat: Array<[number, number]> = [0, 30, 60, 90, 120].map((t) => [t, 1000]);
    const falling: Array<[number, number]> = Array.from({ length: 10 }, (_, i) => [
      150 + i * 30,
      1000 - (i + 1) * 100,
    ]);
    const proj = burnDownProjection(pts(...flat, ...falling));
    expect(proj!.slopePerMin).toBeCloseTo(-200, 6);
    expect(PROJECTION_POINTS).toBe(10);
  });

  it("skips null gaps when collecting the fit window", () => {
    const proj = burnDownProjection(pts([0, 600], [30, null], [60, 400], [90, null], [120, 200]));
    expect(proj!.etaMinutes).not.toBeNull();
    expect(proj!.current).toBe(200);
  });

  it("returns null when there are not enough samples to fit", () => {
    expect(burnDownProjection(pts([0, 500]))).toBeNull();
    expect(burnDownProjection(pts([0, null], [30, null]))).toBeNull();
    expect(projectionSentence(null)).toBe("");
  });

  it("reports an already-empty state as drained", () => {
    const proj = burnDownProjection(pts([0, 100], [30, 40], [60, 0]));
    expect(proj!.etaMinutes).toBe(0);
    expect(projectionSentence(proj)).toBe("drained");
  });
});

describe("formatEta", () => {
  it("renders minutes, hours and days in ops-speak", () => {
    expect(formatEta(0.4)).toBe("<1m");
    expect(formatEta(42)).toBe("~42m");
    expect(formatEta(70)).toBe("~1h 10m");
    expect(formatEta(120)).toBe("~2h");
    expect(formatEta(60 * 24 * 3 + 60 * 4)).toBe("~3d 4h");
  });
  it("refuses nonsense", () => {
    expect(formatEta(-5)).toBe("—");
    expect(formatEta(Number.POSITIVE_INFINITY)).toBe("—");
  });
});

describe("markerFraction", () => {
  const start = Date.parse("2026-07-25T00:00:00Z") / 1000;
  const end = Date.parse("2026-07-25T04:00:00Z") / 1000;

  it("maps a marker into its 0..1 x-position", () => {
    expect(markerFraction("2026-07-25T01:00:00Z", start, end)).toBeCloseTo(0.25, 10);
    expect(markerFraction("2026-07-25T04:00:00Z", start, end)).toBe(1);
    expect(markerFraction("2026-07-25T00:00:00Z", start, end)).toBe(0);
  });

  it("drops off-domain and unparsable markers (nowhere, not at an edge)", () => {
    expect(markerFraction("2026-07-24T23:59:59Z", start, end)).toBeNull();
    expect(markerFraction("2026-07-25T05:00:00Z", start, end)).toBeNull();
    expect(markerFraction("yesterday-ish", start, end)).toBeNull();
    expect(markerFraction("2026-07-25T01:00:00Z", end, start)).toBeNull();
  });
});

describe("learningRemaining", () => {
  const now = Date.parse("2026-07-25T12:00:00Z");

  it("counts down in hours then minutes", () => {
    expect(learningRemaining("2026-07-26T11:00:00Z", now)).toBe("~23h");
    expect(learningRemaining("2026-07-25T12:25:00Z", now)).toBe("~25m");
    expect(learningRemaining("2026-07-25T12:00:20Z", now)).toBe("~1m");
  });

  it("goes silent once live, absent or unparsable", () => {
    expect(learningRemaining("2026-07-25T11:59:00Z", now)).toBe("");
    expect(learningRemaining(undefined, now)).toBe("");
    expect(learningRemaining("not-a-time", now)).toBe("");
  });
});
