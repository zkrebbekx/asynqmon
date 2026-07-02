import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "react-router-dom";
import { useDispatch, useSelector } from "react-redux";
import { AppState } from "../store";
import { pollTick } from "../actions/settingsActions";
import { ThemePreference } from "../reducers/settingsReducer";

export function usePolling(
  doFn: () => void,
  interval: number,
  fetchKey: ReadonlyArray<string | number | boolean | null | undefined> = []
) {
  const dispatch = useDispatch();
  const pollingActive = useSelector((s: AppState) => s.settings.pollingActive);

  // Keep latest doFn in a ref so inline callbacks don't re-trigger the effect
  // every render (which would cause an infinite update loop).
  const savedFn = useRef(doFn);
  savedFn.current = doFn;

  // Params the callback captures (queue, page, task id, ...) go in fetchKey:
  // when they change we fetch immediately and restart the interval, instead
  // of showing stale data until the next poll tick.
  const key = JSON.stringify(fetchKey);

  useEffect(() => {
    const tick = () => {
      savedFn.current();
      dispatch(pollTick());
    };
    tick();
    // When polling is paused we still fetch once, but skip the interval.
    if (!pollingActive) return;

    // Pause the interval while the tab is hidden — polling a dashboard nobody
    // is looking at just loads the server — and refetch immediately on return.
    let id: ReturnType<typeof setInterval> | null = null;
    const start = () => {
      if (id === null) id = setInterval(tick, interval * 1000);
    };
    const stop = () => {
      if (id !== null) {
        clearInterval(id);
        id = null;
      }
    };
    const onVisibilityChange = () => {
      if (document.hidden) {
        stop();
      } else {
        tick();
        start();
      }
    };
    if (!document.hidden) start();
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [interval, pollingActive, dispatch, key]);
}

export function useQuery(): URLSearchParams {
  const { search } = useLocation();
  return useMemo(() => new URLSearchParams(search), [search]);
}

// usePrefersDark tracks the OS color-scheme preference reactively, so a theme
// flip while the app is open takes effect without a reload.
export function usePrefersDark(): boolean {
  const [prefersDark, setPrefersDark] = useState(
    () => window.matchMedia("(prefers-color-scheme: dark)").matches
  );
  useEffect(() => {
    const mql = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (e: MediaQueryListEvent) => setPrefersDark(e.matches);
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, []);
  return prefersDark;
}

// useIsDark is the single source of truth for dark mode: the user's theme
// preference from settings, falling back to the (live) OS preference.
export function useIsDark(): boolean {
  const themePreference = useSelector(
    (s: AppState) => s.settings.themePreference
  );
  const prefersDark = usePrefersDark();
  if (themePreference === ThemePreference.Always) return true;
  if (themePreference === ThemePreference.Never) return false;
  return prefersDark;
}
