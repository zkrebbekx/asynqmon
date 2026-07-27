// ScanJobMeter render states: attaching, counting (with matches + source
// label + cancel), paused, complete-with-exact-count, canceled and failed.
// Presentational component — the transport lives in useJobProgress.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { JobInfo } from "../api";
import ScanJobMeter from "./ScanJobMeter";

function makeJob(over: Partial<JobInfo> = {}): JobInfo {
  return {
    id: "jb_1753500000000_ab12cd34",
    verb: "archive",
    scope: { queue: "all", state: "pending", q: "boom", meta: [] },
    phase: "preview",
    state: "previewing",
    throttle: 200,
    reason: "scan",
    actor: "tester",
    counts: { candidates: 120, acted: 0, skipped: 0, failed: 0, scanned: 4500 },
    cost_class: "cheap",
    cost_list_len: 0,
    preview_complete: false,
    proceed_on_partial: false,
    created_at: "2026-07-27T00:00:00Z",
    started_at: "",
    finished_at: "",
    fence: 1,
    error: "",
    failures_overflow: 0,
    ctl_pending: "",
    ...over,
  };
}

function renderMeter(props: Partial<Parameters<typeof ScanJobMeter>[0]> = {}) {
  const onCancel = vi.fn();
  const onDismiss = vi.fn();
  render(
    <MemoryRouter>
      <ScanJobMeter
        job={makeJob()}
        live
        candidateEstimate={10_000}
        onCancel={onCancel}
        onDismiss={onDismiss}
        {...props}
      />
    </MemoryRouter>
  );
  return { onCancel, onDismiss };
}

beforeEach(() => {
  window.READ_ONLY = false;
});

describe("ScanJobMeter", () => {
  it("shows the attaching state while the job has not loaded", () => {
    renderMeter({ job: null });
    expect(screen.getByText(/attaching to scan job/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /cancel scan/i })).toBeNull();
  });

  it("counts scanned/candidates up with matches and the live-source label", () => {
    const { onCancel } = renderMeter();
    expect(
      screen.getByText(/scanned 4,500 of ~10,000 · 120 matches/)
    ).toBeInTheDocument();
    expect(screen.getByText("live stream")).toBeInTheDocument();
    expect(screen.getByText(/→ Operations/)).toBeInTheDocument();
    // Cancel wires through.
    screen.getByRole("button", { name: /cancel scan/i }).click();
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("labels the poll fallback honestly", () => {
    renderMeter({ live: false });
    expect(screen.getByText("polling")).toBeInTheDocument();
  });

  it("omits the estimate bound when none is known", () => {
    renderMeter({ candidateEstimate: 0 });
    expect(screen.getByText(/scanned 4,500 · 120 matches/)).toBeInTheDocument();
  });

  it("flags a paused scan", () => {
    renderMeter({ job: makeJob({ state: "paused" }) });
    expect(screen.getByText(/· paused/)).toBeInTheDocument();
  });

  it("states the exact match count on completion and offers dismiss", async () => {
    const { onDismiss } = renderMeter({
      job: makeJob({
        preview_complete: true,
        state: "preview_ready",
        counts: { candidates: 1234, acted: 0, skipped: 0, failed: 0, scanned: 10_000 },
      }),
    });
    expect(
      screen.getByText("scan complete — 1,234 matches (exact)")
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /cancel scan/i })).toBeNull();
    await userEvent.click(
      screen.getByRole("button", { name: /dismiss scan job meter/i })
    );
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("reports a canceled scan as partial", () => {
    renderMeter({
      job: makeJob({
        state: "canceled",
        counts: { candidates: 77, acted: 0, skipped: 0, failed: 0, scanned: 2000 },
      }),
    });
    expect(
      screen.getByText(/scan canceled — 77 matches \(partial, 2,000 scanned\)/)
    ).toBeInTheDocument();
  });

  it("surfaces a failed scan with its error", () => {
    renderMeter({
      job: makeJob({ state: "failed", error: "compiling scope: boom" }),
    });
    expect(
      screen.getByText(/scan failed — compiling scope: boom/)
    ).toBeInTheDocument();
  });
});
