// URL-serialized console state for the Tasks query console.
//
// Fleet Console build contract §2: all view state lives in the URL — back/
// forward works, refresh restores the view, and copying the URL reproduces it
// exactly. Drawer state is the separate `peek` param so a task peek is
// deep-linkable from any filter combination.
//
// These are pure functions over URLSearchParams so round-tripping is
// unit-testable without a router.

import { MetaPair } from "./metadata";

export const CONSOLE_STATES = [
  "active",
  "pending",
  "aggregating",
  "scheduled",
  "retry",
  "archived",
  "completed",
] as const;
export type ConsoleTaskState = (typeof CONSOLE_STATES)[number];

export const PAGE_SIZES = [20, 50, 100] as const;

// Result rendering modes. Group modes are only meaningful for failure states
// (retry/archived) where the aggregate endpoints exist; the view falls back
// to rows elsewhere, but the URL keeps the mode so it survives tab switches.
export const RESULT_MODES = ["rows", "group:error", "group:type"] as const;
export type ResultMode = (typeof RESULT_MODES)[number];

export interface ConsoleState {
  q: string; // free-text query
  state: ConsoleTaskState; // state tab
  queue: string; // queue name, or "all"
  meta: MetaPair[]; // metadata chips (AND semantics)
  page: number; // 0-based page index (1-based in the URL for humans)
  size: number; // page size
  mode: ResultMode; // result rendering mode
}

export const DEFAULT_CONSOLE_STATE: ConsoleState = {
  q: "",
  state: "pending",
  queue: "all",
  meta: [],
  page: 0,
  size: 20,
  mode: "rows",
};

// Params owned by the console; serializeConsoleState clears exactly these
// before writing so params it doesn't own (e.g. `peek`) survive untouched.
const CONSOLE_PARAMS = ["q", "state", "queue", "meta", "page", "size", "mode"];

// Meta chips use the same `key:value` wire format as the search API
// (split on the first colon, so values may themselves contain colons).
function parseMetaParam(raw: string): MetaPair[] {
  const i = raw.indexOf(":");
  if (i <= 0) return []; // malformed (no key) — drop rather than crash
  return [{ key: raw.slice(0, i), value: raw.slice(i + 1) }];
}

export function serializeMetaPair(p: MetaPair): string {
  return `${p.key}:${p.value}`;
}

// parseConsoleState reads console state from URL search params. Missing or
// invalid values (a hand-edited URL) degrade to defaults instead of crashing.
export function parseConsoleState(params: URLSearchParams): ConsoleState {
  const state = params.get("state") ?? "";
  const mode = params.get("mode") ?? "";
  const size = Number(params.get("size"));
  const page = Number(params.get("page"));
  return {
    q: params.get("q") ?? "",
    state: (CONSOLE_STATES as readonly string[]).includes(state)
      ? (state as ConsoleTaskState)
      : DEFAULT_CONSOLE_STATE.state,
    queue: params.get("queue") || DEFAULT_CONSOLE_STATE.queue,
    meta: params.getAll("meta").flatMap(parseMetaParam),
    page: Number.isInteger(page) && page > 1 ? page - 1 : 0,
    size: (PAGE_SIZES as readonly number[]).includes(size)
      ? size
      : DEFAULT_CONSOLE_STATE.size,
    mode: (RESULT_MODES as readonly string[]).includes(mode)
      ? (mode as ResultMode)
      : DEFAULT_CONSOLE_STATE.mode,
  };
}

// serializeConsoleState writes console state into search params. Values equal
// to the defaults are omitted so shared URLs stay short; any params in `base`
// that the console doesn't own (e.g. `peek`) are carried through.
export function serializeConsoleState(
  state: ConsoleState,
  base?: URLSearchParams
): URLSearchParams {
  const params = new URLSearchParams(base);
  for (const key of CONSOLE_PARAMS) params.delete(key);
  if (state.q) params.set("q", state.q);
  if (state.state !== DEFAULT_CONSOLE_STATE.state) params.set("state", state.state);
  if (state.queue !== DEFAULT_CONSOLE_STATE.queue) params.set("queue", state.queue);
  for (const p of state.meta) params.append("meta", serializeMetaPair(p));
  if (state.page > 0) params.set("page", String(state.page + 1));
  if (state.size !== DEFAULT_CONSOLE_STATE.size) params.set("size", String(state.size));
  if (state.mode !== DEFAULT_CONSOLE_STATE.mode) params.set("mode", state.mode);
  return params;
}

/**************************************************************
              Queues Directory (`/queues`) state
 **************************************************************/

// Server-side sortable columns of GET /api/fleet/queues (frozen contract).
export const DIRECTORY_SORT_KEYS = [
  "name",
  "pending",
  "active",
  "scheduled",
  "retry",
  "archived",
  "completed",
  "oldest_pending_age",
  "latency",
  "processed_today",
  "failed_today",
  "error_rate",
  "consumers",
] as const;
export type DirectorySortKey = (typeof DIRECTORY_SORT_KEYS)[number];

export type SortDir = "asc" | "desc";

export const DIRECTORY_LIMITS = [50, 100, 200] as const;

export interface DirectoryState {
  sort: DirectorySortKey;
  dir: SortDir;
  f: string; // queue filter expression, passed to the API verbatim
  cursor: string; // opaque server cursor ("" = first page)
  limit: number;
}

// Default sort mirrors the approved mockup: worst queue first by oldest
// pending age.
export const DEFAULT_DIRECTORY_STATE: DirectoryState = {
  sort: "oldest_pending_age",
  dir: "desc",
  f: "",
  cursor: "",
  limit: 100,
};

// Params owned by the directory; serializeDirectoryState clears exactly these
// so params it doesn't own survive untouched (same contract as the console).
const DIRECTORY_PARAMS = ["sort", "dir", "f", "cursor", "limit"];

export function parseDirectoryState(params: URLSearchParams): DirectoryState {
  const sort = params.get("sort") ?? "";
  const dir = params.get("dir") ?? "";
  const limit = Number(params.get("limit"));
  return {
    sort: (DIRECTORY_SORT_KEYS as readonly string[]).includes(sort)
      ? (sort as DirectorySortKey)
      : DEFAULT_DIRECTORY_STATE.sort,
    dir: dir === "asc" || dir === "desc" ? dir : DEFAULT_DIRECTORY_STATE.dir,
    f: params.get("f") ?? "",
    cursor: params.get("cursor") ?? "",
    limit: (DIRECTORY_LIMITS as readonly number[]).includes(limit)
      ? limit
      : DEFAULT_DIRECTORY_STATE.limit,
  };
}

export function serializeDirectoryState(
  state: DirectoryState,
  base?: URLSearchParams
): URLSearchParams {
  const params = new URLSearchParams(base);
  for (const key of DIRECTORY_PARAMS) params.delete(key);
  if (state.sort !== DEFAULT_DIRECTORY_STATE.sort) params.set("sort", state.sort);
  if (state.dir !== DEFAULT_DIRECTORY_STATE.dir) params.set("dir", state.dir);
  if (state.f) params.set("f", state.f);
  if (state.cursor) params.set("cursor", state.cursor);
  if (state.limit !== DEFAULT_DIRECTORY_STATE.limit)
    params.set("limit", String(state.limit));
  return params;
}

/**************************************************************
                    Task drawer (`peek` param)
 **************************************************************/

export interface PeekTarget {
  queue: string;
  id: string;
}

// The peek param is `<queue>/<taskid>`. Task IDs are UUIDs and never contain
// "/", so splitting on the last slash is safe even for queue names that
// themselves contain slashes.
export function parsePeek(raw: string | null): PeekTarget | null {
  if (!raw) return null;
  const i = raw.lastIndexOf("/");
  if (i <= 0 || i === raw.length - 1) return null; // missing queue or id
  return { queue: raw.slice(0, i), id: raw.slice(i + 1) };
}

export function serializePeek(target: PeekTarget): string {
  return `${target.queue}/${target.id}`;
}
