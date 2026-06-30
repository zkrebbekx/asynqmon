import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { ReactNode } from "react";
import { act, renderHook } from "@testing-library/react";
import { combineReducers, configureStore } from "@reduxjs/toolkit";
import { Provider } from "react-redux";
import settingsReducer from "../reducers/settingsReducer";
import { togglePolling } from "../actions/settingsActions";
import { usePolling } from "./index";

function makeStore() {
  return configureStore({ reducer: combineReducers({ settings: settingsReducer }) });
}

function wrapperFor(store: ReturnType<typeof makeStore>) {
  return ({ children }: { children: ReactNode }) => <Provider store={store}>{children}</Provider>;
}

describe("usePolling", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("invokes the callback once immediately on mount", () => {
    const fn = vi.fn();
    const store = makeStore();
    renderHook(() => usePolling(fn, 5), { wrapper: wrapperFor(store) });
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("invokes the callback again after each interval while active", () => {
    const fn = vi.fn();
    const store = makeStore();
    renderHook(() => usePolling(fn, 5), { wrapper: wrapperFor(store) });
    expect(fn).toHaveBeenCalledTimes(1);
    act(() => void vi.advanceTimersByTime(5000));
    expect(fn).toHaveBeenCalledTimes(2);
    act(() => void vi.advanceTimersByTime(5000));
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it("does not re-fire on every render when given an inline callback (no infinite loop)", () => {
    // Regression: passing a fresh arrow each render previously re-ran the effect
    // every render, causing an infinite update loop.
    const store = makeStore();
    let calls = 0;
    const { rerender } = renderHook(() => usePolling(() => { calls++; }, 5), {
      wrapper: wrapperFor(store),
    });
    expect(calls).toBe(1);
    rerender();
    rerender();
    rerender();
    // Still 1 — re-rendering with a new callback identity must not re-run the effect.
    expect(calls).toBe(1);
  });

  it("fetches once but does not set up an interval when polling is paused", () => {
    const fn = vi.fn();
    const store = makeStore();
    store.dispatch(togglePolling()); // pollingActive: true -> false
    renderHook(() => usePolling(fn, 5), { wrapper: wrapperFor(store) });
    expect(fn).toHaveBeenCalledTimes(1);
    act(() => void vi.advanceTimersByTime(20000));
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("records a refresh timestamp on each tick", () => {
    const store = makeStore();
    expect(store.getState().settings.lastUpdatedAt).toBe(0);
    renderHook(() => usePolling(() => {}, 5), { wrapper: wrapperFor(store) });
    expect(store.getState().settings.lastUpdatedAt).toBeGreaterThan(0);
  });
});
