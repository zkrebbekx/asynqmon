// Accessible names for the per-row icon-only buttons in every task table
// (review #55.3). Radix Tooltip sets aria-describedby only, so each button
// carries an explicit aria-label. One case per table, resolved via
// getByRole("button", { name }).

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { Provider } from "react-redux";
import { MemoryRouter } from "react-router-dom";
import store from "../store";
import * as api from "../api";
import { Queue, TaskInfo } from "../api";
import ActiveTasksTable from "./ActiveTasksTable";
import PendingTasksTable from "./PendingTasksTable";
import ScheduledTasksTable from "./ScheduledTasksTable";
import RetryTasksTable from "./RetryTasksTable";
import ArchivedTasksTable from "./ArchivedTasksTable";
import CompletedTasksTable from "./CompletedTasksTable";
import AggregatingTasksTableContainer from "./AggregatingTasksTableContainer";
import DeleteConfirmButton from "./DeleteConfirmButton";
import { TooltipProvider } from "./ui/tooltip";

vi.mock("../api");
vi.mock("./ObservedRunStamp", () => ({ default: () => null }));

const stats: Queue = {
  queue: "q1",
  paused: false,
  size: 1,
  groups: 1,
  latency_msec: 0,
  display_latency: "0s",
  memory_usage_bytes: 0,
  active: 1,
  pending: 1,
  aggregating: 1,
  scheduled: 1,
  retry: 1,
  archived: 1,
  completed: 1,
  processed: 1,
  succeeded: 1,
  failed: 0,
  timestamp: new Date().toISOString(),
};

function task(state: string): TaskInfo {
  return {
    id: "t1-aaaa-bbbb",
    queue: "q1",
    type: "demo:task",
    payload: '{"n":1}',
    state,
    start_time: new Date().toISOString(),
    max_retry: 3,
    retried: 0,
    last_failed_at: "",
    error_message: "",
    next_process_at: new Date(Date.now() + 60_000).toISOString(),
    timeout_seconds: 30,
    deadline: "",
    group: "g1",
    completed_at: new Date().toISOString(),
    result: "",
    ttl_seconds: 0,
    is_orphaned: false,
  };
}

function ui(el: React.ReactElement) {
  return render(
    <Provider store={store}>
      <MemoryRouter>{el}</MemoryRouter>
    </Provider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.READ_ONLY = false;
});

describe("task table icon buttons carry accessible names", () => {
  it("ActiveTasksTable: Cancel task", async () => {
    vi.mocked(api.listActiveTasks).mockResolvedValue({ tasks: [task("active")], stats });
    ui(<ActiveTasksTable queue="q1" totalTaskCount={1} />);
    expect(await screen.findByRole("button", { name: /Cancel task/ })).toBeInTheDocument();
  });

  it("PendingTasksTable: Archive task and Delete task", async () => {
    vi.mocked(api.listPendingTasks).mockResolvedValue({ tasks: [task("pending")], stats });
    ui(<PendingTasksTable queue="q1" totalTaskCount={1} />);
    expect(await screen.findByRole("button", { name: /Archive task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Delete task/ })).toBeInTheDocument();
  });

  it("ScheduledTasksTable: Run task, Archive task, Delete task", async () => {
    vi.mocked(api.listScheduledTasks).mockResolvedValue({ tasks: [task("scheduled")], stats });
    ui(<ScheduledTasksTable queue="q1" totalTaskCount={1} />);
    expect(await screen.findByRole("button", { name: /Run task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Archive task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Delete task/ })).toBeInTheDocument();
  });

  it("RetryTasksTable: Run task, Archive task, Delete task", async () => {
    vi.mocked(api.listRetryTasks).mockResolvedValue({ tasks: [task("retry")], stats });
    ui(<RetryTasksTable queue="q1" totalTaskCount={1} />);
    expect(await screen.findByRole("button", { name: /Run task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Archive task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Delete task/ })).toBeInTheDocument();
  });

  it("ArchivedTasksTable: Run task and Delete task", async () => {
    vi.mocked(api.listArchivedTasks).mockResolvedValue({ tasks: [task("archived")], stats });
    ui(<ArchivedTasksTable queue="q1" totalTaskCount={1} />);
    expect(await screen.findByRole("button", { name: /Run task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Delete task/ })).toBeInTheDocument();
  });

  it("CompletedTasksTable: Delete task", async () => {
    vi.mocked(api.listCompletedTasks).mockResolvedValue({ tasks: [task("completed")], stats });
    ui(<CompletedTasksTable queue="q1" totalTaskCount={1} />);
    expect(await screen.findByRole("button", { name: /Delete task/ })).toBeInTheDocument();
  });

  it("AggregatingTasksTableContainer: Run task, Archive task, Delete task", async () => {
    vi.mocked(api.listGroups).mockResolvedValue({ stats, groups: [{ group: "g1", size: 1 }] });
    vi.mocked(api.listAggregatingTasks).mockResolvedValue({
      tasks: [task("aggregating")],
      stats,
      groups: [{ group: "g1", size: 1 }],
    });
    ui(<AggregatingTasksTableContainer queue="q1" />);
    expect(await screen.findByRole("button", { name: /Run task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Archive task/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Delete task/ })).toBeInTheDocument();
  });
});

describe("DeleteConfirmButton", () => {
  it("defaults its accessible name to Delete task and accepts an override", () => {
    const { unmount } = render(
      <TooltipProvider>
        <DeleteConfirmButton description="x" onDelete={() => {}} />
      </TooltipProvider>
    );
    expect(screen.getByRole("button", { name: "Delete task" })).toBeInTheDocument();
    unmount();
    render(
      <TooltipProvider>
        <DeleteConfirmButton description="x" onDelete={() => {}} ariaLabel="Delete view" />
      </TooltipProvider>
    );
    expect(screen.getByRole("button", { name: "Delete view" })).toBeInTheDocument();
  });
});
