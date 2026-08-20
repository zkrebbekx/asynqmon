import { initialState as settingsInitialState } from "./reducers/settingsReducer"
import { AppState } from "./store";

const LOCAL_STORAGE_KEY = "asynqmon:state";

export function loadState(): Partial<AppState> {
  try {
    const serializedState = localStorage.getItem(LOCAL_STORAGE_KEY);
    if (serializedState === null) {
      return {};
    }
    const savedState = JSON.parse(serializedState);
    return {
      settings: {
        ...settingsInitialState,
        ...(savedState.settings || {}),
      }
    }
  } catch (err) {
    console.log("loadState: could not load state ", err)
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
