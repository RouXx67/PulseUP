import { createSignal, createEffect, Signal } from 'solid-js';

/**
 * Creates a signal that syncs with localStorage
 * @param key - The localStorage key
 * @param defaultValue - Default value if nothing in localStorage
 * @param parse - Optional parser function for complex types
 * @param stringify - Optional stringify function for complex types
 */
export function createLocalStorageSignal<T>(
  key: string,
  defaultValue: T,
  parse?: (value: string) => T,
  stringify?: (value: T) => string,
): Signal<T> {
  // Get initial value from localStorage
  const stored = localStorage.getItem(key);
  const initialValue =
    stored !== null ? (parse ? parse(stored) : (stored as unknown as T)) : defaultValue;

  const [value, setValue] = createSignal<T>(initialValue);

  // Sync to localStorage on changes
  createEffect(() => {
    const val = value();
    if (val === null || val === undefined) {
      localStorage.removeItem(key);
    } else {
      localStorage.setItem(key, stringify ? stringify(val) : String(val));
    }
  });

  return [value, setValue];
}

/**
 * Creates a boolean signal that syncs with localStorage
 * @param key - The localStorage key
 * @param defaultValue - Default value if nothing in localStorage
 */
export function createLocalStorageBooleanSignal(
  key: string,
  defaultValue: boolean = false,
): Signal<boolean> {
  return createLocalStorageSignal(
    key,
    defaultValue,
    (val) => val === 'true',
    (val) => String(val),
  );
}

/**
 * Creates a number signal that syncs with localStorage
 * @param key - The localStorage key
 * @param defaultValue - Default value if nothing in localStorage
 */
export function createLocalStorageNumberSignal(
  key: string,
  defaultValue: number = 0,
): Signal<number> {
  return createLocalStorageSignal(
    key,
    defaultValue,
    (val) => Number(val),
    (val) => String(val),
  );
}

/**
 * Storage keys used throughout the application
 */
export const STORAGE_KEYS = {
  // UI preferences
  DARK_MODE: 'darkMode',
  SIDEBAR_COLLAPSED: 'sidebarCollapsed',

  // Alert settings
  ALERT_HISTORY_TIME_FILTER: 'alertHistoryTimeFilter',
  ALERT_HISTORY_SEVERITY_FILTER: 'alertHistorySeverityFilter',

  // Storage settings
  STORAGE_SHOW_FILTERS: 'storageShowFilters',
  STORAGE_VIEW_MODE: 'storageViewMode',

  // Backup settings
  BACKUPS_SHOW_FILTERS: 'backupsShowFilters',
  BACKUPS_USE_RELATIVE_TIME: 'backupsUseRelativeTime',
  BACKUPS_SEARCH_HISTORY: 'backupsSearchHistory',

  // Dashboard settings
  DASHBOARD_SHOW_FILTERS: 'dashboardShowFilters',
  DASHBOARD_CARD_VIEW: 'dashboardCardView',
  DASHBOARD_AUTO_REFRESH: 'dashboardAutoRefresh',
  DASHBOARD_SEARCH_HISTORY: 'dashboardSearchHistory',

  // Storage search
  STORAGE_SEARCH_HISTORY: 'storageSearchHistory',

  // Docker search
  DOCKER_SEARCH_HISTORY: 'dockerSearchHistory',

  // Alerts search
  ALERTS_SEARCH_HISTORY: 'alertsSearchHistory',

  // API token
  API_TOKEN: 'apiToken',
} as const;
