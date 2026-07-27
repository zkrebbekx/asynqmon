import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { JobInfo } from "../api";

// Mock the API (poll fallback) and the SSE URL; the transport is a stubbed
// global EventSource below (same pattern as useFleetEvents.test).
vi.mock("../api", () => ({
  getJob: vi.fn(),
}));
vi.mock("../api-fleet", () => ({
  fleetEventsUrl: () => "/api/fleet/events",
}));

import { getJob } from "../api";
import { useJobProgress } from "./useJobProgress";

const mockGetJob = vi.mocked(getJob);

function makeJob(over: Partial<JobInfo> = {}): JobInfo {
  return {
    id: "jb_1",
    verb: "archive",
    scope: { queue: "all", state: "pending", q: "", meta: [] },
    phase: "preview",
    state: "previewing",
    throttle: 200,
    reason: "test",
    actor: "tester",
    counts: { candidates: 0, acted: 0, skipped: 0, failed: 0, scanned: 0 },
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

class MockEventSource {
  static instances: MockEventSource[] = [];
  url: string;
  listeners: Record<string, (e: MessageEvent) => void> = {};
  onerror: (() => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }
  addEventListener(name: string, fn: (e: MessageEvent) => void) {
    this.listeners[name] = fn;
  }
  close() {
    this.closed = true;
  }
  emitJobs(jobs: JobInfo[]) {
    this.listeners["jobs"]?.({ data: JSON.stringify({ jobs }) } as MessageEvent);
  }
  fail() {
    this.onerror?.();
  }
}

describe("useJobProgress", () => {
  beforeEach(() => {
    MockEventSource.instances = [];
    vi.stubGlobal("EventSource", MockEventSource);
    mockGetJob.mockResolvedValue(makeJob() as Awaited<ReturnType<typeof getJob>>);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.clearAllMocks();
    vi.useRealTimers();
  });

  it("does nothing without a job id", async () => {
    const { result } = renderHook(() => useJobProgress(null));
    await act(async () => {});
    expect(result.current.job).toBeNull();
    expect(mockGetJob).not.toHaveBeenCalled();
    expect(MockEventSource.instances).toHaveLength(0);
  });

  it("hydrates immediately from GET /api/jobs/:id (source: poll)", async () => {
    const { result } = renderHook(() => useJobProgress("jb_1"));
    await waitFor(() => expect(result.current.job).not.toBeNull());
    expect(result.current.job?.id).toBe("jb_1");
    expect(result.current.source).toBe("poll");
    expect(mockGetJob).toHaveBeenCalledWith("jb_1");
  });

  it("keeps polling every 2s while SSE is not delivering the job", async () => {
    vi.useFakeTimers();
    renderHook(() => useJobProgress("jb_1"));
    await act(async () => {}); // initial fetch
    expect(mockGetJob).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4_100);
    });
    expect(mockGetJob).toHaveBeenCalledTimes(3); // +2 fallback polls
  });

  it("switches to SSE updates and stands the poll down", async () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useJobProgress("jb_1"));
    await act(async () => {});
    const progressed = makeJob({
      counts: { candidates: 7, acted: 0, skipped: 0, failed: 0, scanned: 40 },
    });
    act(() => MockEventSource.instances[0].emitJobs([progressed]));
    expect(result.current.source).toBe("sse");
    expect(result.current.job?.counts.scanned).toBe(40);
    // The reconciliation fetch on transport switch may run once; afterwards
    // no 2s cadence keeps firing while SSE covers the job.
    await act(async () => {});
    const callsAfterSwitch = mockGetJob.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6_000);
    });
    expect(mockGetJob.mock.calls.length).toBe(callsAfterSwitch);
  });

  it("ignores jobs events for other jobs", async () => {
    const { result } = renderHook(() => useJobProgress("jb_1"));
    await waitFor(() => expect(result.current.job).not.toBeNull());
    act(() =>
      MockEventSource.instances[0].emitJobs([
        makeJob({ id: "jb_OTHER", counts: { candidates: 99, acted: 0, skipped: 0, failed: 0, scanned: 99 } }),
      ])
    );
    expect(result.current.job?.counts.candidates).toBe(0);
    expect(result.current.source).toBe("poll"); // stream is up but not covering OUR job
  });

  it("fires onSettled exactly once when the job settles live", async () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() =>
      useJobProgress("jb_1", { onSettled })
    );
    await waitFor(() => expect(result.current.job).not.toBeNull());
    expect(onSettled).not.toHaveBeenCalled();

    const done = makeJob({
      phase: "execute",
      state: "done",
      counts: { candidates: 7, acted: 7, skipped: 0, failed: 0, scanned: 40 },
    });
    act(() => MockEventSource.instances[0].emitJobs([done]));
    expect(onSettled).toHaveBeenCalledTimes(1);
    expect(onSettled.mock.calls[0][0].state).toBe("done");
    // A duplicate terminal frame must not re-fire.
    act(() => MockEventSource.instances[0].emitJobs([done]));
    expect(onSettled).toHaveBeenCalledTimes(1);
  });

  it("honors a custom settled predicate (preview_complete)", async () => {
    const onSettled = vi.fn();
    const settled = (j: JobInfo) => j.preview_complete;
    const { result } = renderHook(() =>
      useJobProgress("jb_1", { settled, onSettled })
    );
    await waitFor(() => expect(result.current.job).not.toBeNull());
    act(() =>
      MockEventSource.instances[0].emitJobs([
        makeJob({ preview_complete: true, state: "preview_ready" }),
      ])
    );
    expect(onSettled).toHaveBeenCalledTimes(1);
    expect(onSettled.mock.calls[0][0].state).toBe("preview_ready");
  });

  it("attaches silently to an already-settled job (reload after the fact)", async () => {
    const onSettled = vi.fn();
    mockGetJob.mockResolvedValue(
      makeJob({ phase: "execute", state: "done" }) as Awaited<ReturnType<typeof getJob>>
    );
    const { result } = renderHook(() => useJobProgress("jb_1", { onSettled }));
    await waitFor(() => expect(result.current.job?.state).toBe("done"));
    expect(onSettled).not.toHaveBeenCalled();
  });

  it("never regresses a terminal job on a stale frame", async () => {
    const { result } = renderHook(() => useJobProgress("jb_1"));
    await waitFor(() => expect(result.current.job).not.toBeNull());
    act(() =>
      MockEventSource.instances[0].emitJobs([makeJob({ state: "canceled" })])
    );
    expect(result.current.job?.state).toBe("canceled");
    act(() =>
      MockEventSource.instances[0].emitJobs([makeJob({ state: "previewing" })])
    );
    expect(result.current.job?.state).toBe("canceled");
  });

  it("keeps working on polling alone when the stream errors", async () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useJobProgress("jb_1"));
    await act(async () => {});
    act(() => MockEventSource.instances[0].fail());
    expect(MockEventSource.instances[0].closed).toBe(true);
    mockGetJob.mockResolvedValue(
      makeJob({
        counts: { candidates: 3, acted: 0, skipped: 0, failed: 0, scanned: 12 },
      }) as Awaited<ReturnType<typeof getJob>>
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_100);
    });
    expect(result.current.job?.counts.scanned).toBe(12);
    expect(result.current.source).toBe("poll");
  });
});
