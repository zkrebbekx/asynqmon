import { describe, expect, it } from "vitest";
import {
  archiveTrimWarning,
  costEstimateLabel,
  gateExecute,
  isTerminalJobState,
  jobProgress,
  scopeLabel,
  typedConfirmPhrase,
  verifyVerdict,
} from "./bulkjob";

const baseGate = {
  verb: "archive" as const,
  previewComplete: true,
  candidates: 50,
  reason: "flatten gw storm",
  typedConfirm: "",
  proceedOnPartial: false,
};

describe("gateExecute (§4.3 execute gate)", () => {
  it("passes a completed-preview archive with a reason", () => {
    expect(gateExecute(baseGate)).toEqual({ ok: true, blockedBecause: "" });
  });

  it("always requires a reason", () => {
    const r = gateExecute({ ...baseGate, reason: "   " });
    expect(r.ok).toBe(false);
    expect(r.blockedBecause).toMatch(/reason/i);
  });

  it("blocks delete until the preview is complete, with no partial-ack override", () => {
    const r = gateExecute({
      ...baseGate,
      verb: "delete",
      previewComplete: false,
      proceedOnPartial: true, // ack must NOT unlock delete
    });
    expect(r.ok).toBe(false);
    expect(r.blockedBecause).toMatch(/completed preview/i);
  });

  it("blocks non-delete verbs on a partial preview until the ack is given", () => {
    const partial = { ...baseGate, previewComplete: false };
    expect(gateExecute(partial).ok).toBe(false);
    expect(gateExecute({ ...partial, proceedOnPartial: true }).ok).toBe(true);
  });

  it("requires the typed phrase for large deletes and accepts the exact phrase", () => {
    const big = {
      ...baseGate,
      verb: "delete" as const,
      candidates: 12400,
    };
    expect(gateExecute(big).ok).toBe(false);
    expect(gateExecute(big).blockedBecause).toContain("DELETE 12400");
    expect(gateExecute({ ...big, typedConfirm: "DELETE 12399" }).ok).toBe(false);
    expect(gateExecute({ ...big, typedConfirm: "DELETE 12400" }).ok).toBe(true);
  });

  it("does not demand a typed phrase for deletes at or under 1,000", () => {
    const small = { ...baseGate, verb: "delete" as const, candidates: 1000 };
    expect(gateExecute(small).ok).toBe(true);
  });
});

describe("typedConfirmPhrase", () => {
  it("only applies to delete above the 1,000 threshold", () => {
    expect(typedConfirmPhrase("delete", 1000)).toBeNull();
    expect(typedConfirmPhrase("delete", 1001)).toBe("DELETE 1001");
    expect(typedConfirmPhrase("archive", 50000)).toBeNull();
    expect(typedConfirmPhrase("run", 50000)).toBeNull();
  });
});

describe("scopeLabel", () => {
  it("renders queue, state, meta pairs, and quoted free text", () => {
    expect(
      scopeLabel({ queue: "billing", state: "retry", q: "gateway timeout", meta: ["region:eu"] })
    ).toBe('queue=billing state=retry region=eu q~"gateway timeout"');
  });

  it("defaults an empty queue to all and omits empty parts", () => {
    expect(scopeLabel({ state: "pending" })).toBe("queue=all state=pending");
    expect(scopeLabel({ queue: "", state: "pending", q: "", meta: [] })).toBe(
      "queue=all state=pending"
    );
  });

  it("keeps colons inside meta values", () => {
    expect(scopeLabel({ state: "pending", meta: ["url:https://x"] })).toBe(
      "queue=all state=pending url=https://x"
    );
  });
});

describe("archiveTrimWarning (§7.3 trim disclosure)", () => {
  it("warns when archiving more than the 10k cap", () => {
    expect(archiveTrimWarning("archive", 30000)).toContain("discard the oldest 20,000");
  });
  it("stays quiet at or under the cap, and for other verbs", () => {
    expect(archiveTrimWarning("archive", 10000)).toBeNull();
    expect(archiveTrimWarning("delete", 30000)).toBeNull();
  });
});

describe("costEstimateLabel (list_removal disclosure)", () => {
  it("scales matches × list length into human time", () => {
    expect(costEstimateLabel(10, 100)).toBe("under a second");
    expect(costEstimateLabel(10000, 48210)).toBe("~49s");
    expect(costEstimateLabel(12400, 1200000)).toBe("~25 min");
    expect(costEstimateLabel(500000, 1200000)).toBe("~16.7 h");
  });
});

describe("jobProgress", () => {
  it("is null before candidates are known and caps at 1", () => {
    expect(jobProgress({ candidates: 0, acted: 0, skipped: 0, failed: 0 })).toBeNull();
    expect(jobProgress({ candidates: 10, acted: 5, skipped: 2, failed: 1 })).toBeCloseTo(0.8);
    expect(jobProgress({ candidates: 4, acted: 4, skipped: 1, failed: 0 })).toBe(1);
  });
});

describe("isTerminalJobState", () => {
  it("matches the backend lifecycle", () => {
    for (const s of ["done", "canceled", "failed"]) expect(isTerminalJobState(s)).toBe(true);
    for (const s of ["previewing", "preview_ready", "running", "paused"])
      expect(isTerminalJobState(s)).toBe(false);
  });
});

describe("verifyVerdict (§3.9 verify panel)", () => {
  const scope = { queue: "bulk", state: "retry", q: "", meta: [] };
  const counts = { candidates: 100, acted: 97, skipped: 3, failed: 0 };

  it("declares a drained scope in plain language", () => {
    expect(verifyVerdict(0, false, scope, counts)).toContain("matches 0 tasks — drained");
  });

  it("reports a live remainder against the job's final counts", () => {
    const v = verifyVerdict(12, false, scope, counts);
    expect(v).toContain("still matches 12 task(s)");
    expect(v).toContain("97 acted");
  });

  it("marks truncated live counts as a lower bound", () => {
    expect(verifyVerdict(5000, true, scope, counts)).toContain("5000+");
  });
});
