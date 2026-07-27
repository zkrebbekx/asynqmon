// TasksGlobalView job-results mode: once a scan-to-completion job finishes,
// the results table serves the job's OWN enumerated result set (GET
// /api/jobs/:id/results) instead of re-running the query as a fresh budgeted
// scan — the re-run started from scratch and dropped every match beyond the
// first budget window ("4 matches (exact)" meter over a 1-row table).
// Covered: mode switch on live completion and on re-attach, the banner (count
// / exactness / as-of / vanished note), pagination driving the offset, and
// Back-to-live exiting the mode.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import { MemoryRouter } from "react-router-dom";
import store from "../store";
import * as api from "../api";
import { JobInfo, TaskInfo } from "../api";
import TasksGlobalView from "./TasksGlobalView";

vi.mock("../api");
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));
vi.mock("../hooks/useJobProgress", () => ({
  useJobProgress: vi.fn(),
}));

import { toast } from "sonner";
import { useJobProgress } from "../hooks/useJobProgress";

const mockUseJobProgress = vi.mocked(useJobProgress);

function makeTask(n: number): TaskInfo {
  return {
    // The table renders uuidPrefix(id) — everything before the first dash.
    id: `task${n}-aaaa-bbbb`,
    queue: "bulkdemo",
    type: "demo:bulk",
    payload: `{"n":${n}}`,
    state: "pending",
    max_retry: 3,
    retried: 0,
    error_message: "",
    last_failed_at: "",
    next_process_at: "",
    completed_at: "",
  } as TaskInfo;
}

function makeScanJob(over: Partial<JobInfo> = {}): JobInfo {
  return {
    id: "jb_1",
    verb: "archive",
    scope: { queue: "bulkdemo", state: "pending", q: "", meta: [] },
    phase: "preview",
    state: "previewing",
    throttle: 200,
    reason: "scan",
    actor: "tester",
    counts: { candidates: 0, acted: 0, skipped: 0, failed: 0, scanned: 0 },
    cost_class: "cheap",
    cost_list_len: 0,
    preview_complete: false,
    proceed_on_partial: false,
    created_at: "2026-07-27T00:00:00Z",
    started_at: "",
    finished_at: "",
    preview_completed_at: "",
    fence: 1,
    error: "",
    failures_overflow: 0,
    ctl_pending: "",
    ...over,
  };
}

const completedJob = makeScanJob({
  state: "preview_ready",
  preview_complete: true,
  preview_completed_at: "2026-07-27T10:00:00Z",
  counts: { candidates: 4, acted: 0, skipped: 0, failed: 0, scanned: 45000 },
});

const runningJob = makeScanJob({
  counts: { candidates: 1, acted: 0, skipped: 0, failed: 0, scanned: 9000 },
});

// The live budgeted scan's response: 1 visible match, partial coverage —
// the bug scenario's "results table shows only 1 row".
const liveScanResponse = {
  tasks: [makeTask(0)],
  total: 1,
  scanned: 10000,
  truncated: true,
  page: 1,
  size: 20,
  mode: "scan" as const,
  exact: false,
  scan_cursor: "resume-here",
  candidate_estimate: 45000,
};

function renderView(url: string) {
  return render(
    <Provider store={store}>
      <MemoryRouter initialEntries={[url]}>
        <TasksGlobalView />
      </MemoryRouter>
    </Provider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.ROOT_PATH = "";
  window.READ_ONLY = false;
  vi.mocked(api.listQueues).mockResolvedValue({ queues: [] });
  vi.mocked(api.searchTasks).mockResolvedValue(liveScanResponse);
  vi.mocked(api.taskStateCounts).mockResolvedValue({
    queue: "all",
    counts: { pending: 45000 },
    source: "live",
    refreshed_at: "",
  });
  vi.mocked(api.taskMetadata).mockResolvedValue({
    facets: [],
    scanned: 0,
    truncated: false,
  });
  vi.mocked(api.getJobResults).mockResolvedValue({
    tasks: [makeTask(1), makeTask(2), makeTask(3), makeTask(4)],
    total_candidates: 4,
    offset: 0,
    vanished: 0,
  });
  // Default: tracking a completed job (re-attach); tests override per case.
  mockUseJobProgress.mockImplementation((jobId) =>
    jobId
      ? { job: completedJob, source: "sse", error: "" }
      : { job: null, source: "poll", error: "" }
  );
});

describe("job-results mode on re-attach to a completed scan job", () => {
  it("serves the job's own result set and never re-runs the live scan", async () => {
    renderView("/tasks?q=state%3Dpending+queue%3Dbulkdemo&scanjob=jb_1");

    expect(await screen.findByTestId("job-results-banner")).toBeInTheDocument();
    // All four enumerated matches render — not the live scan's single row.
    expect(await screen.findByText("task1")).toBeInTheDocument();
    expect(screen.getByText("task2")).toBeInTheDocument();
    expect(screen.getByText("task3")).toBeInTheDocument();
    expect(screen.getByText("task4")).toBeInTheDocument();

    expect(api.getJobResults).toHaveBeenCalledWith("jb_1", 0, 20);
    // The fresh budgeted scan is NOT re-run under the job's results.
    expect(api.searchTasks).not.toHaveBeenCalled();
  });

  it("renders the banner: exact count, as-of stamp, and honest coverage note", async () => {
    renderView("/tasks?q=state%3Dpending+queue%3Dbulkdemo&scanjob=jb_1");

    const banner = await screen.findByTestId("job-results-banner");
    expect(banner).toHaveTextContent(/Showing scan results/);
    expect(banner).toHaveTextContent("4");
    expect(banner).toHaveTextContent(/matches, exact, as of/);
    expect(banner).toHaveTextContent(/\(live set may have changed\)/);
    expect(banner).toHaveTextContent(/Back to live search/);
    // No vanished tasks — no note.
    expect(banner).not.toHaveTextContent(/no longer exist/);
  });

  it("states how many enumerated tasks vanished since the scan", async () => {
    vi.mocked(api.getJobResults).mockResolvedValue({
      tasks: [makeTask(1), makeTask(2)],
      total_candidates: 4,
      offset: 0,
      vanished: 2,
    });
    renderView("/tasks?q=state%3Dpending+queue%3Dbulkdemo&scanjob=jb_1");

    expect(
      await screen.findByText(/2 tasks no longer exist since the scan/)
    ).toBeInTheDocument();
  });

  it("pages through the job's results by offset", async () => {
    const many = Array.from({ length: 20 }, (_, i) => makeTask(i));
    vi.mocked(api.getJobResults).mockResolvedValue({
      tasks: many,
      total_candidates: 45,
      offset: 0,
      vanished: 0,
    });
    renderView("/tasks?q=state%3Dpending+queue%3Dbulkdemo&scanjob=jb_1");

    const range = await screen.findByText(/1–20 of/);
    expect(range).toHaveTextContent(/1–20 of 45 · exact/);
    const prev = screen.getByRole("button", { name: "Previous page" });
    expect(prev).toBeDisabled();

    await userEvent.click(screen.getByRole("button", { name: "Next page" }));
    await waitFor(() =>
      expect(api.getJobResults).toHaveBeenLastCalledWith("jb_1", 20, 20)
    );
    expect(await screen.findByText(/21–40 of/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Previous page" })).toBeEnabled();
  });

  it("Back to live search drops the scanjob param and resumes the live query", async () => {
    renderView("/tasks?q=state%3Dpending+queue%3Dbulkdemo&scanjob=jb_1");
    await screen.findByTestId("job-results-banner");
    expect(api.searchTasks).not.toHaveBeenCalled();

    await userEvent.click(
      screen.getByRole("button", { name: /back to live search/i })
    );

    await waitFor(() =>
      expect(screen.queryByTestId("job-results-banner")).not.toBeInTheDocument()
    );
    // Normal search resumes once the param is gone.
    await waitFor(() => expect(api.searchTasks).toHaveBeenCalled());
  });
});

describe("live completion of a tracked scan job", () => {
  it("toasts the exact count, switches to job-results mode, and does NOT re-run the scan", async () => {
    let currentJob: JobInfo = runningJob;
    mockUseJobProgress.mockImplementation((jobId) =>
      jobId
        ? { job: currentJob, source: "sse", error: "" }
        : { job: null, source: "poll", error: "" }
    );
    const { rerender } = renderView(
      "/tasks?q=state%3Dpending+queue%3Dbulkdemo&scanjob=jb_1"
    );

    // While the job runs, the table is the live (partial) search.
    await waitFor(() => expect(api.searchTasks).toHaveBeenCalledTimes(1));
    expect(screen.queryByTestId("job-results-banner")).not.toBeInTheDocument();

    // Enumeration completes: the hook reports the settled job once, live.
    const options = mockUseJobProgress.mock.calls.at(-1)?.[1];
    currentJob = completedJob;
    act(() => {
      options?.onSettled?.(completedJob);
    });
    rerender(
      <Provider store={store}>
        <MemoryRouter
          initialEntries={["/tasks?q=state%3Dpending+queue%3Dbulkdemo&scanjob=jb_1"]}
        >
          <TasksGlobalView />
        </MemoryRouter>
      </Provider>
    );

    expect(vi.mocked(toast.success)).toHaveBeenCalledWith(
      "Scan complete — 4 matches (exact)",
      expect.anything()
    );
    expect(await screen.findByTestId("job-results-banner")).toBeInTheDocument();
    expect(await screen.findByText("task4")).toBeInTheDocument();
    await waitFor(() => expect(api.getJobResults).toHaveBeenCalled());
    // The completion no longer triggers a fresh budgeted re-scan.
    expect(api.searchTasks).toHaveBeenCalledTimes(1);
  });
});
