// Pure-logic tests for the Hygiene screen library (§3.10): job-scope
// construction, schedule-editor logic, summary-chip derivation and honest
// formatting.

import { describe, it, expect } from "vitest";
import type { DeadLetterAction, HygieneCard } from "../api-hygiene";
import {
  HYGIENE_KINDS,
  INTERVAL_PRESETS,
  KIND_TITLES,
  activitySourceLabel,
  deadLetterActionScope,
  formatBytes,
  formatInterval,
  intervalOptions,
  nothingScheduled,
  summaryChips,
  validInterval,
} from "./hygiene";

const card = (over: Partial<HygieneCard>): HygieneCard => ({
  kind: "inventory",
  title: "Queue inventory",
  enabled: false,
  interval_seconds: 604800,
  last_generated_at: "",
  summary: null,
  ...over,
});

describe("kind catalog", () => {
  it("covers all four §3.10 reports with titles", () => {
    expect(HYGIENE_KINDS).toEqual(["inventory", "storage", "dead-letter", "scheduler-health"]);
    for (const k of HYGIENE_KINDS) expect(KIND_TITLES[k]).toBeTruthy();
  });
});

describe("deadLetterActionScope (one-click job construction)", () => {
  it("maps the action descriptor verbatim onto the BulkJobModal scope", () => {
    const action: DeadLetterAction = {
      label: "Delete archived older than 30d",
      verb: "delete",
      queue: "billing:invoice",
      state: "archived",
      aql: "died>30d",
      estimate: 8000,
    };
    expect(deadLetterActionScope(action)).toEqual({
      queue: "billing:invoice",
      state: "archived",
      q: "",
      meta: [],
      aql: "died>30d",
    });
  });
});

describe("schedule editor logic", () => {
  it("validates intervals against the backend's 1m..90d bounds", () => {
    expect(validInterval(59)).toBe(false);
    expect(validInterval(60)).toBe(true);
    expect(validInterval(90 * 24 * 3600)).toBe(true);
    expect(validInterval(90 * 24 * 3600 + 1)).toBe(false);
    expect(validInterval(NaN)).toBe(false);
    expect(validInterval(3600.5)).toBe(false);
  });

  it("names preset intervals and formats non-presets compactly", () => {
    expect(formatInterval(24 * 3600)).toBe("daily");
    expect(formatInterval(7 * 24 * 3600)).toBe("weekly");
    expect(formatInterval(30 * 24 * 3600)).toBe("monthly");
    expect(formatInterval(12 * 3600)).toBe("every 12h");
    expect(formatInterval(90)).toBe("every 90s");
  });

  it("appends the stored custom interval so the select never lies", () => {
    expect(intervalOptions(24 * 3600)).toEqual(INTERVAL_PRESETS);
    const custom = intervalOptions(12 * 3600);
    expect(custom).toHaveLength(INTERVAL_PRESETS.length + 1);
    expect(custom[custom.length - 1]).toEqual({ label: "every 12h", seconds: 12 * 3600 });
  });
});

describe("summaryChips", () => {
  it("returns nothing for a never-generated report", () => {
    expect(summaryChips("inventory", null)).toEqual([]);
  });

  it("marks zero-consumer-with-pending as crit and dead candidates as warn on inventory", () => {
    const chips = summaryChips("inventory", {
      dead_queue_candidates: 41,
      zero_consumer_pending: 3,
      paused_over_7d: 0,
      queues_total: 1847,
    });
    expect(chips.find((c) => c.label.includes("zero-consumer"))!.tone).toBe("crit");
    expect(chips.find((c) => c.label.includes("dead-queue"))!.tone).toBe("warn");
    expect(chips.find((c) => c.label.includes("paused"))!.tone).toBe("mut");
  });

  it("formats storage memory as bytes and flags unsampled queues", () => {
    const chips = summaryChips("storage", {
      estimated_bytes: 2 * 1024 * 1024 * 1024,
      not_sampled_queues: 5,
      reclaim_24h: 12000,
      reclaim_past_due: 0,
    });
    expect(chips[0].formatted).toBe("2.0 GB");
    expect(chips.find((c) => c.label.includes("not sampled"))!.tone).toBe("warn");
  });

  it("marks SCHEDULER GONE as crit only when nonzero", () => {
    expect(summaryChips("scheduler-health", { gone: 1 })[0].tone).toBe("crit");
    expect(summaryChips("scheduler-health", { gone: 0 })[0].tone).toBe("mut");
  });

  it("flags the 10k archive cap on the dead-letter digest", () => {
    const chips = summaryChips("dead-letter", {
      archived_total: 84112,
      older_than_30d: 80000,
      trim_cap_queues: 2,
    });
    expect(chips.find((c) => c.label.includes("10k"))!.tone).toBe("warn");
  });
});

describe("honest formatting", () => {
  it("formats bytes with sensible units", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KB");
    expect(formatBytes(1024 * 1024 * 1024)).toBe("1.0 GB");
    expect(formatBytes(-1)).toBe("—");
  });

  it("spells out what each activity evidence source proves", () => {
    expect(activitySourceLabel("oldest_pending")).toContain("lower bound");
    expect(activitySourceLabel("none")).toContain("30d");
    expect(activitySourceLabel("unknown")).toContain("not probed");
  });
});

describe("nothingScheduled (§3.10 empty state)", () => {
  it("is true only when nothing is enabled AND nothing was ever generated", () => {
    expect(nothingScheduled([card({}), card({ kind: "storage" })])).toBe(true);
    expect(nothingScheduled([card({ enabled: true })])).toBe(false);
    expect(nothingScheduled([card({ last_generated_at: "2026-07-25T00:00:00Z" })])).toBe(false);
    expect(nothingScheduled([])).toBe(false);
  });
});
