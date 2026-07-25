import { describe, it, expect } from "vitest";
import {
  workerBudget,
  budgetTone,
  stuckWorkers,
  COVERAGE_TONE,
  coverageChipLabel,
  filterCoverageRows,
  activeTaskDrawerSearch,
} from "./workers";
import { parsePeek } from "./urlstate";
import { ServerInfo, WorkerInfo } from "../api";
import { CoverageRow } from "../api-fleet";

const NOW = Date.parse("2026-07-25T12:00:00Z");

function worker(overrides: Partial<WorkerInfo>): WorkerInfo {
  return {
    task_id: "task-1",
    queue: "default",
    task_type: "email:send",
    task_payload: "{}",
    start_time: "2026-07-25T11:50:00Z", // 10m before NOW
    deadline: "2026-07-25T12:10:00Z", // 20m budget
    ...overrides,
  };
}

function server(overrides: Partial<ServerInfo>): ServerInfo {
  return {
    id: "srv-1",
    host: "wrk-01",
    pid: 100,
    concurrency: 8,
    queue_priorities: { default: 1 },
    strict_priority_enabled: false,
    start_time: "2026-07-25T00:00:00Z",
    status: "active",
    active_workers: [],
    ...overrides,
  };
}

describe("workerBudget (budget-% computation)", () => {
  it("computes elapsed, budget and pct from start/deadline", () => {
    const b = workerBudget("2026-07-25T11:50:00Z", "2026-07-25T12:10:00Z", NOW);
    expect(b.elapsedMs).toBe(10 * 60_000);
    expect(b.budgetMs).toBe(20 * 60_000);
    expect(b.pct).toBeCloseTo(0.5);
  });

  it("exceeds 1 past the deadline instead of lying with a clamp", () => {
    const b = workerBudget("2026-07-25T11:00:00Z", "2026-07-25T11:30:00Z", NOW);
    expect(b.pct).toBeCloseTo(2);
  });

  it("yields elapsed but a null budget when the deadline is missing", () => {
    const b = workerBudget("2026-07-25T11:50:00Z", "", NOW);
    expect(b.elapsedMs).toBe(10 * 60_000);
    expect(b.budgetMs).toBeNull();
    expect(b.pct).toBeNull();
  });

  it("rejects a deadline at or before the start", () => {
    const b = workerBudget("2026-07-25T11:50:00Z", "2026-07-25T11:50:00Z", NOW);
    expect(b.pct).toBeNull();
  });

  it("treats unparsable or zero-time start as unknown", () => {
    expect(workerBudget("", "2026-07-25T12:10:00Z", NOW).elapsedMs).toBeNull();
    expect(workerBudget("not-a-time", "2026-07-25T12:10:00Z", NOW).elapsedMs).toBeNull();
    expect(workerBudget("0001-01-01T00:00:00Z", "2026-07-25T12:10:00Z", NOW).elapsedMs).toBeNull();
  });

  it("clamps a start in the future (clock skew) to zero elapsed", () => {
    const b = workerBudget("2026-07-25T12:00:30Z", "2026-07-25T12:10:00Z", NOW);
    expect(b.elapsedMs).toBe(0);
    expect(b.pct).toBe(0);
  });
});

describe("budgetTone", () => {
  it("is quiet below 75%", () => {
    expect(budgetTone(null)).toBeNull();
    expect(budgetTone(0)).toBeNull();
    expect(budgetTone(0.74)).toBeNull();
  });

  it("warns at 75% and goes crit at 90% and beyond", () => {
    expect(budgetTone(0.75)).toBe("warn");
    expect(budgetTone(0.89)).toBe("warn");
    expect(budgetTone(0.9)).toBe("crit");
    expect(budgetTone(1.4)).toBe("crit");
  });
});

describe("stuckWorkers (fleet-wide sort)", () => {
  it("flattens all servers and sorts by %-of-budget descending", () => {
    const servers = [
      server({
        id: "a",
        host: "wrk-01",
        active_workers: [
          // 50% of budget
          worker({ task_id: "half" }),
          // 96%: 28m48s into a 30m budget
          worker({
            task_id: "nearly",
            start_time: "2026-07-25T11:31:12Z",
            deadline: "2026-07-25T12:01:12Z",
          }),
        ],
      }),
      server({
        id: "b",
        host: "wrk-02",
        active_workers: [
          // 150% — past deadline, must rank first
          worker({
            task_id: "blown",
            start_time: "2026-07-25T11:30:00Z",
            deadline: "2026-07-25T11:50:00Z",
          }),
        ],
      }),
    ];
    const stuck = stuckWorkers(servers, NOW);
    expect(stuck.map((s) => s.worker.task_id)).toEqual(["blown", "nearly", "half"]);
    expect(stuck[0].host).toBe("wrk-02");
    expect(stuck[0].pct).toBeCloseTo(1.5);
  });

  it("excludes workers without a computable budget — no honest rank exists", () => {
    const servers = [
      server({
        active_workers: [worker({ task_id: "no-deadline", deadline: "" }), worker({})],
      }),
    ];
    const stuck = stuckWorkers(servers, NOW);
    expect(stuck.map((s) => s.worker.task_id)).toEqual(["task-1"]);
  });

  it("caps the strip at the limit (top 10 by default)", () => {
    const many = Array.from({ length: 15 }, (_, i) =>
      worker({
        task_id: `t-${String(i).padStart(2, "0")}`,
        // spread pcts: t-00 elapsed 1m of 20m … t-14 elapsed 15m of 20m
        start_time: new Date(NOW - (i + 1) * 60_000).toISOString(),
      })
    );
    const stuck = stuckWorkers([server({ active_workers: many })], NOW);
    expect(stuck).toHaveLength(10);
    expect(stuck[0].worker.task_id).toBe("t-14"); // worst first
  });
});

describe("coverage chips (uncovered/starved chip logic)", () => {
  it("pairs every status with tone AND wording — never color alone", () => {
    expect(COVERAGE_TONE.uncovered).toBe("crit");
    expect(coverageChipLabel("uncovered")).toBe("UNCOVERED");
    expect(COVERAGE_TONE.starved).toBe("warn");
    expect(coverageChipLabel("starved")).toBe("STARVED");
    expect(COVERAGE_TONE.ok).toBe("good");
    expect(coverageChipLabel("ok")).toBe("COVERED");
  });
});

describe("filterCoverageRows (who consumes Z typeahead)", () => {
  const rows: CoverageRow[] = [
    { queue: "media:transcode", consumers: [], total_concurrency: 0, status: "ok" },
    { queue: "billing:invoice", consumers: [], total_concurrency: 0, status: "ok" },
    { queue: "search:reindex", consumers: [], total_concurrency: 0, status: "uncovered" },
  ];

  it("returns everything for an empty or whitespace query", () => {
    expect(filterCoverageRows(rows, "")).toHaveLength(3);
    expect(filterCoverageRows(rows, "   ")).toHaveLength(3);
  });

  it("matches case-insensitive substrings of the queue name", () => {
    expect(filterCoverageRows(rows, "TRANS").map((r) => r.queue)).toEqual([
      "media:transcode",
    ]);
    expect(filterCoverageRows(rows, "in").map((r) => r.queue)).toEqual([
      "billing:invoice",
      "search:reindex",
    ]);
  });
});

describe("activeTaskDrawerSearch (drawer deep link)", () => {
  it("opens the Tasks console on state=active for the queue with the task peeked", () => {
    const search = activeTaskDrawerSearch("media:transcode", "8f3a2c");
    const params = new URLSearchParams(search);
    expect(params.get("state")).toBe("active");
    expect(params.get("queue")).toBe("media:transcode");
    expect(parsePeek(params.get("peek"))).toEqual({
      queue: "media:transcode",
      id: "8f3a2c",
    });
  });

  it("survives queue names containing slashes via the peek convention", () => {
    const search = activeTaskDrawerSearch("a/b", "id-1");
    const params = new URLSearchParams(search);
    expect(parsePeek(params.get("peek"))).toEqual({ queue: "a/b", id: "id-1" });
  });
});
