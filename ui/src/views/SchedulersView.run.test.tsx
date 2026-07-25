// SchedulersView × run-now (upstream hibiken/asynqmon#337): the per-row
// "Run now" action appears only when the deployment enables enqueue (§5.10),
// shows on live AND gone rows (a gone entry is precisely the one you fire by
// hand), and requires the two-step inline confirm — with the honest "fresh
// task, not a scheduler tick" label — before POSTing.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import { MemoryRouter } from "react-router-dom";
import store from "../store";
import * as api from "../api";
import * as apiFleet from "../api-fleet";
import type { SchedulerRow } from "../api-fleet";
import type { TaskInfo } from "../api";
import { resetFeaturesCache } from "../hooks/useFeatures";
import SchedulersView from "./SchedulersView";

vi.mock("../api");
vi.mock("../api-fleet");

const rows: SchedulerRow[] = [
  {
    stable_key: "a".repeat(64),
    live: true,
    first_seen: "2026-07-20T00:00:00Z",
    last_seen: "2026-07-26T00:00:00Z",
    entry: {
      id: "live-entry-id",
      spec: "@every 1h",
      task_type: "report:build",
      task_payload: '{"kind":"daily"}',
      options: ['Queue("reports")'],
      next_enqueue_at: "2026-07-26T02:00:00Z",
      prev_enqueue_at: "2026-07-26T01:00:00Z",
    },
  },
  {
    stable_key: "b".repeat(64),
    live: false,
    first_seen: "2026-07-01T00:00:00Z",
    last_seen: "2026-07-20T00:00:00Z",
    gone_since: "2026-07-20T00:00:00Z",
    entry: {
      id: "gone-entry-id",
      spec: "@every 5m",
      task_type: "email:digest",
      task_payload: "{}",
      options: [],
      next_enqueue_at: "",
      prev_enqueue_at: "",
    },
  },
];

const createdTask = {
  id: "new-task-id",
  queue: "reports",
  type: "report:build",
  state: "pending",
  payload: '{"kind":"daily"}',
} as TaskInfo;

function mockApi(enqueueEnabled: boolean) {
  vi.mocked(apiFleet.listSchedulers).mockResolvedValue({ entries: rows });
  vi.mocked(api.getFeatures).mockResolvedValue({
    features: { enqueue: enqueueEnabled },
  });
  vi.mocked(apiFleet.runSchedulerEntry).mockResolvedValue({
    task: createdTask,
    applied_options: ['Queue("reports")'],
    skipped_options: ["ProcessIn(1m30s) — schedule is replaced by run-now"],
  });
}

function renderView() {
  return render(
    <Provider store={store}>
      <MemoryRouter>
        <SchedulersView />
      </MemoryRouter>
    </Provider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  resetFeaturesCache();
  window.ROOT_PATH = "";
  window.READ_ONLY = false;
});

describe("SchedulersView run-now gating (#337 × §5.10)", () => {
  it("shows a Run now button on every row — live and GONE — when enqueue is enabled", async () => {
    mockApi(true);
    renderView();
    expect(await screen.findByText("report:build")).toBeInTheDocument();
    expect(screen.getByText("SCHEDULER GONE")).toBeInTheDocument();
    const buttons = await screen.findAllByText("Run now");
    expect(buttons).toHaveLength(2); // one per row, including the gone one
  });

  it("hides the action entirely when the deployment has enqueue off", async () => {
    mockApi(false);
    renderView();
    expect(await screen.findByText("report:build")).toBeInTheDocument();
    expect(screen.queryByText("Run now")).not.toBeInTheDocument();
    expect(screen.queryByText("Actions")).not.toBeInTheDocument();
  });
});

describe("SchedulersView run-now confirm flow (#337)", () => {
  it("requires the two-step confirm with the honest not-a-tick label before POSTing", async () => {
    mockApi(true);
    const user = userEvent.setup();
    renderView();
    const buttons = await screen.findAllByText("Run now");

    // Step 1: arm. Nothing fired yet; the honest label is on screen.
    await user.click(buttons[0]);
    expect(apiFleet.runSchedulerEntry).not.toHaveBeenCalled();
    expect(
      screen.getByText(/Enqueues a fresh task with this entry's type\/payload — not a scheduler\s+tick/)
    ).toBeInTheDocument();

    // Step 2: confirm → exactly one POST with the row's stable key. GONE
    // rows sort first, so buttons[0] belongs to the gone email:digest row.
    await user.click(screen.getByText("Confirm run"));
    await waitFor(() => expect(apiFleet.runSchedulerEntry).toHaveBeenCalledTimes(1));
    expect(apiFleet.runSchedulerEntry).toHaveBeenCalledWith("b".repeat(64));
  });

  it("cancel disarms without POSTing", async () => {
    mockApi(true);
    const user = userEvent.setup();
    renderView();
    const buttons = await screen.findAllByText("Run now");
    await user.click(buttons[0]);
    await user.click(screen.getByText("Cancel"));
    expect(apiFleet.runSchedulerEntry).not.toHaveBeenCalled();
    // Back to the armed-off state.
    expect(screen.queryByText("Confirm run")).not.toBeInTheDocument();
    expect((await screen.findAllByText("Run now")).length).toBe(2);
  });

  it("arming a row does not toggle the row expand", async () => {
    mockApi(true);
    vi.mocked(apiFleet.getSchedulerOutcomes).mockResolvedValue({
      stable_key: "a".repeat(64),
      queue: "reports",
      retention_seconds: 0,
      history_cap: 1000,
      events: [],
    });
    const user = userEvent.setup();
    renderView();
    const buttons = await screen.findAllByText("Run now");
    await user.click(buttons[0]);
    // The expand panel (outcome trace) must NOT have opened from that click.
    expect(screen.queryByText("Outcome trace")).not.toBeInTheDocument();
  });
});
