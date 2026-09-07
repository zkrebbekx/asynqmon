// Helpers for the Tasks-console selection verbs (review #55.7).
//
// A selection can hold up to one page of rows (100). Firing one HTTP
// request per row saturated the browser 6-connections-per-host budget and
// bypassed the server per-request work accounting. Two rules replace that:
//
//  1. above BATCH_SELECTION_THRESHOLD targets, group the selection by queue
//     and send one POST .../:batch_<verb> per queue;
//  2. at or below the threshold, still cap the fan-out at
//     SELECTION_CONCURRENCY in-flight single-task requests.
//
// The console filters one state at a time, so a queue group is also a
// queue+state group.

// Selections larger than this go through the batch endpoints.
export const BATCH_SELECTION_THRESHOLD = 20;

// Maximum number of in-flight requests for either strategy.
export const SELECTION_CONCURRENCY = 4;

export interface QueueGroup {
  queue: string;
  ids: string[];
}

// groupByQueue collects task ids per queue. It keeps first-encounter order
// for the queues and for the ids inside each queue. A repeated id in the
// same queue is kept once.
export function groupByQueue(tasks: readonly { queue: string; id: string }[]): QueueGroup[] {
  const byQueue = new Map<string, string[]>();
  const seen = new Set<string>();
  for (const t of tasks) {
    const key = `${t.queue} ${t.id}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const ids = byQueue.get(t.queue);
    if (ids) {
      ids.push(t.id);
    } else {
      byQueue.set(t.queue, [t.id]);
    }
  }
  return Array.from(byQueue, ([queue, ids]) => ({ queue, ids }));
}

// runWithConcurrency runs fn over items with at most `limit` calls in
// flight. Every item runs even when an earlier one fails. The first
// rejection is re-thrown once all of them settle, so the caller reports one
// error and refetches a settled state.
export async function runWithConcurrency<T>(
  items: readonly T[],
  limit: number,
  fn: (item: T) => Promise<unknown>
): Promise<void> {
  const max = Math.max(1, Math.min(limit, items.length));
  let next = 0;
  let firstError: unknown = undefined;
  const worker = async () => {
    for (;;) {
      const i = next++;
      if (i >= items.length) return;
      try {
        await fn(items[i]);
      } catch (e) {
        if (firstError === undefined) firstError = e;
      }
    }
  };
  await Promise.all(Array.from({ length: max }, worker));
  if (firstError !== undefined) throw firstError;
}
