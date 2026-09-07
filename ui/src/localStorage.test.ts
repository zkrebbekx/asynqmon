// loadState / sanitizeSettings: the persisted settings blob is validated
// field by field (review #55.1). The key is shared with upstream asynqmon
// 0.7.2, so a legacy payload and arbitrary garbage must both load to a
// usable SettingsState.

import { describe, it, expect, beforeEach, afterAll, vi } from "vitest";
import { loadState, sanitizeSettings } from "./localStorage";
import { initialState, ThemePreference } from "./reducers/settingsReducer";

describe("sanitizeSettings", () => {
  it("returns the defaults for a non-object", () => {
    expect(sanitizeSettings(null)).toEqual(initialState);
    expect(sanitizeSettings("x")).toEqual(initialState);
    expect(sanitizeSettings(undefined)).toEqual(initialState);
  });

  it("keeps valid values", () => {
    const s = sanitizeSettings({
      pollInterval: 5,
      taskRowsPerPage: 50,
      themePreference: ThemePreference.Always,
      isDrawerOpen: false,
      pollingActive: false,
    });
    expect(s.pollInterval).toBe(5);
    expect(s.taskRowsPerPage).toBe(50);
    expect(s.themePreference).toBe(ThemePreference.Always);
    expect(s.isDrawerOpen).toBe(false);
    expect(s.pollingActive).toBe(false);
  });

  it("drops an out-of-range or non-integer pollInterval back to the default", () => {
    for (const bad of [0, -1, 1, 21, 2.5, "x", null, NaN, Infinity]) {
      expect(sanitizeSettings({ pollInterval: bad }).pollInterval, String(bad)).toBe(initialState.pollInterval);
    }
    expect(sanitizeSettings({ pollInterval: 2 }).pollInterval).toBe(2);
    expect(sanitizeSettings({ pollInterval: 20 }).pollInterval).toBe(20);
  });

  it("drops a taskRowsPerPage that no table offers", () => {
    for (const bad of [0, 7, 1000, "20", null]) {
      expect(sanitizeSettings({ taskRowsPerPage: bad }).taskRowsPerPage, String(bad)).toBe(initialState.taskRowsPerPage);
    }
    for (const ok of [10, 20, 50, 100]) {
      expect(sanitizeSettings({ taskRowsPerPage: ok }).taskRowsPerPage).toBe(ok);
    }
  });

  it("drops a themePreference outside the enum", () => {
    for (const bad of [3, -1, "dark", null, 1.5]) {
      expect(sanitizeSettings({ themePreference: bad }).themePreference, String(bad)).toBe(initialState.themePreference);
    }
    expect(sanitizeSettings({ themePreference: 2 }).themePreference).toBe(ThemePreference.Never);
  });

  it("coerces non-boolean flags to their defaults", () => {
    const s = sanitizeSettings({ isDrawerOpen: "yes", pollingActive: 0 });
    expect(s.isDrawerOpen).toBe(initialState.isDrawerOpen);
    expect(s.pollingActive).toBe(initialState.pollingActive);
  });

  it("never restores lastUpdatedAt", () => {
    expect(sanitizeSettings({ lastUpdatedAt: 1234 }).lastUpdatedAt).toBe(0);
  });
});

describe("loadState", () => {
  // Node 22+ ships a global `localStorage` getter that yields undefined
  // without --localstorage-file, and the jsdom environment does not
  // override an existing global. Stub a minimal in-memory Storage.
  const mem = new Map<string, string>();
  const stub = {
    getItem: (k: string) => (mem.has(k) ? mem.get(k)! : null),
    setItem: (k: string, v: string) => void mem.set(k, String(v)),
    removeItem: (k: string) => void mem.delete(k),
    clear: () => mem.clear(),
  };
  vi.stubGlobal("localStorage", stub);
  afterAll(() => vi.unstubAllGlobals());
  beforeEach(() => mem.clear());

  it("returns {} when nothing is stored", () => {
    expect(loadState()).toEqual({});
  });

  it("loads the upstream 0.7.2 payload shape and validates its fields", () => {
    // Upstream 0.7.2 persisted exactly these four fields, unvalidated.
    localStorage.setItem(
      "asynqmon:state",
      JSON.stringify({
        settings: { pollInterval: 0, themePreference: 1, isDrawerOpen: false, taskRowsPerPage: 10 },
      })
    );
    const st = loadState();
    expect(st.settings).toEqual({
      ...initialState,
      pollInterval: initialState.pollInterval, // 0 is invalid: default
      themePreference: ThemePreference.Always,
      isDrawerOpen: false,
      taskRowsPerPage: 10,
    });
  });

  it("returns the defaults for garbage settings", () => {
    localStorage.setItem(
      "asynqmon:state",
      JSON.stringify({ settings: { pollInterval: "x", taskRowsPerPage: -5, themePreference: "auto", isDrawerOpen: null } })
    );
    expect(loadState().settings).toEqual(initialState);
    localStorage.setItem("asynqmon:state", JSON.stringify({ settings: 42 }));
    expect(loadState().settings).toEqual(initialState);
    localStorage.setItem("asynqmon:state", "[]");
    expect(loadState().settings).toEqual(initialState);
  });

  it("returns {} for unparseable JSON", () => {
    localStorage.setItem("asynqmon:state", "{not json");
    expect(loadState()).toEqual({});
  });
});
