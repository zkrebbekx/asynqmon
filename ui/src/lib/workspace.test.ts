import { describe, it, expect } from "vitest";
import type { TaskInfo } from "../api";
import type { FleetQueueRow, RetryHistogramResponse } from "../api-fleet";
import {
  parseWorkspaceParams,
  serializeWorkspaceTab,
  activeTaskBudget,
  sortByBudgetPct,
  synthesizeAttention,
  upcomingRetries,
  formatCountdown,
  orphanedActives,
  pastDueScheduled,
  archivedDelta,
  retryFireWithin,
  histogramBars,
  retryHistogramView,
  bucketWaitsMs,
  pendingWaitView,
  mergeErrorClusters,
  healthySummary,
  ARCHIVE_CAP,
} from "./workspace";

const NOW = Date.parse("2026-07-25T12:00:00Z");

function task(partial: Partial<TaskInfo>): TaskInfo {
  return {
    id: "t-1",
    queue: "media:transcode",
    type: "job:x",
    payload: "{}",
    state: "active",
    start_time: "",
    max_retry: 25,
    retried: 0,
    last_failed_at: "",
    error_message: "",
    next_process_at: "",
    timeout_seconds: 0,
    deadline: "",
    group: "",
    completed_at: "",
    result: "",
    ttl_seconds: 0,
    is_orphaned: false,
    ...partial,
  };
}

function fleetRow(partial: Partial<FleetQueueRow>): FleetQueueRow {
  return {
    queue: "media:transcode",
    pending: 0,
    active: 0,
    scheduled: 0,
    retry: 0,
    archived: 0,
    completed: 0,
    groups: 0,
    paused: false,
    processed_today: 0,
    failed_today: 0,
    error_rate: 0,
    orphan_candidates: 0,
    past_due_scheduled: 0,
    oldest_pending_age_ms: 0,
    latency_ms: 0,
    consumers: 0,
    refreshed_at: new Date(NOW).toISOString(),
    ...partial,
  };
}

/**************************************************************
                    Tab & focus routing
 **************************************************************/

describe("parseWorkspaceParams (focus routing)", () => {
  it("defaults to the Attention tab — never lands on active", () => {
    expect(parseWorkspaceParams(new URLSearchParams())).toEqual({
      tab: "attention",
      focus: null,
    });
  });

  it("reads a state tab from the legacy status param", () => {
    const p = parseWorkspaceParams(new URLSearchParams("status=retry"));
    expect(p.tab).toBe("retry");
    expect(p.focus).toBe(null);
  });

  it("degrades invalid tabs to Attention instead of crashing", () => {
    expect(parseWorkspaceParams(new URLSearchParams("status=bogus")).tab).toBe(
      "attention"
    );
  });

  it("accepts ?focus=<state> from attentionTarget deep links", () => {
    const p = parseWorkspaceParams(new URLSearchParams("focus=retry"));
    expect(p.tab).toBe("attention");
    expect(p.focus).toBe("retry");
  });

  it("rejects invalid focus values", () => {
    expect(parseWorkspaceParams(new URLSearchParams("focus=bogus")).focus).toBe(null);
  });

  it("ignores focus when an explicit state tab is set", () => {
    const p = parseWorkspaceParams(new URLSearchParams("status=pending&focus=retry"));
    expect(p.tab).toBe("pending");
    expect(p.focus).toBe(null);
  });
});

describe("serializeWorkspaceTab", () => {
  it("omits the default tab and clears focus on navigation", () => {
    const base = new URLSearchParams("focus=retry&other=1");
    const out = serializeWorkspaceTab("attention", base);
    expect(out.get("status")).toBe(null);
    expect(out.get("focus")).toBe(null);
    expect(out.get("other")).toBe("1"); // unowned params survive
  });

  it("writes non-default tabs", () => {
    expect(serializeWorkspaceTab("archived").get("status")).toBe("archived");
  });
});

/**************************************************************
                    Active-row budgets
 **************************************************************/

describe("activeTaskBudget", () => {
  it("computes pct from start and deadline", () => {
    const b = activeTaskBudget(
      task({
        start_time: new Date(NOW - 27 * 60_000).toISOString(),
        deadline: new Date(NOW + 3 * 60_000).toISOString(),
      }),
      NOW
    );
    expect(b.elapsedMs).toBe(27 * 60_000);
    expect(b.budgetMs).toBe(30 * 60_000);
    expect(b.pct).toBeCloseTo(0.9);
  });

  it("falls back to timeout_seconds when the deadline is absent", () => {
    const b = activeTaskBudget(
      task({ start_time: new Date(NOW - 60_000).toISOString(), timeout_seconds: 600 }),
      NOW
    );
    expect(b.budgetMs).toBe(600_000);
    expect(b.pct).toBeCloseTo(0.1);
  });

  it("returns no budget when neither deadline nor timeout exists (absent ≠ 100%)", () => {
    const b = activeTaskBudget(task({ start_time: new Date(NOW - 60_000).toISOString() }), NOW);
    expect(b.elapsedMs).toBe(60_000);
    expect(b.budgetMs).toBe(null);
    expect(b.pct).toBe(null);
  });

  it("returns nothing without a start time", () => {
    expect(activeTaskBudget(task({}), NOW)).toEqual({
      elapsedMs: null,
      budgetMs: null,
      pct: null,
    });
  });

  it("lets pct exceed 1 past the deadline — the bar clamps, the number does not", () => {
    const b = activeTaskBudget(
      task({
        start_time: new Date(NOW - 40 * 60_000).toISOString(),
        deadline: new Date(NOW - 10 * 60_000).toISOString(),
      }),
      NOW
    );
    expect(b.pct).toBeCloseTo(40 / 30);
  });
});

describe("sortByBudgetPct", () => {
  it("puts the wedged worker first and orphans last", () => {
    const wedged = task({
      id: "wedged",
      start_time: new Date(NOW - 29 * 60_000).toISOString(),
      deadline: new Date(NOW + 60_000).toISOString(),
    });
    const fresh = task({
      id: "fresh",
      start_time: new Date(NOW - 60_000).toISOString(),
      deadline: new Date(NOW + 29 * 60_000).toISOString(),
    });
    const orphan = task({ id: "orphan", is_orphaned: true });
    const noBudget = task({ id: "nobudget", start_time: new Date(NOW - 5000).toISOString() });
    const out = sortByBudgetPct([fresh, orphan, noBudget, wedged], NOW);
    expect(out.map((t) => t.id)).toEqual(["wedged", "fresh", "nobudget", "orphan"]);
  });

  it("does not mutate its input", () => {
    const a = task({ id: "a" });
    const b = task({ id: "b" });
    const input = [a, b];
    sortByBudgetPct(input, NOW);
    expect(input).toEqual([a, b]);
  });
});

/**************************************************************
                    Attention synthesis
 **************************************************************/

const emptyInput = {
  counts: null,
  fleetRow: null,
  activeTasks: [],
  scheduledTasks: [],
  retryHistogram: null,
  history: [],
  nowMs: NOW,
};

describe("synthesizeAttention", () => {
  it("produces nothing for a healthy queue", () => {
    expect(synthesizeAttention(emptyInput)).toEqual([]);
  });

  it("raises ORPHANS from the cache row and the active page (max wins)", () => {
    const items = synthesizeAttention({
      ...emptyInput,
      fleetRow: fleetRow({ orphan_candidates: 14 }),
      activeTasks: [task({ is_orphaned: true })],
    });
    const orphans = items.find((i) => i.kind === "orphans");
    expect(orphans).toBeDefined();
    expect(orphans!.chip).toBe("ORPHANS 14");
    expect(orphans!.focusState).toBe("active");
  });

  it("raises PENDING AGE at the SLO, crit at 6× the SLO", () => {
    const warn = synthesizeAttention({
      ...emptyInput,
      fleetRow: fleetRow({ oldest_pending_age_ms: 6 * 60_000 }),
    }).find((i) => i.kind === "pending_age");
    expect(warn!.tone).toBe("warn");

    const crit = synthesizeAttention({
      ...emptyInput,
      fleetRow: fleetRow({ oldest_pending_age_ms: 42 * 60_000 }),
    }).find((i) => i.kind === "pending_age");
    expect(crit!.tone).toBe("crit");
    expect(crit!.sentence).toContain("42m");
  });

  it("stays quiet below the SLO", () => {
    const items = synthesizeAttention({
      ...emptyInput,
      fleetRow: fleetRow({ oldest_pending_age_ms: 4 * 60_000 }),
    });
    expect(items.find((i) => i.kind === "pending_age")).toBeUndefined();
  });

  it("raises RETRY STORM from the exact histogram mass in 5m", () => {
    const hist: RetryHistogramResponse = {
      queue: "q",
      from: new Date(NOW).toISOString(),
      bucket_seconds: 150,
      past_due: 4,
      buckets: [600, 600, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0],
      beyond: 0,
      total: 1204,
      exact: true,
    };
    const item = synthesizeAttention({ ...emptyInput, retryHistogram: hist }).find(
      (i) => i.kind === "retry_storm"
    );
    expect(item).toBeDefined();
    expect(item!.sentence).toContain("1,204");
    expect(item!.tone).toBe("crit");
    expect(item!.focusState).toBe("retry");
  });

  it("raises PAUSED with the paused-since age", () => {
    const item = synthesizeAttention({
      ...emptyInput,
      fleetRow: fleetRow({
        paused: true,
        paused_since: new Date(NOW - 21 * 86_400_000).toISOString(),
      }),
    }).find((i) => i.kind === "paused");
    expect(item!.chip).toBe("PAUSED 21d");
  });

  it("raises ARCHIVE CAP exactly at the 10k trim cap", () => {
    const at = synthesizeAttention({
      ...emptyInput,
      counts: { archived: ARCHIVE_CAP },
    });
    expect(at.find((i) => i.kind === "archive_cap")).toBeDefined();

    const below = synthesizeAttention({
      ...emptyInput,
      counts: { archived: ARCHIVE_CAP - 1 },
    });
    expect(below.find((i) => i.kind === "archive_cap")).toBeUndefined();
  });

  it("raises PAST-DUE from the scheduled page when the cache row is absent", () => {
    const item = synthesizeAttention({
      ...emptyInput,
      scheduledTasks: [
        task({ next_process_at: new Date(NOW - 60_000).toISOString(), state: "scheduled" }),
      ],
    }).find((i) => i.kind === "past_due");
    expect(item).toBeDefined();
    expect(item!.chip).toBe("PAST-DUE 1");
    expect(item!.focusState).toBe("scheduled");
  });

  it("notes the failure delta vs yesterday from daily counters", () => {
    const item = synthesizeAttention({
      ...emptyInput,
      history: [
        { queue: "q", processed: 100, succeeded: 91, failed: 9, date: "2026-07-24" },
        { queue: "q", processed: 300, succeeded: 88, failed: 212, date: "2026-07-25" },
      ],
    }).find((i) => i.kind === "archived_delta");
    expect(item).toBeDefined();
    expect(item!.sentence).toContain("212");
    expect(item!.sentence).toContain("9 yesterday");
    expect(item!.tone).toBe("warn"); // ≥5× yesterday
  });

  it("sorts crit before warn before mut", () => {
    const items = synthesizeAttention({
      ...emptyInput,
      counts: { archived: ARCHIVE_CAP },
      fleetRow: fleetRow({ oldest_pending_age_ms: 60 * 60_000 }),
      history: [{ queue: "q", processed: 5, succeeded: 3, failed: 2, date: "2026-07-25" }],
    });
    const tones = items.map((i) => i.tone);
    expect(tones).toEqual([...tones].sort((a, b) => {
      const rank = { crit: 0, warn: 1, mut: 2 } as const;
      return rank[a] - rank[b];
    }));
  });
});

describe("attention helpers", () => {
  it("orphanedActives filters the page", () => {
    const o = task({ id: "o", is_orphaned: true });
    expect(orphanedActives([task({}), o])).toEqual([o]);
  });

  it("pastDueScheduled keeps only fire times behind now", () => {
    const due = task({ id: "due", next_process_at: new Date(NOW - 1000).toISOString() });
    const future = task({ id: "f", next_process_at: new Date(NOW + 1000).toISOString() });
    const unknown = task({ id: "u", next_process_at: "" });
    expect(pastDueScheduled([due, future, unknown], NOW)).toEqual([due]);
  });

  it("upcomingRetries sorts by next fire ascending with unknowns last", () => {
    const soon = task({ id: "soon", next_process_at: new Date(NOW + 60_000).toISOString() });
    const later = task({ id: "later", next_process_at: new Date(NOW + 300_000).toISOString() });
    const unknown = task({ id: "u", next_process_at: "" });
    const rows = upcomingRetries([unknown, later, soon], NOW);
    expect(rows.map((r) => r.task.id)).toEqual(["soon", "later", "u"]);
    expect(rows[0].countdownMs).toBe(60_000);
    expect(rows[2].countdownMs).toBe(null);
  });

  it("formatCountdown renders live countdowns and due-now", () => {
    expect(formatCountdown(272_000)).toBe("in 4m 32s");
    expect(formatCountdown(0)).toBe("due now");
    expect(formatCountdown(-5000)).toBe("due now");
    expect(formatCountdown(null)).toBe("—");
  });

  it("archivedDelta reads today/yesterday from UTC counter dates", () => {
    const history = [
      { queue: "q", processed: 10, succeeded: 9, failed: 1, date: "2026-07-24" },
      { queue: "q", processed: 20, succeeded: 15, failed: 5, date: "2026-07-25" },
    ];
    expect(archivedDelta(history, NOW)).toEqual({ today: 5, yesterday: 1 });
    expect(archivedDelta([], NOW)).toBe(null);
  });

  it("healthySummary composes the §3.3 empty state", () => {
    const s = healthySummary(
      fleetRow({ oldest_pending_age_ms: 4000 }),
      [{ queue: "q", processed: 10, succeeded: 10, failed: 0, date: "2026-07-25" }],
      NOW
    );
    expect(s).toBe("Nothing anomalous. Oldest pending 4s. No failures today.");
  });
});

/**************************************************************
                  Histogram bucket rendering
 **************************************************************/

describe("retryFireWithin", () => {
  const hist: RetryHistogramResponse = {
    queue: "q",
    from: new Date(NOW).toISOString(),
    bucket_seconds: 150,
    past_due: 3,
    buckets: [10, 20, 30, 40, 0, 0, 0, 0, 0, 0, 0, 0],
    beyond: 7,
    total: 110,
    exact: true,
  };

  it("sums past-due plus the buckets inside the horizon", () => {
    // 300s / 150s = 2 buckets + past_due.
    expect(retryFireWithin(hist, 300)).toBe(3 + 10 + 20);
  });

  it("rounds partial buckets up (never understates the storm)", () => {
    expect(retryFireWithin(hist, 200)).toBe(3 + 10 + 20); // ceil(200/150) = 2
  });

  it("is zero for absent data", () => {
    expect(retryFireWithin(null, 300)).toBe(0);
  });
});

describe("histogramBars", () => {
  it("normalizes to 0–100 with the peak at 100", () => {
    expect(histogramBars([1, 2, 4])).toEqual([25, 50, 100]);
  });

  it("renders an all-zero histogram flat, not full", () => {
    expect(histogramBars([0, 0, 0])).toEqual([0, 0, 0]);
  });

  it("handles empty input", () => {
    expect(histogramBars([])).toEqual([]);
  });
});

describe("retryHistogramView", () => {
  const hist: RetryHistogramResponse = {
    queue: "q",
    from: "2026-07-25T14:00:00.000Z",
    bucket_seconds: 150,
    past_due: 0,
    buckets: [8, 14, 26, 100, 64, 31, 0, 0, 0, 0, 0, 0],
    beyond: 5,
    total: 248,
    exact: true,
  };

  it("marks the peak bucket hot and labels the storm window", () => {
    const v = retryHistogramView(hist)!;
    expect(v.hotIndex).toBe(3);
    expect(v.bars[3]).toBe(100);
    // Bucket 3 covers +450s..+600s from 14:00 UTC (label uses local clock,
    // so only assert the shape).
    expect(v.stormLabel).toMatch(/^storm lands \d{2}:\d{2}–\d{2}:\d{2}$/);
  });

  it("labels bucket offsets in minutes", () => {
    const v = retryHistogramView(hist)!;
    expect(v.bucketLabel(0)).toBe("+0m");
    expect(v.bucketLabel(2)).toBe("+5m");
    expect(v.bucketLabel(1)).toBe("+2.5m");
  });

  it("carries the exact 5m mass and passthrough totals", () => {
    const v = retryHistogramView(hist)!;
    expect(v.next5m).toBe(8 + 14); // 2 × 150s buckets
    expect(v.total).toBe(248);
    expect(v.beyond).toBe(5);
  });

  it("returns null (not a fake empty view) for absent data", () => {
    expect(retryHistogramView(null)).toBe(null);
  });

  it("has no hot bucket and no storm label when empty", () => {
    const v = retryHistogramView({ ...hist, buckets: hist.buckets.map(() => 0) })!;
    expect(v.hotIndex).toBe(-1);
    expect(v.stormLabel).toBe("");
  });
});

describe("bucketWaitsMs", () => {
  it("folds waits into the fixed boundaries", () => {
    const buckets = bucketWaitsMs([
      500, // <1s
      5_000, // 1–10s
      30_000, // 10s–1m
      120_000, // 1–5m
      600_000, // 5–15m
      1_800_000, // 15m–1h
      7_200_000, // 1–6h
      90_000_000, // ≥6h
    ]);
    expect(buckets.map((b) => b.count)).toEqual([1, 1, 1, 1, 1, 1, 1, 1]);
    expect(buckets[0].label).toBe("<1s");
    expect(buckets[7].label).toBe("≥6h");
  });

  it("drops negative and non-finite samples instead of guessing", () => {
    const buckets = bucketWaitsMs([-5, NaN, Infinity, 500]);
    expect(buckets.reduce((s, b) => s + b.count, 0)).toBe(1);
  });

  it("puts boundary values in the upper bucket (bounds are exclusive)", () => {
    const buckets = bucketWaitsMs([1_000]);
    expect(buckets[1].count).toBe(1);
    expect(buckets[0].count).toBe(0);
  });
});

describe("pendingWaitView", () => {
  it("builds bars and carries the honesty label verbatim", () => {
    const v = pendingWaitView({
      queue: "q",
      list_len: 128_411,
      head_probes: 20,
      tail_probes: 20,
      sampled: 40,
      waits_ms: [500, 500, 5000],
      method: "sampled, head/tail-biased",
      sampled_at: new Date(NOW).toISOString(),
    })!;
    expect(v.method).toBe("sampled, head/tail-biased");
    expect(v.buckets[0].count).toBe(2);
    expect(v.bars[0]).toBe(100);
    expect(v.listLen).toBe(128_411);
  });

  it("returns null for absent data", () => {
    expect(pendingWaitView(null)).toBe(null);
  });
});

/**************************************************************
                    Error-cluster merge
 **************************************************************/

describe("mergeErrorClusters", () => {
  it("joins retry and archived groups by signature, sorted by total", () => {
    const merged = mergeErrorClusters(
      [
        { label: "oom", count: 100 },
        { label: "timeout", count: 30 },
      ],
      [
        { label: "timeout", count: 90 },
        { label: "refused", count: 10 },
      ]
    );
    expect(merged[0]).toEqual({ label: "timeout", retryCount: 30, archivedCount: 90, total: 120 });
    expect(merged[1]).toEqual({ label: "oom", retryCount: 100, archivedCount: 0, total: 100 });
    expect(merged[2].label).toBe("refused");
  });

  it("drops empty labels and respects the cap", () => {
    const merged = mergeErrorClusters(
      [
        { label: "", count: 5 },
        { label: "a", count: 1 },
        { label: "b", count: 2 },
      ],
      [],
      1
    );
    expect(merged).toEqual([{ label: "b", retryCount: 2, archivedCount: 0, total: 2 }]);
  });
});
