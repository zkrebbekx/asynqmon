// Derivation of a task's path through the asynq state machine
// (scheduled → pending → aggregating → active → retry ⟲ → completed/archived)
// from the fields TaskInfo actually stores.
//
// Honesty rule (build contract §3.5/§7): a state is marked traversed only
// when a stored field proves it — retried>0 proves the retry loop ran,
// group!="" proves aggregation, completed_at proves completion, a recorded
// failure proves the task reached active. Nothing is fabricated for
// transitions asynq doesn't record (e.g. whether a running task was ever
// scheduled: pending_since is deleted on dequeue, so we don't guess).
//
// Pure functions so the path logic is unit-testable without an SVG.

export const JOURNEY_STATES = [
  "scheduled",
  "pending",
  "aggregating",
  "active",
  "retry",
  "completed",
  "archived",
] as const;
export type JourneyState = (typeof JOURNEY_STATES)[number];

// The slice of TaskInfo the derivation reads — kept narrow so tests don't
// have to construct full TaskInfo objects.
export interface JourneyInput {
  state: string;
  retried: number;
  // Optional: the API omits the field entirely when the task never grouped.
  group?: string;
  completed_at?: string;
  last_failed_at?: string;
}

export interface Journey {
  current: JourneyState | null; // null for an unrecognized state string
  traversed: ReadonlySet<JourneyState>;
  retryAttempts: number; // msg.Retried — the loop-edge annotation
  viaGroup: boolean; // task entered via an aggregation group
}

const zeroTimestamp = "0001-01-01T00:00:00Z";

// The API renders unset times as "", "-", or the Go zero time.
function hasTimestamp(s: string | undefined): boolean {
  return !!s && s !== "-" && s !== zeroTimestamp;
}

export function deriveJourney(t: JourneyInput): Journey {
  const current = (JOURNEY_STATES as readonly string[]).includes(t.state)
    ? (t.state as JourneyState)
    : null;
  const traversed = new Set<JourneyState>();
  if (current) traversed.add(current);

  const viaGroup = !!t.group || current === "aggregating";
  if (viaGroup) traversed.add("aggregating");

  // Reaching active (and therefore pending) is provable from the current
  // state or from evidence of a past run: a retry count or a recorded
  // failure only exist after a worker picked the task up.
  const hasRun =
    current === "active" ||
    current === "retry" ||
    current === "completed" ||
    t.retried > 0 ||
    hasTimestamp(t.last_failed_at);
  if (hasRun) {
    traversed.add("pending");
    traversed.add("active");
  }

  if (t.retried > 0 || current === "retry") traversed.add("retry");
  if (hasTimestamp(t.completed_at)) traversed.add("completed");

  return { current, traversed, retryAttempts: t.retried, viaGroup };
}

export type JourneyEdge =
  | "scheduled→pending"
  | "pending→aggregating"
  | "aggregating→active"
  | "pending→active"
  | "active→completed"
  | "active→retry"
  | "retry→pending"
  | "retry→archived";

// journeyEdges marks an edge traversed only when both endpoints are proven
// and the edge lies on the path the task actually took: grouped tasks route
// pending→aggregating→active, ungrouped ones take the direct arc. An
// archived task with zero recorded runs (manually archived) gets no edges —
// we can't know how it got there, so we don't draw a path.
export function journeyEdges(j: Journey): ReadonlySet<JourneyEdge> {
  const has = (s: JourneyState) => j.traversed.has(s);
  const edges = new Set<JourneyEdge>();
  if (has("scheduled") && has("pending")) edges.add("scheduled→pending");
  if (j.viaGroup) {
    if (has("aggregating") && has("pending")) edges.add("pending→aggregating");
    if (has("aggregating") && has("active")) edges.add("aggregating→active");
  } else if (has("pending") && has("active")) {
    edges.add("pending→active");
  }
  if (has("completed")) edges.add("active→completed");
  if (has("retry")) {
    edges.add("active→retry");
    // The loop closes (retry→pending) whenever the task provably went back:
    // any retry-state task has a re-run scheduled, and a past retried>0
    // means the loop already cycled.
    if (has("pending")) edges.add("retry→pending");
  }
  if (has("archived") && j.retryAttempts > 0) edges.add("retry→archived");
  return edges;
}

// Status tone per state, shared by the journey graph and drawer chips.
// Tone is always paired with a form cue (chip text, stroke weight, pulse
// dot) per the design contract — never color alone.
export const STATE_TONE: Record<
  JourneyState,
  "good" | "warn" | "crit" | "info" | "mut"
> = {
  scheduled: "mut",
  pending: "mut",
  aggregating: "mut",
  active: "info",
  retry: "warn",
  completed: "good",
  archived: "crit",
};
