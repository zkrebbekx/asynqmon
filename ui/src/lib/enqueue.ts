// Pure logic for the clone-and-edit-then-enqueue flow (build contract §3.5 →
// §5.10): prefilling the modal from a source task, payload JSON/base64
// validation states, and mapping the form draft onto the wire request.
// Kept free of React/axios so it is unit-testable.

import { EnqueueTaskRequest, TaskInfo } from "../api";

// CloneDraft is the modal's form state. Numeric fields are kept as strings
// ("" = absent → asynq default server-side) so the operator can clear them.
export interface CloneDraft {
  type: string;
  queue: string;
  payload: string;
  payloadBase64: boolean;
  maxRetry: string;
  timeoutSeconds: string;
  retentionSeconds: string;
  uniqueTtlSeconds: string;
  processInSeconds: string;
  reason: string;
}

// prefillFromTask maps a source task onto the draft. Only fields asynq
// actually stores on TaskInfo are prefilled (type, queue, payload, max_retry,
// timeout); retention, uniqueness TTL and scheduling are NOT recoverable from
// a task record, so they start blank rather than inventing values.
export function prefillFromTask(task: TaskInfo): CloneDraft {
  return {
    type: task.type,
    queue: task.queue,
    payload: task.payload ?? "",
    payloadBase64: false,
    maxRetry: String(task.max_retry),
    timeoutSeconds: task.timeout_seconds > 0 ? String(task.timeout_seconds) : "",
    retentionSeconds: "",
    uniqueTtlSeconds: "",
    processInSeconds: "",
    reason: "",
  };
}

export type PayloadJsonState = "empty" | "valid" | "invalid";

// payloadJsonState powers the validate-on-type note: "valid" renders the
// green JSON check, "invalid" the "will submit as raw string" note, "empty"
// neither. Base64 mode bypasses this — the payload is binary, not JSON.
export function payloadJsonState(text: string): PayloadJsonState {
  if (text.trim() === "") return "empty";
  try {
    JSON.parse(text);
    return "valid";
  } catch {
    return "invalid";
  }
}

// isLikelyBase64 mirrors the server's decoder closely enough for inline
// feedback: standard alphabet, optional padding, whitespace tolerated.
export function isLikelyBase64(text: string): boolean {
  const compact = text.replace(/\s/g, "");
  if (compact.length % 4 !== 0) return false;
  return /^[A-Za-z0-9+/]*={0,2}$/.test(compact);
}

// Client-side mirrors of the server bounds (enqueue.go). The server stays
// authoritative; these exist for instant field-level feedback.
export const ENQUEUE_BOUNDS = {
  maxRetry: 1000,
  timeoutSeconds: 7 * 24 * 60 * 60,
  retentionSeconds: 90 * 24 * 60 * 60,
  uniqueTtlSeconds: 30 * 24 * 60 * 60,
  processInSeconds: 365 * 24 * 60 * 60,
} as const;

const intField = (
  value: string,
  label: string,
  min: number,
  max: number
): string | null => {
  if (value.trim() === "") return null;
  const n = Number(value);
  if (!Number.isInteger(n)) return `${label}: must be a whole number`;
  if (n < min) return `${label}: must be >= ${min}`;
  if (n > max) return `${label}: must be <= ${max}`;
  return null;
};

// draftErrors returns field → message for everything the client can check.
// An empty object means the draft is submittable.
export function draftErrors(draft: CloneDraft): Record<string, string> {
  const errs: Record<string, string> = {};
  if (draft.type.trim() === "") errs.type = "required";
  if (draft.queue.trim() === "") errs.queue = "required";
  if (draft.payloadBase64 && draft.payload !== "" && !isLikelyBase64(draft.payload)) {
    errs.payload = "not valid base64";
  }
  const b = ENQUEUE_BOUNDS;
  const checks: Array<[keyof typeof b, string, string, number]> = [
    ["maxRetry", draft.maxRetry, "max retry", 0],
    ["timeoutSeconds", draft.timeoutSeconds, "timeout", 0],
    ["retentionSeconds", draft.retentionSeconds, "retention", 0],
    ["uniqueTtlSeconds", draft.uniqueTtlSeconds, "unique TTL", 1],
    ["processInSeconds", draft.processInSeconds, "process in", 0],
  ];
  for (const [key, value, label, min] of checks) {
    const err = intField(value, label, min, b[key]);
    if (err) errs[key] = err;
  }
  return errs;
}

// buildEnqueueRequest maps a (valid) draft onto the wire body, omitting
// every blank optional so the server applies asynq defaults.
export function buildEnqueueRequest(draft: CloneDraft): EnqueueTaskRequest {
  const req: EnqueueTaskRequest = {
    type: draft.type.trim(),
    payload: draft.payloadBase64 ? draft.payload.replace(/\s/g, "") : draft.payload,
  };
  if (draft.payloadBase64) req.payload_base64 = true;
  if (draft.maxRetry.trim() !== "") req.max_retry = Number(draft.maxRetry);
  if (draft.timeoutSeconds.trim() !== "") req.timeout_seconds = Number(draft.timeoutSeconds);
  if (draft.retentionSeconds.trim() !== "") req.retention_seconds = Number(draft.retentionSeconds);
  if (draft.uniqueTtlSeconds.trim() !== "") req.unique_ttl_seconds = Number(draft.uniqueTtlSeconds);
  if (draft.processInSeconds.trim() !== "") req.process_in_seconds = Number(draft.processInSeconds);
  if (draft.reason.trim() !== "") req.reason = draft.reason.trim();
  return req;
}
