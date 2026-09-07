// Review #48: asynq writes the queued-at record (pending_since) on enqueue
// and scheduler forwarding only. A task re-run through Run / Run all, or
// requeued by a worker shutdown, keeps none, so `pending_age>` cannot
// evaluate it. The scan response reports how many such tasks it skipped and
// the console says so, with a one-click list of them.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import { MemoryRouter } from "react-router-dom";
import store from "../store";
import * as api from "../api";
import TasksGlobalView from "./TasksGlobalView";

vi.mock("../api");
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));
vi.mock("../hooks/useJobProgress", () => ({
  useJobProgress: vi.fn(() => ({ job: null, source: "poll", error: "" })),
}));

const scanResponse = {
  tasks: [],
  total: 0,
  scanned: 500,
  truncated: false,
  page: 1,
  size: 20,
  mode: "scan" as const,
  exact: false,
  scan_cursor: "",
  candidate_estimate: 500,
  pending_since_unknown: 3,
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
  vi.mocked(api.searchTasks).mockResolvedValue(scanResponse);
  vi.mocked(api.taskStateCounts).mockResolvedValue({
    queue: "all",
    counts: { pending: 500 },
    source: "live",
    refreshed_at: "",
  });
  vi.mocked(api.taskMetadata).mockResolvedValue({
    facets: [],
    scanned: 0,
    truncated: false,
  });
});

describe("pending tasks with no queued-at record", () => {
  it("reports how many the scan could not evaluate", async () => {
    renderView("/tasks?q=state%3Dpending+pending_age%3E1h");

    const note = await screen.findByTestId("pending-unknown-note");
    expect(note).toHaveTextContent(
      "3 pending tasks have no queued-at record and were not evaluated"
    );
    expect(note).toHaveTextContent(/requeued by a worker shutdown/);
  });

  it("lists them through pending_age=unknown", async () => {
    renderView("/tasks?q=state%3Dpending+pending_age%3E1h");

    const note = await screen.findByTestId("pending-unknown-note");
    await userEvent.click(within(note).getByRole("button", { name: "List them" }));

    await vi.waitFor(() => {
      expect(api.searchTasks).toHaveBeenCalledWith(
        expect.objectContaining({ q: "state=pending pending_age=unknown" })
      );
    });
  });

  it("stays silent when every pending task has a record", async () => {
    vi.mocked(api.searchTasks).mockResolvedValue({
      ...scanResponse,
      pending_since_unknown: 0,
    });
    renderView("/tasks?q=state%3Dpending+pending_age%3E1h");

    await vi.waitFor(() => expect(api.searchTasks).toHaveBeenCalled());
    expect(screen.queryByTestId("pending-unknown-note")).toBeNull();
  });
});
