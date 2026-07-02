import { combineReducers, configureStore, Middleware } from "@reduxjs/toolkit";
import { useDispatch } from "react-redux";
import { toast } from "sonner";
import settingsReducer from "./reducers/settingsReducer";
import queuesReducer from "./reducers/queuesReducer";
import tasksReducer from "./reducers/tasksReducer";
import groupsReducer from "./reducers/groupsReducer";
import serversReducer from "./reducers/serversReducer";
import schedulerEntriesReducer from "./reducers/schedulerEntriesReducer";
import snackbarReducer from "./reducers/snackbarReducer";
import queueStatsReducer from "./reducers/queueStatsReducer";
import redisInfoReducer from "./reducers/redisInfoReducer";
import metricsReducer from "./reducers/metricsReducer";
import { loadState, saveState } from "./localStorage";

const rootReducer = combineReducers({
  settings: settingsReducer,
  queues: queuesReducer,
  tasks: tasksReducer,
  groups: groupsReducer,
  servers: serversReducer,
  schedulerEntries: schedulerEntriesReducer,
  snackbar: snackbarReducer,
  queueStats: queueStatsReducer,
  redis: redisInfoReducer,
  metrics: metricsReducer,
});

const preloadedState = loadState();

// AppState is the top-level application state maintained by redux store.
export type AppState = ReturnType<typeof rootReducer>;

// Surface failed mutations as toasts. List/get errors already render inline
// in their views (and would flood the toaster on every poll tick during an
// outage), so only mutation-style *_ERROR actions are shown. The action type
// doubles as the toast id so a repeated failure updates one toast instead of
// stacking.
const errorToastMiddleware: Middleware = () => (next) => (action) => {
  const a = action as { type?: string; error?: string };
  if (
    typeof a.type === "string" &&
    a.type.endsWith("_ERROR") &&
    !a.type.startsWith("LIST_") &&
    !a.type.startsWith("GET_") &&
    typeof a.error === "string" &&
    a.error !== ""
  ) {
    toast.error(a.error, { id: a.type });
  }
  return next(action);
};

const store = configureStore({
  reducer: rootReducer,
  // Cast: RTK's preloadedState inference trips over combineReducers +
  // Partial<AppState> here (resolves the slice states to `never`).
  preloadedState: preloadedState as any,
  middleware: (getDefaultMiddleware) =>
    getDefaultMiddleware().concat(errorToastMiddleware),
});

// AppDispatch includes the thunk middleware signature, so components can
// dispatch async action creators without casting.
export type AppDispatch = typeof store.dispatch;
export const useAppDispatch = useDispatch.withTypes<AppDispatch>();

// Persist user settings (theme, poll interval, rows per page, drawer state)
// across reloads. Trailing-edge throttle so bursts of dispatches write once.
let saveTimer: ReturnType<typeof setTimeout> | null = null;
store.subscribe(() => {
  if (saveTimer !== null) return;
  saveTimer = setTimeout(() => {
    saveTimer = null;
    saveState(store.getState());
  }, 1000);
});

export default store;
