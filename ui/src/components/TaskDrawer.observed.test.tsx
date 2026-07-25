// TaskDrawer × observed run history: the "Attempt history" section and the
// lifecycle "last run … · observed" stamp render ONLY from data recorded by
// the opt-in observe middleware, are labeled as observed (honesty ledger),
// and leave no trace — no empty section — when the data is absent.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { Provider } from "react-redux";
import store from "../store";
import * as api from "../api";
import { TaskInfo, ObservedTaskResponse } from "../api";
import { resetFeaturesCache } from "../hooks/useFeatures";
import TaskDrawer from "./TaskDrawer";

vi.mock("../api");

const task: TaskInfo = {
  id: "0b5c7d58-0000-4000-8000-00000000obs1",
  queue: "gw",
  type: "charge:settle",
  payload: '{"charge_id":"ch_001"}',
  state: "completed",
  start_time: "",
  max_retry: 5,
  retried: 2,
  last_failed_at: "",
  error_message: "",
  next_process_at: "",
  timeout_seconds: 0,
  deadline: "",
  group: "",
  completed_at: "2026-07-25T01:00:00Z",
  result: "",
  ttl_seconds: 3600,
  is_orphaned: false,
};

const observed: ObservedTaskResponse = {
  present: true,
  attempts: [
    {
      n: 3,
      start: "2026-07-25T01:00:00Z",
      duration_ms: 3400,
      outcome: "ok",
      worker: "worker-a:4242",
    },
    {
      n: 2,
      start: "2026-07-25T00:59:00Z",
      duration_ms: 120,
      outcome: "error",
      error: "gateway timeout POST /v2/charges",
      worker: "worker-a:4242",
    },
    {
      n: 1,
      start: "2026-07-25T00:58:00Z",
      duration_ms: 95,
      outcome: "panic",
      error: "boom: nil deref",
      worker: "worker-b:7",
    },
  ],
  summary: {
    first_seen: "2026-07-25T00:58:00Z",
    total_attempts: 3,
    last_duration_ms: 3400,
    last_outcome: "ok",
    total_busy_ms: 3615,
  },
};

function renderDrawer() {
  return render(
    <Provider store={store}>
      <TaskDrawer
        peek={{ queue: task.queue, id: task.id }}
        resultList={[task]}
        onClose={() => {}}
        onPeek={() => {}}
        onPivot={() => {}}
      />
    </Provider>
  );
}

function mockApi(obs: ObservedTaskResponse | null) {
  vi.mocked(api.getTaskInfo).mockResolvedValue(task);
  vi.mocked(api.getObservedTask).mockResolvedValue(obs);
  vi.mocked(api.getFeatures).mockResolvedValue({ features: { enqueue: false } });
  vi.mocked(api.listQueues).mockResolvedValue({ queues: [] });
}

beforeEach(() => {
  vi.clearAllMocks();
  resetFeaturesCache();
  window.READ_ONLY = false;
});

describe("TaskDrawer attempt history (observe middleware)", () => {
  it("renders the section with one row per recorded attempt when data is present", async () => {
    mockApi(observed);
    renderDrawer();
    expect(await screen.findByText(/Attempt history/)).toBeInTheDocument();
    // Per-attempt rows: number, duration, outcome chip, worker, error line.
    expect(screen.getByText("#3")).toBeInTheDocument();
    expect(screen.getByText("#2")).toBeInTheDocument();
    expect(screen.getByText("#1")).toBeInTheDocument();
    expect(screen.getAllByText("3.4s").length).toBeGreaterThan(0);
    expect(screen.getByText("120ms")).toBeInTheDocument();
    expect(screen.getByText("ok")).toBeInTheDocument();
    expect(screen.getByText("error")).toBeInTheDocument();
    expect(screen.getByText("panic")).toBeInTheDocument();
    expect(screen.getAllByText("worker-a:4242")).toHaveLength(2);
    expect(screen.getByText("worker-b:7")).toBeInTheDocument();
    expect(screen.getByText("gateway timeout POST /v2/charges")).toBeInTheDocument();
  });

  it("carries the adoption-honesty label on the section", async () => {
    mockApi(observed);
    renderDrawer();
    expect(
      await screen.findByText(
        /recorded by observe middleware — attempts before adoption are not shown/
      )
    ).toBeInTheDocument();
  });

  it("shows the observed last-run duration in the lifecycle", async () => {
    mockApi(observed);
    renderDrawer();
    expect(await screen.findByText("last run")).toBeInTheDocument();
    // The lifecycle stamp: "last run 3.4s · observed".
    expect(screen.getAllByText("3.4s").length).toBeGreaterThan(0);
    expect(screen.getByText("observed")).toBeInTheDocument();
  });

  it("counts hidden attempts honestly when the cap trimmed the list", async () => {
    mockApi({
      ...observed,
      summary: { ...observed.summary!, total_attempts: 12 },
    });
    renderDrawer();
    // 3 records kept of 12 observed → the header says so.
    expect(await screen.findByText(/Attempt history · 3 of 12/)).toBeInTheDocument();
  });

  it("renders no section, stamp or label at all when no data was recorded (404 → null)", async () => {
    mockApi(null);
    renderDrawer();
    // Drawer fully loaded…
    expect(await screen.findByText("Lifecycle")).toBeInTheDocument();
    // …with zero observed artifacts.
    expect(screen.queryByText(/Attempt history/)).not.toBeInTheDocument();
    expect(screen.queryByText("last run")).not.toBeInTheDocument();
    expect(screen.queryByText("observed")).not.toBeInTheDocument();
    expect(screen.queryByText(/recorded by observe middleware/)).not.toBeInTheDocument();
  });

  it("renders no section when the backend answers present=false", async () => {
    mockApi({ present: false, attempts: [], summary: null });
    renderDrawer();
    expect(await screen.findByText("Lifecycle")).toBeInTheDocument();
    expect(screen.queryByText(/Attempt history/)).not.toBeInTheDocument();
    expect(screen.queryByText("last run")).not.toBeInTheDocument();
  });

  it("shows the lifecycle stamp without an attempt section when only the summary survives", async () => {
    // The summary hash can outlive the list (or the cap can be 0-effective);
    // the lifecycle stamp still works, the empty section does not render.
    mockApi({ present: true, attempts: [], summary: observed.summary });
    renderDrawer();
    expect(await screen.findByText("last run")).toBeInTheDocument();
    expect(screen.queryByText(/Attempt history/)).not.toBeInTheDocument();
  });
});
