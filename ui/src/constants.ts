export const defaultPageSize = 20;

// Rows-per-page choices offered by the per-queue task tables. The persisted
// `taskRowsPerPage` setting is validated against this list on load.
export const rowsPerPageOptions = [10, 20, 50, 100] as const;
