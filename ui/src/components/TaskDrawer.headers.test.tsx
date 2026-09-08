// TaskDrawer × asynq 0.26 task headers (#44): the drawer shows the header map
// read-only, and the clone-and-edit modal prefills, edits and submits it. The
// read side reports TaskInfo.headers, so a clone that dropped them silently
// lost the source task's trace context.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import store from "../store";
import * as api from "../api";
import { TaskInfo } from "../api";
import { HEADER_BOUNDS } from "../lib/enqueue";
import { resetFeaturesCache } from "../hooks/useFeatures";
import TaskDrawer from "./TaskDrawer";

vi.mock("../api");

const baseTask: TaskInfo = {
  id: "d75c7d58-0000-4000-8000-000000000002",
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

const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

const taskWithHeaders: TaskInfo = {
  ...baseTask,
  headers: { traceparent, tenant: "acme" },
};

function mockApi(task: TaskInfo) {
  vi.mocked(api.getTaskInfo).mockResolvedValue(task);
  vi.mocked(api.getFeatures).mockResolvedValue({ features: { enqueue: true } });
  vi.mocked(api.listQueues).mockResolvedValue({ queues: [] });
  vi.mocked(api.enqueueTask).mockResolvedValue({ ...task, id: "new-id", state: "pending" });
}

function renderDrawer(task: TaskInfo) {
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

// openModal renders the drawer for the given task and opens the clone modal.
async function openModal(task: TaskInfo) {
  mockApi(task);
  const user = userEvent.setup();
  renderDrawer(task);
  await user.click(await screen.findByText(/Clone & edit/));
  return user;
}

beforeEach(() => {
  vi.clearAllMocks();
  resetFeaturesCache();
  window.READ_ONLY = false;
});

describe("TaskDrawer header display", () => {
  it("shows every header of the task read-only, sorted by name", async () => {
    mockApi(taskWithHeaders);
    renderDrawer(taskWithHeaders);
    expect(await screen.findByText("headers")).toBeInTheDocument();
    expect(screen.getByText("tenant")).toBeInTheDocument();
    expect(screen.getByText("acme")).toBeInTheDocument();
    expect(screen.getByText("traceparent")).toBeInTheDocument();
    expect(screen.getByText(traceparent)).toBeInTheDocument();
  });

  it("shows no header block at all when the task carries none", async () => {
    mockApi(baseTask);
    renderDrawer(baseTask);
    // Wait for the task to load (the type renders as chip and pivot link).
    expect((await screen.findAllByText("email:send")).length).toBeGreaterThan(0);
    expect(screen.queryByText("headers")).not.toBeInTheDocument();
  });
});

describe("CloneEnqueueModal header editing (#44)", () => {
  it("prefills a row per header of the cloned task", async () => {
    await openModal(taskWithHeaders);
    expect(screen.getByLabelText("header 1 name")).toHaveValue("tenant");
    expect(screen.getByLabelText("header 1 value")).toHaveValue("acme");
    expect(screen.getByLabelText("header 2 name")).toHaveValue("traceparent");
    expect(screen.getByLabelText("header 2 value")).toHaveValue(traceparent);
  });

  it("says so plainly when the cloned task has no headers", async () => {
    await openModal(baseTask);
    expect(screen.getByText(/no headers/)).toBeInTheDocument();
    expect(screen.queryByLabelText("header 1 name")).not.toBeInTheDocument();
  });

  it("adds a row and submits the new header with the request", async () => {
    const user = await openModal(baseTask);
    await user.click(screen.getByText("+ add header"));
    await user.type(screen.getByLabelText("header 1 name"), "tenant");
    await user.type(screen.getByLabelText("header 1 value"), "acme");
    await user.click(screen.getByText("Enqueue task"));
    await waitFor(() => expect(api.enqueueTask).toHaveBeenCalledTimes(1));
    expect(api.enqueueTask).toHaveBeenCalledWith("emails", {
      type: "email:send",
      payload: '{"to":"ops@example.com"}',
      max_retry: 7,
      timeout_seconds: 90,
      headers: { tenant: "acme" },
    });
  });

  it("removes a row and submits only the remaining header", async () => {
    const user = await openModal(taskWithHeaders);
    await user.click(screen.getByLabelText("remove header 1"));
    expect(screen.getByLabelText("header 1 name")).toHaveValue("traceparent");
    expect(screen.queryByLabelText("header 2 name")).not.toBeInTheDocument();
    await user.click(screen.getByText("Enqueue task"));
    await waitFor(() => expect(api.enqueueTask).toHaveBeenCalledTimes(1));
    expect(vi.mocked(api.enqueueTask).mock.calls[0][1].headers).toEqual({ traceparent });
  });

  it("submits the cloned headers unchanged when the operator edits nothing", async () => {
    const user = await openModal(taskWithHeaders);
    await user.click(screen.getByText("Enqueue task"));
    await waitFor(() => expect(api.enqueueTask).toHaveBeenCalledTimes(1));
    expect(vi.mocked(api.enqueueTask).mock.calls[0][1].headers).toEqual({
      tenant: "acme",
      traceparent,
    });
  });

  it("blocks submit with an inline message when a header name is blank", async () => {
    const user = await openModal(baseTask);
    await user.click(screen.getByText("+ add header"));
    // A row the operator has not touched is neither an error nor submitted.
    expect(screen.getByText("Enqueue task")).not.toBeDisabled();
    await user.type(screen.getByLabelText("header 1 value"), "acme");
    expect(screen.getByText("a header name must not be blank")).toBeInTheDocument();
    expect(screen.getByText("Enqueue task")).toBeDisabled();
    expect(api.enqueueTask).not.toHaveBeenCalled();
  });

  it("blocks submit with an inline message on a duplicate header name", async () => {
    const user = await openModal(taskWithHeaders);
    await user.clear(screen.getByLabelText("header 2 name"));
    await user.type(screen.getByLabelText("header 2 name"), "tenant");
    expect(screen.getByText("duplicate header name")).toBeInTheDocument();
    expect(screen.getByText("Enqueue task")).toBeDisabled();
  });

  it("blocks submit with an inline message when a header value is too long", async () => {
    const user = await openModal(baseTask);
    await user.click(screen.getByText("+ add header"));
    await user.type(screen.getByLabelText("header 1 name"), "big");
    // Typing 4097 characters is slow; paste the over-long value instead.
    await user.click(screen.getByLabelText("header 1 value"));
    await user.paste("x".repeat(HEADER_BOUNDS.maxValueBytes + 1));
    expect(
      screen.getByText(`value exceeds ${HEADER_BOUNDS.maxValueBytes} bytes`)
    ).toBeInTheDocument();
    expect(screen.getByText("Enqueue task")).toBeDisabled();
  });

  it("states the server bounds so the operator does not meet them as a 400", async () => {
    await openModal(baseTask);
    expect(
      screen.getByText(
        new RegExp(
          `at most ${HEADER_BOUNDS.maxEntries} headers.*${HEADER_BOUNDS.maxNameBytes} bytes.*${HEADER_BOUNDS.maxValueBytes} bytes`
        )
      )
    ).toBeInTheDocument();
  });
});
