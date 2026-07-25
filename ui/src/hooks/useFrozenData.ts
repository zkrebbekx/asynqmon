// useFrozenData wraps a polled data feed with §4.4 freeze-on-interaction:
// while `frozen` is true (hover / selection / drawer open) the returned
// `data` stops tracking `live`, `pending` counts the suspended changes for
// the "N updates paused — refresh" pill, `apply` promotes the buffer (pill
// click), and `applyNextChange` marks the NEXT live arrival to apply
// immediately (mutations invalidate immediately — their refetch must land
// even mid-freeze). Core semantics live in lib/frozen (pure, tested).

import { useCallback, useEffect, useRef, useState } from "react";
import { FrozenState, frozenInit, frozenReduce, jsonEquals } from "../lib/frozen";

export interface FrozenData<T> {
  data: T;
  pending: number;
  apply: () => void;
  applyNextChange: () => void;
}

export function useFrozenData<T>(
  live: T,
  frozen: boolean,
  equals: (a: T, b: T) => boolean = jsonEquals,
  // When resetKey changes (filter/sort/page change — the operator ASKED for
  // different data), the buffer reinitializes and the next arrival applies
  // straight through the freeze: a deliberate view change is never "paused".
  resetKey?: string
): FrozenData<T> {
  const [state, setState] = useState<FrozenState<T>>(() => frozenInit(live));
  const equalsRef = useRef(equals);
  equalsRef.current = equals;
  // Set by applyNextChange: the next live arrival bypasses the freeze once.
  const flushNextRef = useRef(false);
  const liveRef = useRef(live);
  liveRef.current = live;
  const frozenRef = useRef(frozen);
  frozenRef.current = frozen;

  // Declared BEFORE the live effect so a same-render live change lands on
  // the reinitialized state.
  const resetKeyRef = useRef(resetKey);
  useEffect(() => {
    if (resetKeyRef.current === resetKey) return;
    resetKeyRef.current = resetKey;
    flushNextRef.current = true;
    setState({ ...frozenInit(liveRef.current), frozen: frozenRef.current });
  }, [resetKey]);

  useEffect(() => {
    setState((s) => frozenReduce(s, { type: "setFrozen", frozen }, equalsRef.current));
  }, [frozen]);

  useEffect(() => {
    setState((s) => {
      // The flush flag is consumed only by an arrival that actually CHANGES
      // the buffer — an unchanged same-render pass (reset, identity churn)
      // must leave it armed for the real response.
      const changed = !equalsRef.current(live, s.live);
      let next = frozenReduce(s, { type: "live", data: live }, equalsRef.current);
      if (changed && flushNextRef.current) {
        flushNextRef.current = false;
        next = frozenReduce(next, { type: "apply" }, equalsRef.current);
      }
      return next;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live]);

  const apply = useCallback(() => {
    setState((s) => frozenReduce(s, { type: "apply" }, equalsRef.current));
  }, []);

  const applyNextChange = useCallback(() => {
    flushNextRef.current = true;
  }, []);

  return { data: state.data, pending: state.pending, apply, applyNextChange };
}
