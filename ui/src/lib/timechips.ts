// State-aware time quick-picks for the Tasks console query bar (§3.4 time
// predicates). Pure data + string helpers: the chip table maps each console
// state to the time clauses the backend AQL field/state matrix allows there
// (aql package — pending_age→pending, running/deadline_pct→active,
// next_run/past_due→scheduled+retry, failed_after/before→retry+archived,
// died→archived, expires→completed), and the compose helper edits the query
// TEXT — the same single-source-of-truth principle as the facet chips
// (lib/aql.ts). The backend parser remains the only validity authority.

import { parseAqlClauses } from "./aql";
import { ConsoleTaskState } from "./urlstate";

export interface TimeChip {
  label: string; // chip caption, e.g. "waiting >5m"
  field: string; // AQL field the clause writes
  op: "" | "=" | "<" | ">"; // "" = flag clause (past_due)
  // Literal clause value (a duration like "5m"); "" for flags and for chips
  // whose value is computed at click time (agoMs).
  value: string;
  // When set, the clause value is computed AT CLICK TIME as the absolute
  // RFC3339 timestamp of (now − agoMs) — the failed_after windows.
  agoMs?: number;
}

const HOUR_MS = 3600_000;

// state → chips. Every entry uses only fields the matrix allows in that
// state; aggregating has no time predicates, so its row renders nothing.
export const TIME_CHIPS: Record<ConsoleTaskState, TimeChip[]> = {
  pending: [
    { label: "waiting >5m", field: "pending_age", op: ">", value: "5m" },
    { label: ">30m", field: "pending_age", op: ">", value: "30m" },
    { label: ">2h", field: "pending_age", op: ">", value: "2h" },
  ],
  active: [
    { label: "running >30s", field: "running", op: ">", value: "30s" },
    { label: ">5m", field: "running", op: ">", value: "5m" },
  ],
  aggregating: [],
  scheduled: [
    { label: "fires <10m", field: "next_run", op: "<", value: "10m" },
    { label: "<1h", field: "next_run", op: "<", value: "1h" },
    { label: "past due", field: "past_due", op: "", value: "" },
  ],
  retry: [
    { label: "fires <10m", field: "next_run", op: "<", value: "10m" },
    { label: "failed last 1h", field: "failed_after", op: "=", value: "", agoMs: HOUR_MS },
    { label: "last 24h", field: "failed_after", op: "=", value: "", agoMs: 24 * HOUR_MS },
  ],
  archived: [
    { label: "died >1d", field: "died", op: ">", value: "1d" },
    { label: ">7d", field: "died", op: ">", value: "7d" },
    { label: "failed last 24h", field: "failed_after", op: "=", value: "", agoMs: 24 * HOUR_MS },
  ],
  completed: [
    { label: "expires <1h", field: "expires", op: "<", value: "1h" },
    { label: "<24h", field: "expires", op: "<", value: "24h" },
  ],
};

// rfc3339 renders an absolute timestamp at whole-second precision (the
// grammar's RFC3339 form; fractional seconds add nothing at these scales).
function rfc3339(d: Date): string {
  return d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** The clause token a chip writes, computed against `now` (agoMs chips stamp
 * the absolute RFC3339 of now − agoMs at click time). */
export function timeChipClause(chip: TimeChip, now: Date): string {
  if (chip.op === "") return chip.field; // flag clause (past_due)
  const value =
    chip.agoMs !== undefined ? rfc3339(new Date(now.getTime() - chip.agoMs)) : chip.value;
  return `${chip.field}${chip.op}${value}`;
}

/** Compose a chip's clause into the query text: replace an existing clause of
 * the same FIELD in place when present (extra duplicates dropped), else
 * append — the facet-chip composition rule applied to time predicates. */
export function applyTimeChip(q: string, chip: TimeChip, now: Date): string {
  const token = timeChipClause(chip, now);
  let replaced = false;
  const tokens = parseAqlClauses(q)
    .map((c) => {
      if (c.field !== chip.field) return c.raw;
      if (replaced) return "";
      replaced = true;
      return token;
    })
    .filter((t) => t !== "");
  if (!replaced) tokens.push(token);
  return tokens.join(" ");
}

/** Whether the query already carries exactly this chip's clause. Chips with a
 * click-time value (agoMs) are never "active": their token is moment-
 * dependent, and re-clicking deliberately re-stamps the window from now. */
export function isTimeChipActive(q: string, chip: TimeChip): boolean {
  if (chip.agoMs !== undefined) return false;
  const token = timeChipClause(chip, new Date(0));
  return parseAqlClauses(q).some((c) => c.raw === token);
}
