import { initialState as settingsInitialState, SettingsState, ThemePreference } from "./reducers/settingsReducer"
import { rowsPerPageOptions } from "./constants";
import { AppState } from "./store";

const LOCAL_STORAGE_KEY = "asynqmon:state";

// Bounds for the poll interval in seconds. They match the settings slider.
export const POLL_INTERVAL_MIN = 2;
export const POLL_INTERVAL_MAX = 20;

const isInt = (v: unknown): v is number => typeof v === "number" && Number.isInteger(v);

// sanitizeSettings validates every persisted settings field and returns a
// complete SettingsState. An invalid or missing field falls back to its
// default. The localStorage key is shared with upstream asynqmon 0.7.2, so
// a legacy payload (different field set, unvalidated numbers) must load
// without producing setInterval(tick, 0) or a page size no table offers.
export function sanitizeSettings(raw: unknown): SettingsState {
  const out: SettingsState = { ...settingsInitialState };
  if (raw === null || typeof raw !== "object") return out;
  const s = raw as Record<string, unknown>;

  if (isInt(s.pollInterval) && s.pollInterval >= POLL_INTERVAL_MIN && s.pollInterval <= POLL_INTERVAL_MAX) {
    out.pollInterval = s.pollInterval;
  }
  if (isInt(s.taskRowsPerPage) && (rowsPerPageOptions as readonly number[]).includes(s.taskRowsPerPage)) {
    out.taskRowsPerPage = s.taskRowsPerPage;
  }
  if (isInt(s.themePreference) && Object.values(ThemePreference).includes(s.themePreference)) {
    out.themePreference = s.themePreference as ThemePreference;
  }
  if (typeof s.isDrawerOpen === "boolean") out.isDrawerOpen = s.isDrawerOpen;
  if (typeof s.pollingActive === "boolean") out.pollingActive = s.pollingActive;
  // lastUpdatedAt is session-only and is never restored (see SESSION_ONLY_SETTINGS).
  return out;
}

export function loadState(): Partial<AppState> {
  try {
    const serializedState = localStorage.getItem(LOCAL_STORAGE_KEY);
    if (serializedState === null) {
      return {};
    }
    const savedState = JSON.parse(serializedState);
    return {
      settings: sanitizeSettings(savedState?.settings),
    };
  } catch (err) {
    console.error("loadState: could not load state ", err);
    return {};
  }
}

// Fields that are live session state, not preferences: persisting them both
// churned localStorage once per poll tick forever (pollTick bumps
// lastUpdatedAt on every polled view) and made the header briefly show the
// PREVIOUS session's "updated Nh ago" on load.
const SESSION_ONLY_SETTINGS = ["lastUpdatedAt"] as const;

let lastSerialized = "";

export function saveState(state: AppState) {
  try {
    const settings: Record<string, unknown> = { ...state.settings };
    for (const k of SESSION_ONLY_SETTINGS) {
      delete settings[k];
    }
    const serializedState = JSON.stringify({ settings });
    if (serializedState === lastSerialized) {
      return; // nothing preference-shaped changed — skip the write
    }
    localStorage.setItem(LOCAL_STORAGE_KEY, serializedState);
    lastSerialized = serializedState;
  } catch (err) {
    console.error("saveState: could not save state: ", err);
  }
}
