import { useEffect, useMemo, useRef } from "react";
import { useLocation } from "react-router-dom";
import { useDispatch, useSelector } from "react-redux";
import { AppState } from "../store";
import { pollTick } from "../actions/settingsActions";

export function usePolling(doFn: () => void, interval: number) {
  const dispatch = useDispatch();
  const pollingActive = useSelector((s: AppState) => s.settings.pollingActive);

  // Keep latest doFn in a ref so inline callbacks don't re-trigger the effect
  // every render (which would cause an infinite update loop).
  const savedFn = useRef(doFn);
  savedFn.current = doFn;

  useEffect(() => {
    const tick = () => {
      savedFn.current();
      dispatch(pollTick());
    };
    tick();
    // When polling is paused we still fetch once, but skip the interval.
    if (!pollingActive) return;
    const id = setInterval(tick, interval * 1000);
    return () => clearInterval(id);
  }, [interval, pollingActive, dispatch]);
}

export function useQuery(): URLSearchParams {
  const { search } = useLocation();
  return useMemo(() => new URLSearchParams(search), [search]);
}
