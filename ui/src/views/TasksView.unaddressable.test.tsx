// Queue Workspace × an unaddressable queue name (#49). The API addresses a
// queue with one path segment, so a name that contains "/" reaches no route,
// not even escaped as "%2F". The workspace says so and disables every
// mutating control instead of sending a request to a different queue.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { Provider } from "react-redux";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import store from "../store";
import * as api from "../api";
import * as apiFleet from "../api-fleet";
import TasksView from "./TasksView";

// Mock every api fetcher, but keep the real isAddressableName: it is the
// pure rule under test, and an automocked version returns undefined, which
// would report every name as unaddressable.
vi.mock("../api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api")>();
  const mocked: Record<string, unknown> = { ...actual };
  for (const [name, value] of Object.entries(actual)) {
    if (typeof value === "function" && name !== "isAddressableName") {
      mocked[name] = vi.fn();
    }
  }
  return mocked;
});
vi.mock("../api-fleet");

function mockApi() {
  vi.mocked(api.taskStateCounts).mockResolvedValue({
    counts: { active: 1, pending: 2, aggregating: 0, scheduled: 0, retry: 0, archived: 0, completed: 0 },
  } as Awaited<ReturnType<typeof api.taskStateCounts>>);
  vi.mocked(api.taskAggregate).mockResolvedValue({
    groups: [], scanned: 0, truncated: false,
  } as unknown as Awaited<ReturnType<typeof api.taskAggregate>>);
  vi.mocked(api.getQueue).mockResolvedValue({
    current: { queue: "q", memory_usage_bytes: 0 },
    history: [],
  } as unknown as Awaited<ReturnType<typeof api.getQueue>>);
  vi.mocked(api.listRetryTasks).mockResolvedValue({ tasks: [] } as unknown as Awaited<ReturnType<typeof api.listRetryTasks>>);
  vi.mocked(api.listActiveTasks).mockResolvedValue({ tasks: [] } as unknown as Awaited<ReturnType<typeof api.listActiveTasks>>);
  vi.mocked(api.listScheduledTasks).mockResolvedValue({ tasks: [] } as unknown as Awaited<ReturnType<typeof api.listScheduledTasks>>);
  vi.mocked(apiFleet.listFleetQueues).mockResolvedValue({ queues: [] } as unknown as Awaited<ReturnType<typeof apiFleet.listFleetQueues>>);
  vi.mocked(apiFleet.getCoverage).mockResolvedValue({ rows: [] } as unknown as Awaited<ReturnType<typeof apiFleet.getCoverage>>);
  vi.mocked(apiFleet.getRetryHistogram).mockRejectedValue(new Error("no data"));
  vi.mocked(apiFleet.getPendingWaitSample).mockRejectedValue(new Error("no data"));
}

function renderWorkspace(qname: string) {
  return render(
    <Provider store={store}>
      <MemoryRouter initialEntries={[`/queues/${encodeURIComponent(qname)}`]}>
        <Routes>
          <Route path="/queues/:qname" element={<TasksView />} />
        </Routes>
      </MemoryRouter>
    </Provider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.ROOT_PATH = "";
  window.READ_ONLY = false;
  mockApi();
});

describe("Queue Workspace × unaddressable queue name (#49)", () => {
  it("shows the notice and hides pause/resume/delete for a name with a slash", async () => {
    renderWorkspace("team/billing");
    const notice = await screen.findByRole("status");
    expect(notice).toHaveTextContent("team/billing");
    expect(notice).toHaveTextContent(
      "cannot be addressed by the API; actions are disabled"
    );
    expect(screen.queryByRole("button", { name: /pause/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /resume/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /delete/i })).toBeNull();
  });

  it("keeps the controls and shows no notice for an addressable name", async () => {
    renderWorkspace("critical#x");
    await waitFor(() => expect(api.taskStateCounts).toHaveBeenCalledWith("critical#x"));
    expect(screen.queryByRole("status")).toBeNull();
    expect(
      screen.queryByRole("button", { name: /pause|resume/i })
    ).not.toBeNull();
  });
});
