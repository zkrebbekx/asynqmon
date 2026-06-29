import {
  DAILY_STATS_KEY_CHANGE,
  POLL_INTERVAL_CHANGE,
  POLL_TICK,
  SettingsActionTypes,
  TASK_ROWS_PER_PAGE_CHANGE,
  THEME_PREFERENCE_CHANGE,
  TOGGLE_DRAWER,
  TOGGLE_POLLING,
} from "../actions/settingsActions";
import { defaultPageSize, DailyStatsKey, defaultDailyStatsKey } from "../constants";

export enum ThemePreference {
  SystemDefault,
  Always,
  Never,
}

export interface SettingsState {
  // Time duration between data refresh.
  pollInterval: number;

  // UI theme setting.
  themePreference: ThemePreference;

  // Whether the drawer (i.e. sidebar) is open or not.
  isDrawerOpen: boolean;

  // Number of tasks displayed in task table.
  taskRowsPerPage: number,

  // Type of the chart displayed for "Processed Tasks" section in dashboard.
  dailyStatsChartType: DailyStatsKey;

  // Whether periodic background refresh (polling) is active.
  pollingActive: boolean;

  // Epoch milliseconds of the most recent data refresh (0 if never).
  lastUpdatedAt: number;
}

export const initialState: SettingsState = {
  pollInterval: 8,
  themePreference: ThemePreference.SystemDefault,
  isDrawerOpen: true,
  taskRowsPerPage: defaultPageSize,
  dailyStatsChartType: defaultDailyStatsKey,
  pollingActive: true,
  lastUpdatedAt: 0,
};

function settingsReducer(
  state = initialState,
  action: SettingsActionTypes
): SettingsState {
  switch (action.type) {
    case POLL_INTERVAL_CHANGE:
      return {
        ...state,
        pollInterval: action.value,
      };

    case THEME_PREFERENCE_CHANGE:
      return {
        ...state,
        themePreference: action.value,
      };

    case TOGGLE_DRAWER:
      return {
        ...state,
        isDrawerOpen: !state.isDrawerOpen,
      };

    case TASK_ROWS_PER_PAGE_CHANGE:
      return {
        ...state,
        taskRowsPerPage: action.value,
      }

    case DAILY_STATS_KEY_CHANGE:
      return {
        ...state,
        dailyStatsChartType: action.value,
      }

    case TOGGLE_POLLING:
      return {
        ...state,
        pollingActive: !state.pollingActive,
      };

    case POLL_TICK:
      return {
        ...state,
        lastUpdatedAt: action.value,
      };

    default:
      return state;
  }
}

export default settingsReducer;
