import { describe, it, expect, vi } from "vitest";
import { BATCH_SELECTION_THRESHOLD, groupByQueue, runWithConcurrency, SELECTION_CONCURRENCY } from "./selection";

const t = (queue: string, id: string) => ({ queue, id });

describe("groupByQueue", () => {
  it("groups ids per queue in first-encounter order", () => {
    expect(
      groupByQueue([t("a", "1"), t("b", "2"), t("a", "3"), t("c", "4"), t("b", "5")])
    ).toEqual([
      { queue: "a", ids: ["1", "3"] },
      { queue: "b", ids: ["2", "5"] },
      { queue: "c", ids: ["4"] },
    ]);
  });

  it("returns [] for an empty selection and one group for one queue", () => {
    expect(groupByQueue([])).toEqual([]);
    expect(groupByQueue([t("q", "1"), t("q", "2")])).toEqual([{ queue: "q", ids: ["1", "2"] }]);
  });

  it("keeps a repeated id once per queue, and the same id in two queues twice", () => {
    expect(groupByQueue([t("q", "1"), t("q", "1")])).toEqual([{ queue: "q", ids: ["1"] }]);
    expect(groupByQueue([t("a", "1"), t("b", "1")])).toEqual([
      { queue: "a", ids: ["1"] },
      { queue: "b", ids: ["1"] },
    ]);
  });

  it("turns a 100-row selection over 3 queues into 3 requests", () => {
    const rows = Array.from({ length: 100 }, (_, i) => t(`q${i % 3}`, `t${i}`));
    const groups = groupByQueue(rows);
    expect(groups.length).toBe(3);
    expect(groups.reduce((n, g) => n + g.ids.length, 0)).toBe(100);
    expect(rows.length).toBeGreaterThan(BATCH_SELECTION_THRESHOLD);
  });
});

describe("runWithConcurrency", () => {
  it("runs every item and never exceeds the limit in flight", async () => {
    let inFlight = 0;
    let peak = 0;
    const done: number[] = [];
    await runWithConcurrency(Array.from({ length: 25 }, (_, i) => i), SELECTION_CONCURRENCY, async (i) => {
      inFlight++;
      peak = Math.max(peak, inFlight);
      await Promise.resolve();
      await Promise.resolve();
      done.push(i);
      inFlight--;
    });
    expect(done.length).toBe(25);
    expect(new Set(done).size).toBe(25);
    expect(peak).toBeLessThanOrEqual(SELECTION_CONCURRENCY);
  });

  it("does nothing for an empty list", async () => {
    const fn = vi.fn();
    await runWithConcurrency([], 4, fn);
    expect(fn).not.toHaveBeenCalled();
  });

  it("runs the remaining items after a failure and re-throws the first error", async () => {
    const seen: number[] = [];
    const err = new Error("boom-2");
    await expect(
      runWithConcurrency([0, 1, 2, 3, 4, 5], 2, async (i) => {
        seen.push(i);
        if (i === 2) throw err;
        if (i === 4) throw new Error("boom-4");
      })
    ).rejects.toBe(err);
    expect(seen.sort()).toEqual([0, 1, 2, 3, 4, 5]);
  });
});
