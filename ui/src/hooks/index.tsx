import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "react-router-dom";
import { useDispatch, useSelector } from "react-redux";
import { AppState } from "../store";
import { pollTick } from "../actions/settingsActions";
import { ThemePreference } from "../reducers/settingsReducer";
import { acquireOverlayMute } from "../lib/keymap";

// Poll cadence bounds, in seconds. The settings value is user-typed; a
// cadence below 2 s hammers the server and one above 20 s leaves the
// dashboard stale for longer than the SSE watchdogs tolerate.
export const MIN_POLL_INTERVAL_SECONDS = 2;
export const MAX_POLL_INTERVAL_SECONDS = 20;

export function clampPollInterval(seconds: number): number {
  if (!Number.isFinite(seconds)) return MAX_POLL_INTERVAL_SECONDS;
  return Math.min(MAX_POLL_INTERVAL_SECONDS, Math.max(MIN_POLL_INTERVAL_SECONDS, seconds));
}

function isThenable(v: unknown): v is PromiseLike<unknown> {
  return typeof v === "object" && v !== null && typeof (v as PromiseLike<unknown>).then === "function";
}

export function usePolling(
  doFn: () => void | Promise<unknown>,
  interval: number,
  fetchKey: ReadonlyArray<string | number | boolean | null | undefined> = []
) {
  const dispatch = useDispatch();
  const pollingActive = useSelector((s: AppState) => s.settings.pollingActive);

  // Keep latest doFn in a ref so inline callbacks don't re-trigger the effect
  // every render (which would cause an infinite update loop).
  const savedFn = useRef(doFn);
  savedFn.current = doFn;

  // Number of callback invocations whose promise is still pending. An
  // interval tick is skipped while one is pending: when Redis or the pod
  // hangs, each tick used to add one more XHR until the browser's per-host
  // connection pool was full and the tab looked dead until reload.
  const inFlight = useRef(0);

  // Params the callback captures (queue, page, task id, ...) go in fetchKey:
  // when they change we fetch immediately and restart the interval, instead
  // of showing stale data until the next poll tick.
  const key = JSON.stringify(fetchKey);

  useEffect(() => {
    // `force` ticks (mount, key change) always run: the pending call, if
    // any, belongs to the previous params. Interval and visibility ticks
    // are skipped while a call is pending.
    const tick = (force: boolean) => {
      if (!force && inFlight.current > 0) return;
      inFlight.current++;
      const clear = () => {
        inFlight.current--;
      };
      let result: unknown;
      try {
        result = savedFn.current();
      } catch (e) {
        clear();
        throw e;
      }
      if (isThenable(result)) {
        result.then(clear, clear);
      } else {
        clear();
      }
      dispatch(pollTick());
    };
    tick(true);
    // When polling is paused we still fetch once, but skip the interval.
    if (!pollingActive) return;

    // Pause the interval while the tab is hidden — polling a dashboard nobody
    // is looking at just loads the server — and refetch immediately on return.
    let id: ReturnType<typeof setInterval> | null = null;
    const start = () => {
      if (id === null) id = setInterval(() => tick(false), clampPollInterval(interval) * 1000);
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
        tick(false);
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

// useLatestOnly guards async fetchers against out-of-order resolution: a
// slow response for an old filter must never overwrite the results of a
// newer one (it used to leave the table showing the previous query's rows,
// total, and cursor under the new state pill until the next poll tick).
//
//   const beginFetch = useLatestOnly();
//   const fetch = useCallback(async () => {
//     const isCurrent = beginFetch();   // this call is now the newest
//     const resp = await api.get(...);
//     if (!isCurrent()) return;         // a newer call started meanwhile
//     setRows(resp.rows);
//   }, [...]);
//
// The returned function identity is stable, so it never churns callback deps.
export function useLatestOnly(): () => () => boolean {
  const seq = useRef(0);
  return useRef(() => {
    const mine = ++seq.current;
    return () => mine === seq.current;
  }).current;
}

// useOverlayMute stamps the keymap's overlay marker while `active` so
// page-level shortcuts (j/k/x/#/…) never fire behind a modal surface.
// Refcounted in lib/keymap so stacked overlays compose.
export function useOverlayMute(active: boolean = true) {
  useEffect(() => {
    if (!active) return;
    return acquireOverlayMute();
  }, [active]);
}
