// Pure helpers for the ⌘K command palette (build contract §4.1): the
// paste-a-task-ID detector and the per-state bulk-verb capability matrix the
// palette's "verb on current scope" entries derive from.

import type { JobVerb } from "../api";

// asynq task IDs are UUIDs (any version).
const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * parseTaskIdInput detects a pasted task ID. Whitespace is trimmed and the
 * ID is normalized to lowercase (asynq stores them lowercased). A
 * `<queue>/<uuid>` paste (the drawer's peek format) is accepted too — the
 * queue part is returned alongside so the lookup can skip the state sweep.
 * Returns null for anything that is not a task ID.
 */
export function parseTaskIdInput(
  raw: string
): { id: string; queue?: string } | null {
  const s = raw.trim();
  if (s === "") return null;
  if (UUID_RE.test(s)) return { id: s.toLowerCase() };
  // peek format: queue names may contain slashes; the ID is after the LAST
  // slash (same contract as urlstate.parsePeek).
  const i = s.lastIndexOf("/");
  if (i > 0 && i < s.length - 1) {
    const queue = s.slice(0, i);
    const id = s.slice(i + 1);
    if (UUID_RE.test(id) && !/\s/.test(queue)) return { id: id.toLowerCase(), queue };
  }
  return null;
}

// Bulk-on-filter capabilities per state — mirrors the backend job runner's
// per-state verb support (§3.4 bulk bar / TasksGlobalView's matrix).
export const STATE_BULK_VERBS: Record<string, JobVerb[]> = {
  active: ["cancel"],
  pending: ["archive", "delete"],
  aggregating: ["run", "archive", "delete"],
  scheduled: ["run", "archive", "delete"],
  retry: ["run", "archive", "delete"],
  archived: ["run", "delete"],
  completed: ["delete"],
};

/** Lookup order for the cross-state task-ID search: states where an operator
 * most likely holds an ID first, cheapest-to-hit early. */
export const TASK_LOOKUP_STATES = [
  "active",
  "pending",
  "retry",
  "scheduled",
  "archived",
  "completed",
  "aggregating",
] as const;
