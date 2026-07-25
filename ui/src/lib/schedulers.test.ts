import { describe, expect, it } from "vitest";
import {
  filterSchedulerRows,
  goneLabel,
  isGone,
  outcomeStamp,
  outcomeTone,
  parseGoDuration,
  queueFromOptions,
  RETENTION_GUIDANCE,
  retentionLabel,
  retentionSecondsFromOptions,
  showRetentionGuidance,
  sortSchedulerRows,
} from "./schedulers";
import { SchedulerRow } from "../api-fleet";

function row(overrides: Partial<SchedulerRow> & { type?: string; spec?: string }): SchedulerRow {
  const { type = "t", spec = "@every 1m", ...rest } = overrides;
  return {
    stable_key: `key-${type}-${spec}`,
    entry: {
      id: "eph",
      spec,
      task_type: type,
      task_payload: "{}",
      options: [],
      next_enqueue_at: "2026-07-25T12:00:00Z",
    },
    live: true,
    first_seen: "2026-07-01T00:00:00Z",
    last_seen: "2026-07-25T11:59:00Z",
    ...rest,
  };
}

describe("outcome chip mapping (§3.8: succeeded good / failed crit / unknown neutral)", () => {
  it("maps the three-valued outcomes plus pending onto tones", () => {
    expect(outcomeTone("succeeded")).toBe("good");
    expect(outcomeTone("failed")).toBe("crit");
    expect(outcomeTone("unknown")).toBe("mut");
    expect(outcomeTone("pending")).toBe("info");
  });

  it("degrades an unrecognized outcome to neutral, never a verdict", () => {
    expect(outcomeTone("something-new")).toBe("mut");
  });

  it("stamps found tasks with their state and unknown ones with the server reason", () => {
    expect(
      outcomeStamp({ task_id: "a", enqueued_at: "", outcome: "succeeded", state: "completed" })
    ).toBe("completed-state task found");
    expect(
      outcomeStamp({ task_id: "a", enqueued_at: "", outcome: "failed", state: "archived" })
    ).toBe("archived-state task found");
    expect(
      outcomeStamp({
        task_id: "a",
        enqueued_at: "",
        outcome: "unknown",
        reason: "not_found — task deleted or Retention=0",
      })
    ).toBe("not_found — task deleted or Retention=0");
  });
});

describe("GONE row logic (§3.8/§5.12)", () => {
  it("a row is GONE exactly when the server flagged gone_since", () => {
    expect(isGone(row({ live: false, gone_since: "2026-07-25T10:00:00Z" }))).toBe(true);
    expect(isGone(row({ live: true }))).toBe(false);
    expect(isGone(row({ live: false }))).toBe(false); // lagging, not yet GONE
  });

  it("renders the last-seen stamp for the chip", () => {
    const nowMs = Date.parse("2026-07-25T12:00:00Z");
    expect(goneLabel("2026-07-25T10:00:00Z", nowMs)).toBe("last seen 2h ago");
    expect(goneLabel(undefined, nowMs)).toBe("");
    expect(goneLabel("not-a-date", nowMs)).toBe("");
  });

  it("sorts GONE rows first, then alphabetically by type", () => {
    const rows = [
      row({ type: "b:live" }),
      row({ type: "z:gone", live: false, gone_since: "2026-07-25T10:00:00Z" }),
      row({ type: "a:live" }),
    ];
    expect(sortSchedulerRows(rows).map((r) => r.entry.task_type)).toEqual([
      "z:gone",
      "a:live",
      "b:live",
    ]);
  });
});

describe("filter by type/spec", () => {
  const rows = [row({ type: "report:nightly", spec: "0 2 * * *" }), row({ type: "cache:warm", spec: "@every 5m" })];

  it("matches case-insensitive substrings of type or spec", () => {
    expect(filterSchedulerRows(rows, "REPORT").map((r) => r.entry.task_type)).toEqual(["report:nightly"]);
    expect(filterSchedulerRows(rows, "@every").map((r) => r.entry.task_type)).toEqual(["cache:warm"]);
    expect(filterSchedulerRows(rows, "")).toEqual(rows);
    expect(filterSchedulerRows(rows, "zzz")).toEqual([]);
  });
});

describe("target queue from options (default handled — no regex-dropped links)", () => {
  it("parses Queue(...) with and without quotes", () => {
    expect(queueFromOptions(['Queue("critical")', "MaxRetry(5)"])).toBe("critical");
    expect(queueFromOptions(["Queue(reports)"])).toBe("reports");
  });

  it("defaults to asynq's default queue when the option is absent", () => {
    expect(queueFromOptions(["MaxRetry(5)"])).toBe("default");
    expect(queueFromOptions([])).toBe("default");
    expect(queueFromOptions(undefined)).toBe("default");
  });
});

describe("retention parsing (Go duration strings)", () => {
  it("parses Go's rendering into whole seconds", () => {
    expect(parseGoDuration("1h0m0s")).toBe(3600);
    expect(parseGoDuration("90s")).toBe(90);
    expect(parseGoDuration("1.5h")).toBe(5400);
    expect(parseGoDuration("500ms")).toBe(0);
    expect(parseGoDuration("0s")).toBe(0);
    expect(parseGoDuration("garbage")).toBeNull();
    expect(parseGoDuration("1h banana")).toBeNull();
  });

  it("extracts Retention(...) from the option strings, 0 when absent", () => {
    expect(retentionSecondsFromOptions(["Retention(1h0m0s)", 'Queue("sq")'])).toBe(3600);
    expect(retentionSecondsFromOptions(['Queue("sq")'])).toBe(0);
    expect(retentionSecondsFromOptions(undefined)).toBe(0);
  });

  it("labels the badge", () => {
    expect(retentionLabel(0)).toBe("Retention 0");
    expect(retentionLabel(3600)).toBe("Retention 1h");
  });
});

describe("Retention=0 guidance gating (§3.8, verbatim when unknowns exist)", () => {
  const unknown = { outcome: "unknown" as const };
  const succeeded = { outcome: "succeeded" as const };

  it("shows only when retention is 0 AND at least one outcome is unknown", () => {
    expect(showRetentionGuidance(0, [unknown, succeeded])).toBe(true);
    expect(showRetentionGuidance(0, [succeeded])).toBe(false);
    expect(showRetentionGuidance(3600, [unknown])).toBe(false);
    expect(showRetentionGuidance(0, [])).toBe(false);
    expect(showRetentionGuidance(0, undefined)).toBe(false);
  });

  it("carries the §3.8 wording verbatim", () => {
    expect(RETENTION_GUIDANCE).toBe(
      "Task no longer in Redis. This scheduler's tasks have Retention=0, so " +
        "successful completions are deleted immediately — success is unknowable. " +
        "Set asynq.Retention on the entry to make outcomes traceable."
    );
  });
});
