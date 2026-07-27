// URL-serialized console state for the Tasks query console.
//
// Fleet Console build contract §2: all view state lives in the URL — back/
// forward works, refresh restores the view, and copying the URL reproduces it
// exactly. Drawer state is the separate `peek` param so a task peek is
// deep-linkable from any filter combination.
//
// Phase 6 (§3.4): the `q` param carries AQL and is the SINGLE SOURCE OF
// TRUTH — queue, state, and metadata chips are all clauses of the query
// string. Legacy URLs (`queue=X&state=Y&meta=k:v&q=free text`) are migrated
// on parse: their params are prepended into the AQL string once, and only
// `q` (plus page/size/mode) is ever written back.
//
// These are pure functions over URLSearchParams so round-tripping is
// unit-testable without a router.

import { MetaPair } from "./metadata";
import { aqlFromLegacy, getClause, metaClausesOf } from "./aql";

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
  q: string; // AQL query — the single source of truth
  state: ConsoleTaskState; // DERIVED from q's state= clause (default pending)
  queue: string; // DERIVED from q's queue= clause, or "all"
  meta: MetaPair[]; // DERIVED from q's meta.* clauses
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
// `state`/`queue`/`meta` remain listed so migrated legacy params are removed
// on the first write.
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

// deriveConsoleQuery computes the derived state/queue/meta fields from the
// AQL string. Invalid state values degrade to the default — the backend's
// rejection banner explains the real problem.
function deriveFromQuery(q: string): Pick<ConsoleState, "state" | "queue" | "meta"> {
  const stateClause = getClause(q, "state");
  const queueClause = getClause(q, "queue");
  const state = stateClause?.value ?? "";
  return {
    state: (CONSOLE_STATES as readonly string[]).includes(state)
      ? (state as ConsoleTaskState)
      : DEFAULT_CONSOLE_STATE.state,
    queue: queueClause?.value || DEFAULT_CONSOLE_STATE.queue,
    meta: metaClausesOf(q),
  };
}

// parseConsoleState reads console state from URL search params. Missing or
// invalid values (a hand-edited URL) degrade to defaults instead of
// crashing. Legacy URLs are migrated: queue=X / state=Y / meta=k:v params
// are prepended into the AQL string once (§3.4 phase 6).
export function parseConsoleState(params: URLSearchParams): ConsoleState {
  const mode = params.get("mode") ?? "";
  const size = Number(params.get("size"));
  const page = Number(params.get("page"));

  let q = params.get("q") ?? "";
  const legacyState = params.get("state") ?? "";
  const legacyQueue = params.get("queue") ?? "";
  const legacyMeta = params.getAll("meta").flatMap(parseMetaParam);
  if (legacyState !== "" || legacyQueue !== "" || legacyMeta.length > 0) {
    q = aqlFromLegacy({
      queue: getClause(q, "queue") ? undefined : legacyQueue,
      state:
        !getClause(q, "state") && (CONSOLE_STATES as readonly string[]).includes(legacyState)
          ? legacyState
          : undefined,
      meta: legacyMeta.filter((p) => !metaClausesOf(q).some((x) => x.key === p.key && x.value === p.value)),
      freeText: q,
    });
  }

  return {
    q,
    ...deriveFromQuery(q),
    page: Number.isInteger(page) && page > 1 ? page - 1 : 0,
    size: (PAGE_SIZES as readonly number[]).includes(size)
      ? size
      : DEFAULT_CONSOLE_STATE.size,
    mode: (RESULT_MODES as readonly string[]).includes(mode)
      ? (mode as ResultMode)
      : DEFAULT_CONSOLE_STATE.mode,
  };
}

// serializeConsoleState writes console state into search params. Only `q`,
// `page`, `size`, `mode` are ever written — state/queue/meta live INSIDE the
// query string (single source of truth). Values equal to the defaults are
// omitted so shared URLs stay short; any params in `base` that the console
// doesn't own (e.g. `peek`) are carried through.
export function serializeConsoleState(
  state: ConsoleState,
  base?: URLSearchParams
): URLSearchParams {
  const params = new URLSearchParams(base);
  for (const key of CONSOLE_PARAMS) params.delete(key);
  if (state.q) params.set("q", state.q);
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
              Scan-to-completion job (`scanjob` param)
 **************************************************************/

// The id of the running scan-to-completion preview job lives in the URL so a
// reload (or a shared link) re-attaches the live meter to the job instead of
// losing the thread. NOT a console-owned param (CONSOLE_PARAMS): query edits
// serialize around it, exactly like `peek`.
export const SCANJOB_PARAM = "scanjob";

// parseScanJob reads the tracked scan-job id ("" and absent both mean none).
export function parseScanJob(params: URLSearchParams): string | null {
  const raw = params.get(SCANJOB_PARAM);
  return raw ? raw : null;
}

// withScanJob returns a copy of the params with the scan-job id set (or
// cleared with null), preserving every other param — the base-preserving
// pattern all url-state writers follow.
export function withScanJob(
  params: URLSearchParams,
  id: string | null
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (id) next.set(SCANJOB_PARAM, id);
  else next.delete(SCANJOB_PARAM);
  return next;
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

// taskDrawerSearch builds the /tasks search string that opens the console
// filtered to the task's queue with the task peeked in the drawer. This is
// the canonical task deep link — the old /queues/:qname/tasks/:taskId route
// redirects here (the drawer supersedes the details page and resolves the
// peek by queue/id independently of the visible result list).
export function taskDrawerSearch(queue: string, taskId: string): string {
  const params = serializeConsoleState({
    ...DEFAULT_CONSOLE_STATE,
    q: aqlFromLegacy({ queue }),
  });
  params.set("peek", serializePeek({ queue, id: taskId }));
  return `?${params.toString()}`;
}
