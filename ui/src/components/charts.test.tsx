// Render tests for the shared chart primitives: null-gap honesty, mark
// counts, deploy-marker overlays and the projection segment.

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Sparkline, RingBars, BurnDown } from "./charts";
import type { DeployMarker, SeriesPoint } from "../api-series";

const pts = (...vs: Array<[number, number | null]>): SeriesPoint[] =>
  vs.map(([t, v]) => ({ t, v }));

describe("Sparkline", () => {
  it("renders one path segment per contiguous non-null run (a gap breaks the line)", () => {
    const { container } = render(
      <Sparkline points={pts([0, 1], [30, 2], [60, null], [90, 3], [120, 4])} />
    );
    const svg = screen.getByTestId("sparkline");
    expect(svg).toBeTruthy();
    // 2 line segments + 2 area fills.
    const lines = container.querySelectorAll('path[fill="none"]');
    expect(lines.length).toBe(2);
    const areas = container.querySelectorAll('path[fill-opacity="0.1"]');
    expect(areas.length).toBe(2);
  });

  it("marks the newest sample with an endpoint dot carrying the surface ring", () => {
    const { container } = render(<Sparkline points={pts([0, 1], [30, 5])} />);
    const dot = screen.getByTestId("sparkline-dot");
    expect(dot.getAttribute("stroke")).toBe("var(--fc-panel)");
    expect(container.querySelectorAll("circle").length).toBe(1);
  });

  it("renders an explicit empty state for all-null series — never a fake zero line", () => {
    render(<Sparkline points={pts([0, null], [30, null])} label="pending" />);
    expect(screen.getByTestId("sparkline-empty")).toBeTruthy();
    expect(screen.queryByTestId("sparkline")).toBeNull();
  });

  it("names itself for accessibility", () => {
    render(<Sparkline points={pts([0, 1], [30, 2])} label="fleet pending, 30m" />);
    expect(screen.getByRole("img", { name: "fleet pending, 30m" })).toBeTruthy();
  });
});

describe("RingBars", () => {
  const markers: DeployMarker[] = [
    { id: "m1", label: "deploy v2", actor: "zac", at: new Date(60_000).toISOString() },
    { id: "m2", label: "off-domain", actor: "zac", at: new Date(999_999_000).toISOString() },
  ];

  it("renders a bar per sample and a muted tick per no-sample slot", () => {
    render(<RingBars points={pts([0, 5], [60, null], [120, 9], [180, 0])} />);
    // 0 is a real sample → it gets a bar group (hit target, zero height).
    expect(screen.getAllByTestId("ringbar").length).toBe(3);
    expect(screen.getAllByTestId("ringbar-nosample").length).toBe(1);
  });

  it("overlays only in-domain deploy markers", () => {
    render(<RingBars points={pts([0, 5], [60, 6], [120, 9])} markers={markers} />);
    const lines = screen.getAllByTestId("deploy-marker");
    expect(lines.length).toBe(1); // m2 is outside the x-domain → nowhere, not clamped
  });

  it("carries per-bar native tooltips", () => {
    const { container } = render(
      <RingBars
        points={pts([0, 1234])}
        titleFor={(p) => `${p.v} failures`}
      />
    );
    expect(container.querySelector("title")?.textContent).toBe("1234 failures");
  });
});

describe("BurnDown", () => {
  it("draws the observed line plus a dashed projection to zero when draining", () => {
    render(
      <BurnDown
        points={pts([0, 1000], [30, 900], [60, 800])}
        slopePerMin={-200}
        etaMinutes={4}
      />
    );
    expect(screen.getByTestId("burndown")).toBeTruthy();
    const proj = screen.getByTestId("burndown-projection");
    expect(proj.getAttribute("stroke-dasharray")).toBe("4 3"); // dashed = projection, not data
  });

  it("omits the projection when not draining", () => {
    render(
      <BurnDown
        points={pts([0, 100], [30, 150], [60, 200])}
        slopePerMin={200}
        etaMinutes={null}
      />
    );
    expect(screen.queryByTestId("burndown-projection")).toBeNull();
  });

  it("renders nothing at all for a sample-less series", () => {
    const { container } = render(
      <BurnDown points={pts([0, null])} slopePerMin={null} etaMinutes={null} />
    );
    expect(container.innerHTML).toBe("");
  });
});
