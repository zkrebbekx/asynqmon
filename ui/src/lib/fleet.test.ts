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
  it("routes single queue-state anomalies to the Queue Workspace pre-focused on the state (§3.3)", () => {
    const t = attentionTarget("queue=search:reindex state=pending");
    expect(t.kind).toBe("workspace");
    expect(t.queue).toBe("search:reindex");
    expect(new URLSearchParams(t.search).get("focus")).toBe("pending");
  });

  it("routes detector queries with workspace-representable extras to the workspace", () => {
    // PENDING_AGE: the age clause is what the Attention tab itself shows.
    const age = attentionTarget("state=pending queue=media:transcode pending_age>5m");
    expect(age.kind).toBe("workspace");
    expect(age.queue).toBe("media:transcode");
    expect(new URLSearchParams(age.search).get("focus")).toBe("pending");

    // ORPHANS: the bare `orphaned` flag is covered by the active section.
    const orphans = attentionTarget("state=active queue=media:transcode orphaned");
    expect(orphans.kind).toBe("workspace");
    expect(new URLSearchParams(orphans.search).get("focus")).toBe("active");

    // RETRY_STORM: next_run< is the retry section's own sort.
    const storm = attentionTarget("state=retry queue=media:transcode next_run<5m");
    expect(storm.kind).toBe("workspace");
    expect(new URLSearchParams(storm.search).get("focus")).toBe("retry");

    // GROUP_STALL: group= names a section of the aggregating state.
    const stall = attentionTarget("state=aggregating queue=media:transcode group=g1");
    expect(stall.kind).toBe("workspace");
    expect(new URLSearchParams(stall.search).get("focus")).toBe("aggregating");
  });

  it("keeps state-only (fleet-wide) queries on the Tasks console verbatim", () => {
    const t = attentionTarget("state=pending pending_age>5m");
    expect(t.kind).toBe("tasks");
    const params = new URLSearchParams(t.search);
    expect(params.get("q")).toBe("state=pending pending_age>5m");
    expect(params.get("queue")).toBeNull();
    expect(params.get("state")).toBeNull();
  });

  it("keeps every clause — error~ and friends are no longer dropped or downgraded", () => {
    const raw = 'queue=billing:invoice state=retry error~"gateway timeout"';
    const t = attentionTarget(raw);
    expect(t.kind).toBe("tasks");
    const params = new URLSearchParams(t.search);
    expect(params.get("q")).toBe(raw);
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

  it("passes unknown state values through — the console's parser owns the rejection", () => {
    const t = attentionTarget("state=bogus queue=email");
    expect(t.kind).toBe("tasks");
    const params = new URLSearchParams(t.search);
    expect(params.get("q")).toBe("state=bogus queue=email");
  });

  it("handles empty input", () => {
    expect(attentionTarget("")).toEqual({ kind: "queues", search: "" });
  });

  it("routes scheduler-scoped queries to the Schedulers screen with the filter prefilled", () => {
    const t = attentionTarget("schedulers type=report:nightly");
    expect(t.kind).toBe("schedulers");
    expect(new URLSearchParams(t.search).get("filter")).toBe("report:nightly");
  });

  it("routes a bare schedulers query without inventing a filter", () => {
    expect(attentionTarget("schedulers")).toEqual({ kind: "schedulers", search: "" });
  });
});

describe("formatFindingValue for SCHEDULER_GONE", () => {
  it("renders the value as an age, not a count", () => {
    expect(formatFindingValue("SCHEDULER_GONE", 7200)).toBe("2h");
  });
});
