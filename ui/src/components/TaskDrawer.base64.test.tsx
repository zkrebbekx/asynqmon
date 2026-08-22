// TaskDrawer × base64 payload fields: when a payload carries reliably
// detected base64-encoded values, the drawer shows the DECODED rendering by
// default with a decoded/raw toggle and an honesty note naming the decoded
// fields; payloads with no detections render raw with no toggle.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";
import { Provider } from "react-redux";
import store from "../store";
import * as api from "../api";
import { TaskInfo } from "../api";
import { resetFeaturesCache } from "../hooks/useFeatures";
import TaskDrawer from "./TaskDrawer";

vi.mock("../api");

const b64 = (s: string) => Buffer.from(s, "utf-8").toString("base64");

const baseTask: TaskInfo = {
  id: "d75c7d58-0000-4000-8000-000000000003",
  queue: "webhooks",
  type: "webhook:deliver",
  payload: "",
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

beforeEach(() => {
  vi.clearAllMocks();
  resetFeaturesCache();
  window.READ_ONLY = false;
  vi.mocked(api.getFeatures).mockResolvedValue({ features: { enqueue: false } });
});

describe("TaskDrawer base64 payload decoding", () => {
  const inner = '{"event":"order.updated","order_id":"ord_00042"}';
  const payload = JSON.stringify({ envelope: b64(inner), attempt: 1 });

  it("shows the decoded rendering by default with an honesty note", async () => {
    renderDrawer({ ...baseTask, payload });
    // Decoded content visible (the embedded JSON's field value).
    expect((await screen.findAllByText(/order\.updated/)).length).toBeGreaterThan(0);
    // Honesty note names the decoded path.
    expect(screen.getByText(/decoded from base64: envelope/)).toBeInTheDocument();
    // The payload BLOCK does not show the raw base64 by default (the
    // metadata chip row legitimately still shows the raw value).
    const block = screen.getByTestId("decodable-body");
    expect(within(block).queryByText(new RegExp(b64(inner).slice(0, 24)))).not.toBeInTheDocument();
  });

  it("toggles back to the raw payload and again to decoded", async () => {
    renderDrawer({ ...baseTask, payload });
    await screen.findAllByText(/order\.updated/);

    fireEvent.click(screen.getByRole("button", { name: "raw" }));
    expect((await screen.findAllByText(new RegExp(b64(inner).slice(0, 24)))).length).toBeGreaterThan(0);
    expect(screen.getByText(/raw view — 1 base64 field can be decoded/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "decoded" }));
    expect((await screen.findAllByText(/order\.updated/)).length).toBeGreaterThan(0);
  });

  it("renders plain payloads raw with no toggle", async () => {
    renderDrawer({ ...baseTask, payload: '{"user_id":42,"template":"welcome"}' });
    expect((await screen.findAllByText(/welcome/)).length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: "raw" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "decoded" })).not.toBeInTheDocument();
  });
});
