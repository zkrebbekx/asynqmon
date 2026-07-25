// TaskDrawer × full payload on task detail (upstream hibiken/asynqmon#301):
// the payload section shows an honest "truncated at N chars" note when — and
// only when — the server's detail cap (features.payload_detail_limit) capped
// the formatted payload, and the copy-payload button copies the payload text.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { Provider } from "react-redux";
import store from "../store";
import * as api from "../api";
import { TaskInfo } from "../api";
import { resetFeaturesCache } from "../hooks/useFeatures";
import TaskDrawer from "./TaskDrawer";

vi.mock("../api");

const baseTask: TaskInfo = {
  id: "d75c7d58-0000-4000-8000-000000000002",
  queue: "emails",
  type: "email:send",
  payload: '{"to":"ops@example.com"}',
  state: "pending",
  start_time: "",
  max_retry: 25,
  retried: 0,
  last_failed_at: "",
  error_message: "",
  next_process_at: "2026-07-26T02:00:00Z",
  timeout_seconds: 1800,
  deadline: "",
  group: "",
  completed_at: "",
  result: "",
  ttl_seconds: 0,
  is_orphaned: false,
};

// A server-truncated payload is exactly cap+1 code points ending in "…"
// (cmd/asynqmon truncate semantics).
const CAP = 1000;
const truncatedPayload = "x".repeat(CAP) + "…";

function renderDrawer(task: TaskInfo) {
  vi.mocked(api.getTaskInfo).mockResolvedValue(task);
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

function mockFeatures(payloadDetailLimit?: number) {
  vi.mocked(api.getFeatures).mockResolvedValue({
    features: { enqueue: false },
    ...(payloadDetailLimit !== undefined
      ? { payload_detail_limit: payloadDetailLimit }
      : {}),
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  resetFeaturesCache();
  window.READ_ONLY = false;
});

describe("TaskDrawer payload truncation note (#301)", () => {
  it("shows the honest cap note when the payload exceeds the advertised detail cap", async () => {
    mockFeatures(CAP);
    renderDrawer({ ...baseTask, payload: truncatedPayload });
    expect(
      await screen.findByText(/payload truncated at 1,000 chars/)
    ).toBeInTheDocument();
    expect(screen.getByText(/--max-detail-payload-length/)).toBeInTheDocument();
  });

  it("shows no note for a payload within the cap", async () => {
    mockFeatures(CAP);
    renderDrawer(baseTask);
    // Drawer fully loaded (payload rendered)…
    expect((await screen.findAllByText(/ops@example\.com/)).length).toBeGreaterThan(0);
    // …and no truncation claim anywhere.
    expect(screen.queryByText(/payload truncated at/)).not.toBeInTheDocument();
  });

  it("shows no note when the backend does not advertise a cap (older backend)", async () => {
    mockFeatures(undefined);
    renderDrawer({ ...baseTask, payload: truncatedPayload });
    // Drawer fully loaded (the type renders as the header chip + pivot link).
    expect((await screen.findAllByText("email:send")).length).toBeGreaterThan(0);
    expect(screen.queryByText(/payload truncated at/)).not.toBeInTheDocument();
  });
});

describe("TaskDrawer copy-payload button (#301)", () => {
  it("copies the payload text to the clipboard", async () => {
    mockFeatures(CAP);
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    renderDrawer(baseTask);
    fireEvent.click(await screen.findByLabelText("Copy payload"));
    expect(writeText).toHaveBeenCalledWith('{"to":"ops@example.com"}');
  });

  it("offers no copy button when the task has no payload", async () => {
    mockFeatures(CAP);
    renderDrawer({ ...baseTask, payload: "" });
    expect(await screen.findByText("No payload")).toBeInTheDocument();
    expect(screen.queryByLabelText("Copy payload")).not.toBeInTheDocument();
  });
});
