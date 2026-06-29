// Case-insensitive substring match used by the queue and task filters.
// An empty/whitespace query matches everything.
export function matchesQuery(haystack: string, query: string): boolean {
  const needle = query.trim().toLowerCase();
  if (!needle) return true;
  return haystack.toLowerCase().includes(needle);
}
