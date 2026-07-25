import { describe, it, expect } from "vitest";
import {
  humanizeMs,
  ageTone,
  isStale,
  STALE_ROW_THRESHOLD_MS,
  formatLatencyMs,
  formatErrorRate,
  attentionTarget,
  severityTone,
  formatFindingValue,
} from "./fleet";

describe("humanizeMs", () => {
  it("renders — for absent or non-positive values", () => {
    expect(humanizeMs(undefined)).toBe("—");
    expect(humanizeMs(null)).toBe("—");
    expect(humanizeMs(0)).toBe("—");
    expect(humanizeMs(-5)).toBe("—");
    expect(humanizeMs(NaN)).toBe("—");
  });

  it("renders sub-second as <1s and seconds plainly", () => {
    expect(humanizeMs(500)).toBe("<1s");
    expect(humanizeMs(59_000)).toBe("59s");
  });

  it("renders two units max, largest first (mockup style)", () => {
    expect(humanizeMs(60_000)).toBe("1m");
    expect(humanizeMs(42 * 60_000 + 18_000)).toBe("42m 18s");
    expect(humanizeMs(3_600_000)).toBe("1h");
    expect(humanizeMs(3_600_000 + 58 * 60_000)).toBe("1h 58m");
    expect(humanizeMs(3 * 86_400_000 + 4 * 3_600_000)).toBe("3d 4h");
  });
});

describe("ageTone", () => {
  it("is quiet below the 5m SLO", () => {
    expect(ageTone(0)).toBeNull();
    expect(ageTone(4 * 60_000)).toBeNull();
    expect(ageTone(undefined)).toBeNull();
  });

  it("warns at the 5m SLO and goes crit at 30m", () => {
    expect(ageTone(5 * 60_000)).toBe("warn");
    expect(ageTone(29 * 60_000)).toBe("warn");
    expect(ageTone(30 * 60_000)).toBe("crit");
    expect(ageTone(2 * 3_600_000)).toBe("crit");
  });
});

describe("isStale (staleness dot)", () => {
  const now = Date.parse("2026-07-25T12:00:00Z");

  it("is fresh within the hot-tier threshold", () => {
    expect(isStale("2026-07-25T11:59:50Z", now)).toBe(false); // 10s old
    expect(isStale("2026-07-25T12:00:00Z", now)).toBe(false);
  });

  it("is stale beyond the threshold", () => {
    expect(isStale("2026-07-25T11:59:44Z", now)).toBe(true); // 16s old
    expect(isStale("2026-07-25T11:57:00Z", now)).toBe(true); // 3m old
  });

  it("sits exactly at the boundary without flapping fresh", () => {
    const at = new Date(now - STALE_ROW_THRESHOLD_MS).toISOString();
    expect(isStale(at, now)).toBe(false); // > threshold, not >=
  });

  it("treats missing or unparsable timestamps as stale, never fresh", () => {
    expect(isStale(undefined, now)).toBe(true);
    expect(isStale(null, now)).toBe(true);
    expect(isStale("", now)).toBe(true);
    expect(isStale("not-a-time", now)).toBe(true);
  });

  it("honors a custom threshold", () => {
    expect(isStale("2026-07-25T11:59:50Z", now, 5_000)).toBe(true);
    expect(isStale("2026-07-25T11:59:58Z", now, 5_000)).toBe(false);
  });
});

describe("formatLatencyMs", () => {
  it("keeps sub-second values in milliseconds", () => {
    expect(formatLatencyMs(0)).toBe("0ms");
    expect(formatLatencyMs(340)).toBe("340ms");
  });

  it("humanizes at a second and beyond", () => {
    expect(formatLatencyMs(1_500)).toBe("1s");
    expect(formatLatencyMs(64_000)).toBe("1m 4s");
  });

  it("renders — for absent values", () => {
    expect(formatLatencyMs(undefined)).toBe("—");
    expect(formatLatencyMs(-1)).toBe("—");
  });
});

describe("formatErrorRate", () => {
  it("renders a fraction as a percentage", () => {
    expect(formatErrorRate(0.024)).toBe("2.4%");
    expect(formatErrorRate(0)).toBe("0.0%");
    expect(formatErrorRate(1)).toBe("100.0%");
  });

  it("treats values above 1 as already-percent (contract-drift guard)", () => {
    expect(formatErrorRate(2.4)).toBe("2.4%");
  });

  it("renders — for absent values", () => {
    expect(formatErrorRate(undefined)).toBe("—");
    expect(formatErrorRate(NaN)).toBe("—");
  });
});

describe("severityTone", () => {
  it("maps the 1–5 attention scale onto three tones", () => {
    expect(severityTone(5)).toBe("crit"); // work is not running
    expect(severityTone(4)).toBe("warn");
    expect(severityTone(3)).toBe("warn");
    expect(severityTone(2)).toBe("info");
    expect(severityTone(1)).toBe("info"); // hygiene
  });
});

describe("formatFindingValue", () => {
  it("renders age-detector values (whole seconds) as durations", () => {
    expect(formatFindingValue("PENDING_AGE", 1318)).toBe("21m 58s");
    expect(formatFindingValue("PAUSED_LONG", 86_400 * 21)).toBe("21d");
    expect(formatFindingValue("GROUP_STALL", 95)).toBe("1m 35s");
  });

  it("renders count-detector values as formatted counts", () => {
    expect(formatFindingValue("NO_CONSUMERS", 48210)).toBe("48,210");
    expect(formatFindingValue("ORPHANS", 14)).toBe("14");
  });

  it("renders — for non-finite values", () => {
    expect(formatFindingValue("ORPHANS", NaN)).toBe("—");
  });
});

describe("attentionTarget (suggested_query → URL wiring)", () => {
  it("routes task-scoped queries to the Tasks console with queue and state", () => {
    const t = attentionTarget("queue=search:reindex state=pending");
    expect(t.kind).toBe("tasks");
    const params = new URLSearchParams(t.search);
    expect(params.get("queue")).toBe("search:reindex");
    // pending is the console default, so it's omitted from the URL.
    expect(params.get("state")).toBeNull();
  });

  it("carries a non-default state and maps error~ into free text", () => {
    const t = attentionTarget('queue=billing:invoice state=retry error~"gateway timeout"');
    expect(t.kind).toBe("tasks");
    const params = new URLSearchParams(t.search);
    expect(params.get("queue")).toBe("billing:invoice");
    expect(params.get("state")).toBe("retry");
    expect(params.get("q")).toBe("gateway timeout");
  });

  it("routes queue-set predicates to the Queues Directory verbatim", () => {
    const t = attentionTarget("consumers=0 pending>10000");
    expect(t.kind).toBe("queues");
    const params = new URLSearchParams(t.search);
    expect(params.get("f")).toBe("consumers=0 pending>10000");
  });

  it("keeps bare-word predicates on the directory (no console mapping)", () => {
    const t = attentionTarget("paused>7d");
    expect(t.kind).toBe("queues");
    expect(new URLSearchParams(t.search).get("f")).toBe("paused>7d");
  });

  it("degrades an unknown state value to the console default instead of crashing", () => {
    const t = attentionTarget("state=bogus queue=email");
    expect(t.kind).toBe("tasks");
    const params = new URLSearchParams(t.search);
    expect(params.get("state")).toBeNull(); // default (pending) omitted
    expect(params.get("queue")).toBe("email");
  });

  it("handles empty input", () => {
    expect(attentionTarget("")).toEqual({ kind: "queues", search: "" });
  });
});
