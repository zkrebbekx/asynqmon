import { AxiosError } from "axios";
import dayjs from "dayjs";

// The API returns errors as JSON bodies: {"error": "..."}. Older endpoints
// (and proxies) may still return plain text, so fall back gracefully.
type ErrorBody = string | { error?: string } | undefined;

function errorMessageFromBody(data: ErrorBody): string {
  if (typeof data === "string" && data !== "") {
    return data;
  }
  if (data && typeof data === "object" && typeof data.error === "string") {
    return data.error;
  }
  return "";
}

// toErrorStringWithHttpStatus returns a string representaion of axios error with HTTP status.
export function toErrorStringWithHttpStatus(error: AxiosError<ErrorBody>): string {
  const { response } = error;
  if (!response) {
    return "error: no error response data available";
  }
  const msg = errorMessageFromBody(response.data) || response.statusText;
  return `${response.status} (${response.statusText}): ${msg}`;
}

// toErrorString returns a string representaion of axios error.
export function toErrorString(error: AxiosError<ErrorBody>): string {
  const { response } = error;
  if (!response) {
    return "Unknown error occurred. See the logs for details.";
  }
  return (
    errorMessageFromBody(response.data) ||
    `${response.status} ${response.statusText}`
  );
}

interface Duration {
  hour: number;
  minute: number;
  second: number;
  totalSeconds: number;
}

// Returns a duration from the number of seconds provided.
export function durationFromSeconds(totalSeconds: number): Duration {
  const hour = Math.floor(totalSeconds / 3600);
  const minute = Math.floor((totalSeconds - 3600 * hour) / 60);
  const second = totalSeconds - 3600 * hour - 60 * minute;
  return { hour, minute, second, totalSeconds };
}

// start and end are in milliseconds.
function durationBetween(start: number, end: number): Duration {
  const durationInMillisec = start - end;
  const totalSeconds = Math.floor(durationInMillisec / 1000);
  return durationFromSeconds(totalSeconds);
}

export function stringifyDuration(d: Duration): string {
  if (!Number.isFinite(d.totalSeconds)) {
    return "-";
  }
  if (d.hour > 24) {
    const n = Math.floor(d.hour / 24);
    return n + (n === 1 ? " day" : " days");
  }
  return (
    (d.hour !== 0 ? `${d.hour}h` : "") +
    (d.minute !== 0 ? `${d.minute}m` : "") +
    `${d.second}s`
  );
}

const zeroTimestamp = "0001-01-01T00:00:00Z";

// parseTimestamp returns the epoch milliseconds for an RFC3339 timestamp, or
// null when the value is missing, the Go zero time, or a placeholder the API
// uses for unknown values (e.g. "-"). Date.parse returns NaN rather than
// throwing on unparsable input, so callers must check for null explicitly.
function parseTimestamp(timestamp: string): number | null {
  if (!timestamp || timestamp === "-" || timestamp === zeroTimestamp) {
    return null;
  }
  const ms = Date.parse(timestamp);
  return Number.isNaN(ms) ? null : ms;
}

export function durationBefore(timestamp: string): string {
  const ms = parseTimestamp(timestamp);
  if (ms === null) {
    return "-";
  }
  const duration = durationBetween(ms, Date.now());
  if (duration.totalSeconds < 1) {
    return "now";
  }
  return "in " + stringifyDuration(duration);
}

export function timeAgo(timestamp: string): string {
  const ms = parseTimestamp(timestamp);
  if (ms === null) {
    return "-";
  }
  return timeAgoUnix(ms / 1000);
}

// durationSince renders how much time has elapsed since the given timestamp as
// a bare duration (e.g. "2h13m4s"), without the "ago" suffix timeAgo adds.
// Used for "how long has this been running/queued" columns.
export function durationSince(timestamp: string): string {
  const ms = parseTimestamp(timestamp);
  if (ms === null) {
    return "-";
  }
  const duration = durationBetween(Date.now(), ms);
  if (duration.totalSeconds < 1) {
    return "0s";
  }
  return stringifyDuration(duration);
}

export function timeAgoUnix(unixtime: number): string {
  if (!Number.isFinite(unixtime) || unixtime === 0) {
    return "";
  }
  const duration = durationBetween(Date.now(), unixtime * 1000);
  return stringifyDuration(duration) + " ago";
}

// formatTimestamp renders a server RFC3339 timestamp in the viewer's locale
// and timezone, for display next to relative times like timeAgo.
export function formatTimestamp(timestamp: string): string {
  if (!timestamp || timestamp === zeroTimestamp) {
    return "-";
  }
  const d = dayjs(timestamp);
  return d.isValid() ? d.format("MMM D, YYYY HH:mm:ss") : timestamp;
}

export function uuidPrefix(uuid: string): string {
  const idx = uuid.indexOf("-");
  if (idx === -1) {
    return uuid;
  }
  return uuid.substr(0, idx);
}

export function percentage(numerator: number, denominator: number): string {
  if (denominator === 0) return "0.00%";
  const perc = ((numerator / denominator) * 100).toFixed(2);
  return `${perc} %`;
}

export function isJsonPayload(p: string) {
  try {
    JSON.parse(p);
  } catch (error) {
    return false;
  }
  return true;
}

export function prettifyPayload(p: string) {
  if (isJsonPayload(p)) {
    return JSON.stringify(JSON.parse(p), null, 2);
  }
  return p;
}

// Returns the number of seconds elapsed since January 1, 1970 00:00:00 UTC.
export function currentUnixtime(): number {
  return Math.floor(Date.now() / 1000);
}
