import { describe, it, expect } from "vitest";
import {
  TIME_CHIPS,
  TimeChip,
  timeChipClause,
  applyTimeChip,
  isTimeChipActive,
} from "./timechips";
import { CONSOLE_STATES } from "./urlstate";

// Fixed "now" so agoMs chips are deterministic.
const NOW = new Date("2026-07-27T12:00:00Z");

describe("TIME_CHIPS per-state tables", () => {
  // Mirror of the backend §3.4 field/state matrix, TIME fields only
  // (aql/aql_test.go). A chip may only write a field its state can answer.
  const LEGAL_TIME_FIELDS: Record<string, string[]> = {
    pending: ["pending_age"],
    active: ["running", "deadline_pct"],
    aggregating: ["group_age"],
    scheduled: ["next_run", "past_due"],
    retry: ["next_run", "past_due", "failed_after", "failed_before"],
    archived: ["failed_after", "failed_before", "died"],
    completed: ["expires"],
  };

  it("has a row for every console state", () => {
    for (const st of CONSOLE_STATES) {
      expect(TIME_CHIPS[st]).toBeDefined();
    }
  });

  it("uses only matrix-legal fields for each state", () => {
    for (const st of CONSOLE_STATES) {
      for (const chip of TIME_CHIPS[st]) {
        expect(
          LEGAL_TIME_FIELDS[st],
          `${st} chip "${chip.label}" writes ${chip.field}`
        ).toContain(chip.field);
      }
    }
  });

  it("has no chips for aggregating (no time predicates there)", () => {
    expect(TIME_CHIPS.aggregating).toEqual([]);
  });

  it("gives every non-aggregating state at least two picks", () => {
    for (const st of CONSOLE_STATES) {
      if (st === "aggregating") continue;
      expect(TIME_CHIPS[st].length).toBeGreaterThanOrEqual(2);
    }
  });

  it("stamps failed_after chips at click time (agoMs), never as a literal", () => {
    const agoChips = [...TIME_CHIPS.retry, ...TIME_CHIPS.archived].filter(
      (c) => c.field === "failed_after"
    );
    expect(agoChips.length).toBeGreaterThan(0);
    for (const chip of agoChips) {
      expect(chip.agoMs).toBeGreaterThan(0);
      expect(chip.value).toBe("");
    }
  });
});

describe("timeChipClause", () => {
  it("renders duration clauses verbatim", () => {
    const chip: TimeChip = { label: ">2h", field: "pending_age", op: ">", value: "2h" };
    expect(timeChipClause(chip, NOW)).toBe("pending_age>2h");
  });

  it("renders flag clauses as the bare field", () => {
    const chip: TimeChip = { label: "past due", field: "past_due", op: "", value: "" };
    expect(timeChipClause(chip, NOW)).toBe("past_due");
  });

  it("computes absolute RFC3339 from now for agoMs chips", () => {
    const chip: TimeChip = {
      label: "failed last 1h",
      field: "failed_after",
      op: "=",
      value: "",
      agoMs: 3600_000,
    };
    expect(timeChipClause(chip, NOW)).toBe("failed_after=2026-07-27T11:00:00Z");
  });

  it("stamps whole-second RFC3339 (no fractional seconds)", () => {
    const chip: TimeChip = {
      label: "last 24h",
      field: "failed_after",
      op: "=",
      value: "",
      agoMs: 24 * 3600_000,
    };
    const withMs = new Date("2026-07-27T12:00:00.123Z");
    expect(timeChipClause(chip, withMs)).toBe("failed_after=2026-07-26T12:00:00Z");
  });
});

describe("applyTimeChip (compose into the query text)", () => {
  const waiting5m: TimeChip = { label: "waiting >5m", field: "pending_age", op: ">", value: "5m" };
  const waiting2h: TimeChip = { label: ">2h", field: "pending_age", op: ">", value: "2h" };

  it("appends when the field is absent", () => {
    expect(applyTimeChip("queue=email state=pending", waiting5m, NOW)).toBe(
      "queue=email state=pending pending_age>5m"
    );
  });

  it("appends to an empty query", () => {
    expect(applyTimeChip("", waiting5m, NOW)).toBe("pending_age>5m");
  });

  it("replaces an existing clause of the same field in place", () => {
    expect(applyTimeChip("pending_age>5m queue=email", waiting2h, NOW)).toBe(
      "pending_age>2h queue=email"
    );
  });

  it("replaces regardless of the existing clause's operator or value", () => {
    expect(applyTimeChip("queue=a pending_age>=30m", waiting5m, NOW)).toBe(
      "queue=a pending_age>5m"
    );
  });

  it("collapses duplicate clauses of the field to one", () => {
    expect(applyTimeChip("pending_age>5m pending_age>30m", waiting2h, NOW)).toBe(
      "pending_age>2h"
    );
  });

  it("leaves clauses of other fields (and free text) untouched", () => {
    const q = 'error~"gateway timeout" boom state=retry';
    const chip: TimeChip = { label: "fires <10m", field: "next_run", op: "<", value: "10m" };
    expect(applyTimeChip(q, chip, NOW)).toBe(
      'error~"gateway timeout" boom state=retry next_run<10m'
    );
  });

  it("composes flag clauses (past_due) and keeps them single", () => {
    const chip: TimeChip = { label: "past due", field: "past_due", op: "", value: "" };
    expect(applyTimeChip("state=scheduled", chip, NOW)).toBe("state=scheduled past_due");
    expect(applyTimeChip("state=scheduled past_due", chip, NOW)).toBe(
      "state=scheduled past_due"
    );
  });

  it("re-stamps an agoMs clause with the fresh absolute timestamp", () => {
    const chip: TimeChip = {
      label: "failed last 1h",
      field: "failed_after",
      op: "=",
      value: "",
      agoMs: 3600_000,
    };
    const q = "state=retry failed_after=2026-07-27T09:00:00Z";
    expect(applyTimeChip(q, chip, NOW)).toBe("state=retry failed_after=2026-07-27T11:00:00Z");
  });

  it("different fields coexist (next_run + failed_after)", () => {
    const fires: TimeChip = { label: "fires <10m", field: "next_run", op: "<", value: "10m" };
    const failed: TimeChip = {
      label: "failed last 1h",
      field: "failed_after",
      op: "=",
      value: "",
      agoMs: 3600_000,
    };
    let q = applyTimeChip("state=retry", fires, NOW);
    q = applyTimeChip(q, failed, NOW);
    expect(q).toBe("state=retry next_run<10m failed_after=2026-07-27T11:00:00Z");
  });
});

describe("isTimeChipActive", () => {
  const waiting5m: TimeChip = { label: "waiting >5m", field: "pending_age", op: ">", value: "5m" };

  it("is active only on the exact clause token", () => {
    expect(isTimeChipActive("queue=email pending_age>5m", waiting5m)).toBe(true);
    expect(isTimeChipActive("queue=email pending_age>30m", waiting5m)).toBe(false);
    expect(isTimeChipActive("queue=email", waiting5m)).toBe(false);
  });

  it("matches flag clauses", () => {
    const chip: TimeChip = { label: "past due", field: "past_due", op: "", value: "" };
    expect(isTimeChipActive("state=scheduled past_due", chip)).toBe(true);
    expect(isTimeChipActive("state=scheduled", chip)).toBe(false);
  });

  it("never marks agoMs chips active (their token is moment-dependent)", () => {
    const chip: TimeChip = {
      label: "failed last 1h",
      field: "failed_after",
      op: "=",
      value: "",
      agoMs: 3600_000,
    };
    expect(isTimeChipActive("failed_after=2026-07-27T11:00:00Z", chip)).toBe(false);
  });
});
