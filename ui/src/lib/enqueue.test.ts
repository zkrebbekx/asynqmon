import { describe, expect, it } from "vitest";
import { TaskInfo } from "../api";
import {
  ENQUEUE_BOUNDS,
  HEADER_BOUNDS,
  buildEnqueueRequest,
  draftErrors,
  headerRowsFromTask,
  headersFromRows,
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

// ----------------------------------------------------------------------------
// asynq 0.26 task headers (#44). The read side reports TaskInfo.headers, so a
// clone must carry them through the draft and back onto the wire body.
// ----------------------------------------------------------------------------

const taskWithHeaders: TaskInfo = {
  ...sourceTask,
  headers: {
    tenant: "acme",
    traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
  },
};

describe("headerRowsFromTask (clone prefill)", () => {
  it("turns the header map into rows sorted by name", () => {
    expect(headerRowsFromTask(taskWithHeaders)).toEqual([
      { name: "tenant", value: "acme" },
      {
        name: "traceparent",
        value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
      },
    ]);
  });

  it("returns no rows when the task has no headers field", () => {
    expect(headerRowsFromTask(sourceTask)).toEqual([]);
  });

  it("returns no rows for an empty header map", () => {
    expect(headerRowsFromTask({ ...sourceTask, headers: {} })).toEqual([]);
  });
});

describe("prefillFromTask headers", () => {
  it("prefills the header rows from the source task", () => {
    expect(prefillFromTask(taskWithHeaders).headers).toEqual([
      { name: "tenant", value: "acme" },
      {
        name: "traceparent",
        value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
      },
    ]);
  });

  it("prefills an empty row list when the task carries no headers", () => {
    expect(prefillFromTask(sourceTask).headers).toEqual([]);
  });
});

describe("header validation (mirrors validateEnqueueHeaders in enqueue.go)", () => {
  const withRows = (headers: Array<{ name: string; value: string }>) => ({
    ...prefillFromTask(sourceTask),
    headers,
  });

  it("accepts a well-formed row", () => {
    expect(draftErrors(withRows([{ name: "tenant", value: "acme" }]))).toEqual({});
  });

  it("ignores a row the operator added and left blank", () => {
    expect(draftErrors(withRows([{ name: "", value: "" }]))).toEqual({});
  });

  it("rejects a blank name once the row has a value", () => {
    expect(draftErrors(withRows([{ name: "  ", value: "acme" }]))["header:0"]).toBe(
      "a header name must not be blank"
    );
  });

  it("rejects a name over the byte cap, counting UTF-8 bytes", () => {
    // "é" is 2 bytes: 129 characters are 258 bytes, over the 256-byte cap.
    const name = "é".repeat(129);
    expect(draftErrors(withRows([{ name, value: "v" }]))["header:0"]).toBe(
      `name exceeds ${HEADER_BOUNDS.maxNameBytes} bytes`
    );
    // One byte under the cap is accepted.
    expect(draftErrors(withRows([{ name: "a".repeat(256), value: "v" }]))).toEqual({});
  });

  it("rejects a value over the byte cap", () => {
    const value = "x".repeat(HEADER_BOUNDS.maxValueBytes + 1);
    expect(draftErrors(withRows([{ name: "big", value }]))["header:0"]).toBe(
      `value exceeds ${HEADER_BOUNDS.maxValueBytes} bytes`
    );
  });

  it("rejects a duplicate name, which the wire map would silently collapse", () => {
    const errs = draftErrors(
      withRows([
        { name: "tenant", value: "acme" },
        { name: "tenant", value: "other" },
      ])
    );
    expect(errs["header:0"]).toBeUndefined();
    expect(errs["header:1"]).toBe("duplicate header name");
  });

  it("rejects more than the entry cap, reporting the count", () => {
    const rows = Array.from({ length: HEADER_BOUNDS.maxEntries + 1 }, (_, i) => ({
      name: `h${i}`,
      value: "v",
    }));
    expect(draftErrors(withRows(rows)).headers).toBe(
      `at most ${HEADER_BOUNDS.maxEntries} headers (got ${HEADER_BOUNDS.maxEntries + 1})`
    );
  });

  it("accepts exactly the entry cap", () => {
    const rows = Array.from({ length: HEADER_BOUNDS.maxEntries }, (_, i) => ({
      name: `h${i}`,
      value: "v",
    }));
    expect(draftErrors(withRows(rows))).toEqual({});
  });
});

describe("headersFromRows", () => {
  it("collapses rows into the wire map and trims the names", () => {
    expect(
      headersFromRows([
        { name: " tenant ", value: "acme" },
        { name: "empty-value", value: "" },
      ])
    ).toEqual({ tenant: "acme", "empty-value": "" });
  });

  it("returns undefined when every row is blank", () => {
    expect(headersFromRows([{ name: "", value: "" }])).toBeUndefined();
    expect(headersFromRows([])).toBeUndefined();
  });
});

describe("buildEnqueueRequest headers", () => {
  it("carries the header rows onto the wire body", () => {
    const req = buildEnqueueRequest(prefillFromTask(taskWithHeaders));
    expect(req.headers).toEqual({
      tenant: "acme",
      traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
    });
  });

  it("omits the field when the draft has no live header row", () => {
    const req = buildEnqueueRequest({
      ...prefillFromTask(sourceTask),
      headers: [{ name: "", value: "" }],
    });
    expect(req).not.toHaveProperty("headers");
  });
});
