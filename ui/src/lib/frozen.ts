// Freeze-on-interaction core (build contract §4.4): while the operator
// hovers a table, holds a selection, or has a drawer open, row RENDERING is
// suspended — data keeps being fetched into the `live` buffer, a "N updates
// paused — refresh" pill counts the suspended changes, and applying (pill
// click, unfreeze, or a mutation) promotes the buffer. Pure reducer so the
// semantics are unit-testable; useFrozenData is the thin React wrapper.

export interface FrozenState<T> {
  /** What the table renders. */
  data: T;
  /** The latest fetched value (always current). */
  live: T;
  /** Count of suspended CHANGES (identical refetches don't count). */
  pending: number;
  frozen: boolean;
}

export type FrozenAction<T> =
  | { type: "live"; data: T } // a poll result arrived
  | { type: "setFrozen"; frozen: boolean }
  | { type: "apply" }; // pill click / mutation invalidation

export function frozenInit<T>(data: T): FrozenState<T> {
  return { data, live: data, pending: 0, frozen: false };
}

/** Default equality: structural, so a poll returning identical rows in a
 * fresh array is not an "update". */
export function jsonEquals<T>(a: T, b: T): boolean {
  if (Object.is(a, b)) return true;
  try {
    return JSON.stringify(a) === JSON.stringify(b);
  } catch {
    return false;
  }
}

export function frozenReduce<T>(
  s: FrozenState<T>,
  a: FrozenAction<T>,
  equals: (x: T, y: T) => boolean = jsonEquals
): FrozenState<T> {
  switch (a.type) {
    case "live": {
      if (!s.frozen) {
        // Render frozen? No — pass straight through.
        return { data: a.data, live: a.data, pending: 0, frozen: false };
      }
      // Frozen: buffer it; count it only when it differs from the PREVIOUS
      // buffered value — comparing against the rendered data would re-count
      // one real change on every subsequent identical poll tick.
      const changed = !equals(a.data, s.live);
      return { ...s, live: a.data, pending: changed ? s.pending + 1 : s.pending };
    }
    case "setFrozen": {
      if (a.frozen === s.frozen) return s;
      if (a.frozen) return { ...s, frozen: true };
      // Unfreezing applies the buffer immediately.
      return { data: s.live, live: s.live, pending: 0, frozen: false };
    }
    case "apply":
      // Applies the buffer but KEEPS the freeze (the drawer may still be
      // open) — the pill resets and recounts.
      return { ...s, data: s.live, pending: 0 };
  }
}
