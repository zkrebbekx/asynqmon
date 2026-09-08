// Queue Workspace × a queue name that contains "/". asynq puts no
// restriction on a queue name, so a producer may create "tenant/acme". The
// server matches on the raw path (mux.Router.UseEncodedPath), so such a
// queue is fully addressable: the workspace must keep every mutating
// control and must show no notice.
//
// This file replaces TasksView.unaddressable.test.tsx, which asserted the
// old stopgap (notice shown, controls disabled).

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { Provider } from "react-redux";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import store from "../store";
import * as api from "../api";
import * as apiFleet from "../api-fleet";
import TasksView from "./TasksView";

vi.mock("../api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api")>();
  const mocked: Record<string, unknown> = { ...actual };
  for (const [name, value] of Object.entries(actual)) {
    if (typeof value === "function") {
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

describe("Queue Workspace × a queue name that contains a slash", () => {
  it("passes the decoded name to the API and keeps pause/resume", async () => {
    renderWorkspace("tenant/acme");
    await waitFor(() => expect(api.taskStateCounts).toHaveBeenCalledWith("tenant/acme"));
    expect(screen.queryByRole("button", { name: /pause|resume/i })).not.toBeNull();
  });

  it("shows no unaddressable notice", async () => {
    renderWorkspace("tenant/acme");
    await waitFor(() => expect(api.taskStateCounts).toHaveBeenCalledWith("tenant/acme"));
    expect(screen.queryByText(/cannot be addressed by the API/i)).toBeNull();
  });

  it("keeps the controls for a name with '#', '?' and '%'", async () => {
    renderWorkspace("a#b?c%d");
    await waitFor(() => expect(api.taskStateCounts).toHaveBeenCalledWith("a#b?c%d"));
    expect(screen.queryByRole("button", { name: /pause|resume/i })).not.toBeNull();
  });
});
