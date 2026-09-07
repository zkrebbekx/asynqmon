import { describe, it, expect, vi, beforeEach } from "vitest";

// Mock axios: `create` returns one spy so every api module's `http(...)`
// call records the exact URL it sends.
const { httpSpy } = vi.hoisted(() => ({
  httpSpy: vi.fn(async () => ({ data: {} })),
}));
vi.mock("axios", () => ({
  default: {
    create: vi.fn(() => httpSpy),
    isAxiosError: () => false,
  },
}));

import axios from "axios";
import * as api from "./api";
import * as fleet from "./api-fleet";
import * as errors from "./api-errors";
import * as views from "./api-views";
import * as hygiene from "./api-hygiene";

const QUEUE = "a#b/c?d";
const QUEUE_ENC = "a%23b%2Fc%3Fd";
const TASK = "50%off";
const TASK_ENC = "50%25off";
const base = () => `${window.location.origin}/api`;

function lastUrl(): string {
  const call = httpSpy.mock.calls.at(-1) as unknown as [{ url: string }] | undefined;
  return call?.[0].url ?? "";
}

describe("api path-segment encoding (#49)", () => {
  beforeEach(() => {
    window.ROOT_PATH = "";
    httpSpy.mockClear();
  });

  it("creates one shared client with a 15 s timeout", () => {
    expect(axios.create).toHaveBeenCalledWith({ timeout: 15_000 });
  });

  it("escapes the queue name in the queue routes", async () => {
    await api.deleteQueue(QUEUE);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}`);
    await api.pauseQueue(QUEUE);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}:pause`);
    await api.listGroups(QUEUE);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}/groups`);
  });

  it("escapes the queue name and the task id in the task routes", async () => {
    await api.deletePendingTask(QUEUE, TASK);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}/pending_tasks/${TASK_ENC}`);
    await api.runRetryTask(QUEUE, TASK);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}/retry_tasks/${TASK_ENC}:run`);
    await api.cancelActiveTask(QUEUE, TASK);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}/active_tasks/${TASK_ENC}:cancel`);
    await api.getTaskInfo(QUEUE, TASK);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}/tasks/${TASK_ENC}`);
  });

  it("escapes the group name in the aggregating routes", async () => {
    await api.deleteAggregatingTask(QUEUE, "g/1", TASK);
    expect(lastUrl()).toBe(
      `${base()}/queues/${QUEUE_ENC}/groups/g%2F1/aggregating_tasks/${TASK_ENC}`
    );
  });

  it("escapes the job id, the scheduler key, the signature, the view id and the hygiene kind", async () => {
    await api.cancelJob("jb#1");
    expect(lastUrl()).toBe(`${base()}/jobs/jb%231/cancel`);
    await fleet.runSchedulerEntry("key/with?chars");
    expect(lastUrl()).toBe(`${base()}/schedulers/key%2Fwith%3Fchars/run`);
    await fleet.getRetryHistogram(QUEUE);
    expect(lastUrl()).toBe(`${base()}/queues/${QUEUE_ENC}/retry_histogram`);
    await errors.getErrorSignature("sig#1");
    expect(lastUrl()).toBe(`${base()}/errors/signatures/sig%231`);
    await views.deleteView("v?1");
    expect(lastUrl()).toBe(`${base()}/views/v%3F1`);
    await hygiene.getHygieneReport("dead-letter");
    expect(lastUrl()).toBe(`${base()}/hygiene/dead-letter`);
  });
});

describe("isAddressableName", () => {
  it("accepts every name without a slash", () => {
    expect(api.isAddressableName("a#b?c%d")).toBe(true);
    expect(api.isAddressableName("critical")).toBe(true);
  });
  it("rejects a name that contains a slash", () => {
    expect(api.isAddressableName("team/billing")).toBe(false);
  });
});
