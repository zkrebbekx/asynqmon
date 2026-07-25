import { describe, expect, it } from "vitest";
import { TaskInfo } from "../api";
import {
  ENQUEUE_BOUNDS,
  buildEnqueueRequest,
  draftErrors,
  isLikelyBase64,
  payloadJsonState,
  prefillFromTask,
} from "./enqueue";

const sourceTask: TaskInfo = {
  id: "5f1a...c3",
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

describe("prefillFromTask (§3.5 clone-and-edit prefill)", () => {
  it("maps the fields asynq actually stores: type, queue, payload, retry, timeout", () => {
    const d = prefillFromTask(sourceTask);
    expect(d.type).toBe("email:send");
    expect(d.queue).toBe("emails");
    expect(d.payload).toBe('{"to":"ops@example.com"}');
    expect(d.maxRetry).toBe("7");
    expect(d.timeoutSeconds).toBe("90");
  });

  it("leaves unknowable fields blank instead of inventing values", () => {
    const d = prefillFromTask(sourceTask);
    expect(d.retentionSeconds).toBe("");
    expect(d.uniqueTtlSeconds).toBe("");
    expect(d.processInSeconds).toBe("");
    expect(d.payloadBase64).toBe(false);
    expect(d.reason).toBe("");
  });

  it("prefills an empty timeout when the task has none (0 = asynq default)", () => {
    const d = prefillFromTask({ ...sourceTask, timeout_seconds: 0 });
    expect(d.timeoutSeconds).toBe("");
  });

  it("tolerates an empty payload", () => {
    const d = prefillFromTask({ ...sourceTask, payload: "" });
    expect(d.payload).toBe("");
  });
});

describe("payloadJsonState (validate-on-type note)", () => {
  it("reports valid for parseable JSON", () => {
    expect(payloadJsonState('{"a":1}')).toBe("valid");
    expect(payloadJsonState("[1,2]")).toBe("valid");
    expect(payloadJsonState('"str"')).toBe("valid");
  });

  it("reports invalid for anything else — submitted as a raw string", () => {
    expect(payloadJsonState("{a:1}")).toBe("invalid");
    expect(payloadJsonState("plain text")).toBe("invalid");
    expect(payloadJsonState('{"a":')).toBe("invalid");
  });

  it("reports empty for blank input", () => {
    expect(payloadJsonState("")).toBe("empty");
    expect(payloadJsonState("   \n")).toBe("empty");
  });
});

describe("isLikelyBase64 (binary payload mode)", () => {
  it("accepts standard base64 with and without padding or whitespace", () => {
    expect(isLikelyBase64("aGVsbG8=")).toBe(true);
    expect(isLikelyBase64("aGVs\nbG8=")).toBe(true);
    expect(isLikelyBase64("")).toBe(true);
  });

  it("rejects the invalid alphabet and bad lengths", () => {
    expect(isLikelyBase64("not-*-base64")).toBe(false);
    expect(isLikelyBase64("abc")).toBe(false);
  });
});

describe("draftErrors (client mirror of the §5.10 bounds)", () => {
  const valid = prefillFromTask(sourceTask);

  it("passes a prefilled draft untouched", () => {
    expect(draftErrors(valid)).toEqual({});
  });

  it("requires type and queue", () => {
    expect(draftErrors({ ...valid, type: " " }).type).toBe("required");
    expect(draftErrors({ ...valid, queue: "" }).queue).toBe("required");
  });

  it("checks base64 validity only in base64 mode", () => {
    expect(draftErrors({ ...valid, payload: "not-*-base64" }).payload).toBeUndefined();
    expect(
      draftErrors({ ...valid, payload: "not-*-base64", payloadBase64: true }).payload
    ).toMatch(/base64/);
  });

  it("enforces the numeric bounds", () => {
    expect(draftErrors({ ...valid, maxRetry: "-1" }).maxRetry).toMatch(/>= 0/);
    expect(
      draftErrors({ ...valid, maxRetry: String(ENQUEUE_BOUNDS.maxRetry + 1) }).maxRetry
    ).toMatch(/<=/);
    expect(draftErrors({ ...valid, uniqueTtlSeconds: "0" }).uniqueTtlSeconds).toMatch(/>= 1/);
    expect(draftErrors({ ...valid, timeoutSeconds: "1.5" }).timeoutSeconds).toMatch(/whole/);
    expect(draftErrors({ ...valid, processInSeconds: "abc" }).processInSeconds).toMatch(/whole/);
  });
});

describe("buildEnqueueRequest (draft → wire body)", () => {
  it("carries set fields and omits blanks so asynq defaults apply", () => {
    const req = buildEnqueueRequest(prefillFromTask(sourceTask));
    expect(req).toEqual({
      type: "email:send",
      payload: '{"to":"ops@example.com"}',
      max_retry: 7,
      timeout_seconds: 90,
    });
    // No base64 flag, no retention/unique/schedule, no reason.
    expect("payload_base64" in req).toBe(false);
    expect("retention_seconds" in req).toBe(false);
  });

  it("maps every optional field when present", () => {
    const req = buildEnqueueRequest({
      ...prefillFromTask(sourceTask),
      payloadBase64: true,
      payload: "aGVs bG8=",
      retentionSeconds: "3600",
      uniqueTtlSeconds: "60",
      processInSeconds: "300",
      reason: "  re-drain edited payload  ",
    });
    expect(req.payload_base64).toBe(true);
    // Whitespace is stripped from base64 payloads before submit.
    expect(req.payload).toBe("aGVsbG8=");
    expect(req.retention_seconds).toBe(3600);
    expect(req.unique_ttl_seconds).toBe(60);
    expect(req.process_in_seconds).toBe(300);
    expect(req.reason).toBe("re-drain edited payload");
  });
});
