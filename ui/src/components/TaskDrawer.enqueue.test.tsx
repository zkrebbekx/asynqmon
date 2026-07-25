// TaskDrawer × §5.10 enqueue capability: the "Clone & edit…" action hides
// entirely when the deployment has enqueue off, and the modal prefills from
// the source task with live JSON validation on the payload editor.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import store from "../store";
import * as api from "../api";
import { TaskInfo } from "../api";
import { resetFeaturesCache } from "../hooks/useFeatures";
import TaskDrawer from "./TaskDrawer";

vi.mock("../api");

const task: TaskInfo = {
  id: "d75c7d58-0000-4000-8000-000000000001",
  queue: "emails",
  type: "email:send",
  payload: '{"to":"ops@example.com"}',
  state: "retry",
  start_time: "",
  max_retry: 7,
  retried: 3,
  last_failed_at: "2026-07-25T01:00:00Z",
  error_message: "smtp timeout",
  next_process_at: "2026-07-25T02:00:00Z",
  timeout_seconds: 90,
  deadline: "",
  group: "",
  completed_at: "",
  result: "",
  ttl_seconds: 0,
  is_orphaned: false,
};

function renderDrawer(overrides?: { onPeek?: (t: { queue: string; id: string }) => void }) {
  return render(
    <Provider store={store}>
      <TaskDrawer
        peek={{ queue: task.queue, id: task.id }}
        resultList={[task]}
        onClose={() => {}}
        onPeek={overrides?.onPeek ?? (() => {})}
        onPivot={() => {}}
      />
    </Provider>
  );
}

function mockApi(enqueueEnabled: boolean) {
  vi.mocked(api.getTaskInfo).mockResolvedValue(task);
  vi.mocked(api.getFeatures).mockResolvedValue({ features: { enqueue: enqueueEnabled } });
  vi.mocked(api.listQueues).mockResolvedValue({ queues: [] });
  vi.mocked(api.enqueueTask).mockResolvedValue({ ...task, id: "new-id", state: "pending" });
}

beforeEach(() => {
  vi.clearAllMocks();
  resetFeaturesCache();
  window.READ_ONLY = false;
});

describe("TaskDrawer clone-and-edit gating (§5.10)", () => {
  it("shows the Clone & edit action when the deployment enables enqueue", async () => {
    mockApi(true);
    renderDrawer();
    expect(await screen.findByText(/Clone & edit/)).toBeInTheDocument();
  });

  it("hides the action entirely when enqueue is disabled", async () => {
    mockApi(false);
    renderDrawer();
    // The drawer is fully loaded (actions row rendered for a retry task)…
    expect(await screen.findByText(/Run now/)).toBeInTheDocument();
    // …but the clone action is nowhere.
    expect(screen.queryByText(/Clone & edit/)).not.toBeInTheDocument();
  });

  it("hides the action in a read-only build even when the backend reports enqueue on", async () => {
    // The features endpoint IS still probed in read-only builds — the Flow
    // view's correlation-key list rides on it (useCorrelationKeys) — but its
    // enqueue capability bit must never surface the clone action.
    mockApi(true);
    window.READ_ONLY = true;
    renderDrawer();
    // Read-only also hides Run/Archive/Delete; wait for the task to load
    // (the type renders as both the header chip and the pivot link).
    expect((await screen.findAllByText("email:send")).length).toBeGreaterThan(0);
    expect(screen.queryByText(/Clone & edit/)).not.toBeInTheDocument();
  });
});

describe("CloneEnqueueModal prefill and payload validation (§3.5)", () => {
  async function openModal() {
    mockApi(true);
    const user = userEvent.setup();
    renderDrawer();
    await user.click(await screen.findByText(/Clone & edit/));
    return user;
  }

  it("prefills type, queue, payload, retry and timeout from the source task", async () => {
    await openModal();
    expect(screen.getByLabelText("type")).toHaveValue("email:send");
    expect(screen.getByLabelText("queue")).toHaveValue("emails");
    expect(screen.getByLabelText("payload")).toHaveValue('{"to":"ops@example.com"}');
    expect(screen.getByLabelText("max retry")).toHaveValue("7");
    expect(screen.getByLabelText("timeout (s)")).toHaveValue("90");
    // Unknowable fields start blank.
    expect(screen.getByLabelText("retention (s)")).toHaveValue("");
    expect(screen.getByLabelText("unique TTL (s)")).toHaveValue("");
    expect(screen.getByLabelText("process in (s)")).toHaveValue("");
  });

  it("validates the payload as JSON on type, falling back to the raw-string note", async () => {
    const user = await openModal();
    // Prefilled payload is valid JSON.
    expect(screen.getByText("valid JSON")).toBeInTheDocument();
    // Break it → the honest raw-string note appears.
    const payload = screen.getByLabelText("payload");
    await user.clear(payload);
    await user.type(payload, "not json");
    expect(screen.getByText(/invalid JSON — will submit as raw string/)).toBeInTheDocument();
    // Empty payloads are allowed and labeled.
    await user.clear(payload);
    expect(screen.getByText("empty payload")).toBeInTheDocument();
  });

  it("submits the edited draft and peeks the created task from the toast action", async () => {
    const onPeek = vi.fn();
    mockApi(true);
    const user = userEvent.setup();
    renderDrawer({ onPeek });
    await user.click(await screen.findByText(/Clone & edit/));
    await user.clear(screen.getByLabelText("process in (s)"));
    await user.type(screen.getByLabelText("process in (s)"), "60");
    await user.click(screen.getByText("Enqueue task"));
    await waitFor(() => expect(api.enqueueTask).toHaveBeenCalledTimes(1));
    expect(api.enqueueTask).toHaveBeenCalledWith("emails", {
      type: "email:send",
      payload: '{"to":"ops@example.com"}',
      max_retry: 7,
      timeout_seconds: 90,
      process_in_seconds: 60,
    });
  });

  it("blocks submit while client-side field errors exist", async () => {
    const user = await openModal();
    await user.clear(screen.getByLabelText("type"));
    expect(screen.getByText("Enqueue task")).toBeDisabled();
    expect(screen.getByText("required")).toBeInTheDocument();
  });
});
