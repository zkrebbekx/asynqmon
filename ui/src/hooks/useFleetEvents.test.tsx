import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { ReactNode } from "react";
import { act, renderHook, waitFor } from "@testing-library/react";
import { combineReducers, configureStore } from "@reduxjs/toolkit";
import { Provider } from "react-redux";
import settingsReducer from "../reducers/settingsReducer";
import { togglePolling } from "../actions/settingsActions";

// Mock the fetchers so no axios calls happen; the SSE transport is a stubbed
// global EventSource below.
vi.mock("../api-fleet", () => ({
  getFleetOverview: vi.fn(),
  getFleetAttention: vi.fn(),
  fleetEventsUrl: () => "/api/fleet/events",
}));

import { getFleetOverview, getFleetAttention } from "../api-fleet";
import { useFleetEvents } from "./useFleetEvents";

const mockOverview = vi.mocked(getFleetOverview);
const mockAttention = vi.mocked(getFleetAttention);

// Minimal fixtures — the hook treats payloads as opaque snapshots.
const OVERVIEW = {
  fleet: { pending: 42, queues_total: 3 },
  coverage: { tier: 1, queues_total: 3, refreshed_pct_5m: 100, updated_at: "" },
} as Awaited<ReturnType<typeof getFleetOverview>>;
const ATTENTION = {
  findings: [],
  updated_at: "",
  detectors_live: 9,
  detectors_learning: 2,
} as Awaited<ReturnType<typeof getFleetAttention>>;

class MockEventSource {
  static instances: MockEventSource[] = [];
  url: string;
  listeners: Record<string, (e: MessageEvent) => void> = {};
  onerror: (() => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }
  addEventListener(name: string, fn: (e: MessageEvent) => void) {
    this.listeners[name] = fn;
  }
  close() {
    this.closed = true;
  }
  emit(name: string, data: unknown) {
    this.listeners[name]?.({ data: JSON.stringify(data) } as MessageEvent);
  }
  fail() {
    this.onerror?.();
  }
}

function makeStore() {
  return configureStore({ reducer: combineReducers({ settings: settingsReducer }) });
}

function wrapperFor(store: ReturnType<typeof makeStore>) {
  return ({ children }: { children: ReactNode }) => (
    <Provider store={store}>{children}</Provider>
  );
}

describe("useFleetEvents", () => {
  beforeEach(() => {
    MockEventSource.instances = [];
    vi.stubGlobal("EventSource", MockEventSource);
    mockOverview.mockResolvedValue(OVERVIEW);
    mockAttention.mockResolvedValue(ATTENTION);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.clearAllMocks();
    vi.useRealTimers();
  });

  it("hydrates immediately from the GET endpoints (source: poll)", async () => {
    const store = makeStore();
    const { result } = renderHook(() => useFleetEvents(), { wrapper: wrapperFor(store) });
    await waitFor(() => expect(result.current.overview).not.toBeNull());
    expect(result.current.attention).toEqual(ATTENTION);
    expect(result.current.source).toBe("poll");
    expect(result.current.updatedAt).toBeGreaterThan(0);
    // Freshness is also stamped into settings for the chrome's live pill.
    expect(store.getState().settings.lastUpdatedAt).toBeGreaterThan(0);
  });

  it("switches to the SSE source when the stream delivers events", async () => {
    const store = makeStore();
    const { result } = renderHook(() => useFleetEvents(), { wrapper: wrapperFor(store) });
    expect(MockEventSource.instances).toHaveLength(1);
    const updated = { ...OVERVIEW, fleet: { ...OVERVIEW.fleet, pending: 99 } };
    act(() => MockEventSource.instances[0].emit("overview", updated));
    expect(result.current.source).toBe("sse");
    expect(result.current.overview?.fleet.pending).toBe(99);
    act(() => MockEventSource.instances[0].emit("attention", ATTENTION));
    expect(result.current.attention).toEqual(ATTENTION);
  });

  it("falls back to polling and reconnects with backoff when the stream errors", async () => {
    vi.useFakeTimers();
    const store = makeStore();
    const { result } = renderHook(() => useFleetEvents(), { wrapper: wrapperFor(store) });
    await act(async () => {}); // flush the initial fetch
    expect(mockOverview).toHaveBeenCalledTimes(1);

    const es = MockEventSource.instances[0];
    act(() => es.fail());
    expect(es.closed).toBe(true);
    expect(result.current.source).toBe("poll");

    // Poll interval (default 8s) keeps data flowing while SSE is down, and a
    // reconnect attempt is scheduled (5s initial backoff).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(8_000);
    });
    expect(mockOverview).toHaveBeenCalledTimes(2);
    expect(MockEventSource.instances).toHaveLength(2);
  });

  it("keeps polling data flowing after an SSE error (UI renders on polling alone)", async () => {
    vi.useFakeTimers();
    const store = makeStore();
    const { result } = renderHook(() => useFleetEvents(), { wrapper: wrapperFor(store) });
    act(() => MockEventSource.instances[0].fail());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(8_000);
    });
    expect(result.current.overview).toEqual(OVERVIEW);
    expect(result.current.source).toBe("poll");
    expect(result.current.error).toBe("");
  });

  it("exposes the transport error when the endpoints are absent (backend not landed)", async () => {
    mockOverview.mockRejectedValue(new Error("Request failed with status code 404"));
    mockAttention.mockRejectedValue(new Error("Request failed with status code 404"));
    const store = makeStore();
    const { result } = renderHook(() => useFleetEvents(), { wrapper: wrapperFor(store) });
    act(() => MockEventSource.instances[0].fail());
    await waitFor(() => expect(result.current.error).not.toBe(""));
    expect(result.current.overview).toBeNull();
    expect(result.current.attention).toBeNull();
    expect(result.current.updatedAt).toBe(0);
  });

  it("takes one snapshot but opens no stream and no interval while paused", async () => {
    vi.useFakeTimers();
    const store = makeStore();
    store.dispatch(togglePolling()); // pollingActive: true -> false
    renderHook(() => useFleetEvents(), { wrapper: wrapperFor(store) });
    await act(async () => {});
    expect(mockOverview).toHaveBeenCalledTimes(1);
    expect(MockEventSource.instances).toHaveLength(0);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(mockOverview).toHaveBeenCalledTimes(1);
  });
});
