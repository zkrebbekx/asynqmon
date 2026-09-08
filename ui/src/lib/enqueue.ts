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
  // headers is an ordered row list, not a map: the modal must keep a row the
  // operator is still typing (a blank name, a duplicate name) on screen, and
  // a map cannot hold either.
  headers: HeaderRow[];
}

// HeaderRow is one editable line of the header table.
export interface HeaderRow {
  name: string;
  value: string;
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
    headers: headerRowsFromTask(task),
  };
}

// headerRowsFromTask turns the task's header map into ordered rows. It sorts
// by name so the same task always prefills the same way; a map has no order.
export function headerRowsFromTask(task: TaskInfo): HeaderRow[] {
  const h = task.headers;
  if (!h) return [];
  return Object.entries(h)
    .map(([name, value]) => ({ name, value }))
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

// isBlankHeaderRow is true for a row the operator added and left untouched.
// Such a row is dropped on submit and raises no error: an empty line must not
// block the form.
export function isBlankHeaderRow(row: HeaderRow): boolean {
  return row.name.trim() === "" && row.value === "";
}

// utf8Bytes counts the bytes the server sees. The server bounds a header name
// and value in BYTES (Go len on a string), so a multi-byte name must be
// measured the same way here.
function utf8Bytes(text: string): number {
  return new TextEncoder().encode(text).length;
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

// Header bounds mirror validateEnqueueHeaders in enqueue.go. The modal
// imports these constants; it never repeats the numbers.
export const HEADER_BOUNDS = {
  maxEntries: 64,
  maxNameBytes: 256,
  maxValueBytes: 4096,
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
  Object.assign(errs, headerErrors(draft.headers));
  return errs;
}

// headerErrors returns row index -> message under the key "header:<index>",
// plus the whole-table message under "headers". The keys are distinct so the
// modal can mark one row red and still show the count error above the table.
// The checks mirror validateEnqueueHeaders in enqueue.go, with one addition
// the server cannot make: two rows with the same name collapse into one JSON
// key, so the operator would silently lose a header.
export function headerErrors(rows: HeaderRow[]): Record<string, string> {
  const errs: Record<string, string> = {};
  const live = rows.filter((r) => !isBlankHeaderRow(r));
  if (live.length > HEADER_BOUNDS.maxEntries) {
    errs.headers = `at most ${HEADER_BOUNDS.maxEntries} headers (got ${live.length})`;
  }
  const seen = new Set<string>();
  rows.forEach((row, i) => {
    if (isBlankHeaderRow(row)) return;
    const name = row.name.trim();
    let msg: string | null = null;
    if (name === "") {
      msg = "a header name must not be blank";
    } else if (utf8Bytes(name) > HEADER_BOUNDS.maxNameBytes) {
      msg = `name exceeds ${HEADER_BOUNDS.maxNameBytes} bytes`;
    } else if (utf8Bytes(row.value) > HEADER_BOUNDS.maxValueBytes) {
      msg = `value exceeds ${HEADER_BOUNDS.maxValueBytes} bytes`;
    } else if (seen.has(name)) {
      msg = "duplicate header name";
    }
    if (name !== "") seen.add(name);
    if (msg) errs[`header:${i}`] = msg;
  });
  return errs;
}

// headersFromRows collapses the rows into the wire map, dropping blank rows.
// It returns undefined when nothing survives, so the body omits the field.
export function headersFromRows(rows: HeaderRow[]): { [name: string]: string } | undefined {
  const out: { [name: string]: string } = {};
  let n = 0;
  for (const row of rows) {
    if (isBlankHeaderRow(row)) continue;
    const name = row.name.trim();
    if (name === "") continue;
    out[name] = row.value;
    n++;
  }
  return n > 0 ? out : undefined;
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
  const headers = headersFromRows(draft.headers);
  if (headers) req.headers = headers;
  return req;
}
