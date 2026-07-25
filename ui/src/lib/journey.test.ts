import { describe, it, expect } from "vitest";
import { deriveJourney, journeyEdges, JourneyInput } from "./journey";

// Minimal input builder — the derivation only reads this slice of TaskInfo.
function input(overrides: Partial<JourneyInput>): JourneyInput {
  return { state: "pending", retried: 0, group: "", ...overrides };
}

describe("deriveJourney group handling", () => {
  it("treats an omitted group field (API leaves it out) as ungrouped", () => {
    const j = deriveJourney({ state: "retry", retried: 1, group: undefined });
    expect(j.viaGroup).toBe(false);
    expect(j.traversed.has("aggregating")).toBe(false);
  });
});

describe("deriveJourney", () => {
  it("marks only pending for a fresh pending task", () => {
    const j = deriveJourney(input({ state: "pending" }));
    expect(j.current).toBe("pending");
    expect([...j.traversed]).toEqual(["pending"]);
    expect(j.retryAttempts).toBe(0);
    expect(j.viaGroup).toBe(false);
  });

  it("marks only scheduled for a scheduled task (no fabricated pending)", () => {
    const j = deriveJourney(input({ state: "scheduled" }));
    expect(j.current).toBe("scheduled");
    expect([...j.traversed]).toEqual(["scheduled"]);
  });

  it("marks aggregating (not pending) for a grouped task still in its group", () => {
    const j = deriveJourney(input({ state: "aggregating", group: "notify" }));
    expect(j.current).toBe("aggregating");
    expect(j.viaGroup).toBe(true);
    expect(j.traversed.has("aggregating")).toBe(true);
    expect(j.traversed.has("pending")).toBe(false);
  });

  it("proves pending+active for an active task", () => {
    const j = deriveJourney(input({ state: "active" }));
    expect(j.traversed.has("pending")).toBe(true);
    expect(j.traversed.has("active")).toBe(true);
    expect(j.traversed.has("retry")).toBe(false);
  });

  it("marks the retry loop for a retry-state task", () => {
    const j = deriveJourney(input({ state: "retry", retried: 14, last_failed_at: "2026-07-25T10:00:00Z" }));
    expect(j.current).toBe("retry");
    expect(j.retryAttempts).toBe(14);
    for (const s of ["pending", "active", "retry"] as const) {
      expect(j.traversed.has(s)).toBe(true);
    }
  });

  it("marks retry traversal from retried>0 even when currently active", () => {
    const j = deriveJourney(input({ state: "active", retried: 3 }));
    expect(j.traversed.has("retry")).toBe(true);
  });

  it("proves the full path for a completed task", () => {
    const j = deriveJourney(input({ state: "completed", completed_at: "2026-07-25T10:00:00Z" }));
    for (const s of ["pending", "active", "completed"] as const) {
      expect(j.traversed.has(s)).toBe(true);
    }
    expect(j.traversed.has("retry")).toBe(false);
  });

  it("keeps a manually archived never-run task honest: archived only", () => {
    const j = deriveJourney(input({ state: "archived", retried: 0 }));
    expect([...j.traversed]).toEqual(["archived"]);
  });

  it("proves pending/active/retry for an archived task that exhausted retries", () => {
    const j = deriveJourney(
      input({ state: "archived", retried: 25, last_failed_at: "2026-07-25T09:00:00Z" })
    );
    for (const s of ["pending", "active", "retry", "archived"] as const) {
      expect(j.traversed.has(s)).toBe(true);
    }
  });

  it("treats Go zero timestamps as absent", () => {
    const j = deriveJourney(
      input({ state: "archived", last_failed_at: "0001-01-01T00:00:00Z" })
    );
    expect(j.traversed.has("active")).toBe(false);
  });

  it("returns null current for an unrecognized state string", () => {
    const j = deriveJourney(input({ state: "wat" }));
    expect(j.current).toBeNull();
  });
});

describe("journeyEdges", () => {
  it("uses the direct pending→active arc for ungrouped tasks", () => {
    const edges = journeyEdges(deriveJourney(input({ state: "active" })));
    expect(edges.has("pending→active")).toBe(true);
    expect(edges.has("pending→aggregating")).toBe(false);
    expect(edges.has("aggregating→active")).toBe(false);
  });

  it("routes through aggregating for grouped tasks that ran", () => {
    const edges = journeyEdges(
      deriveJourney(input({ state: "completed", group: "notify", completed_at: "2026-07-25T10:00:00Z" }))
    );
    expect(edges.has("pending→aggregating")).toBe(true);
    expect(edges.has("aggregating→active")).toBe(true);
    expect(edges.has("pending→active")).toBe(false);
    expect(edges.has("active→completed")).toBe(true);
  });

  it("closes the retry loop for a retry-state task", () => {
    const edges = journeyEdges(deriveJourney(input({ state: "retry", retried: 2 })));
    expect(edges.has("active→retry")).toBe(true);
    expect(edges.has("retry→pending")).toBe(true);
  });

  it("draws retry→archived only when retries were recorded", () => {
    const exhausted = journeyEdges(
      deriveJourney(input({ state: "archived", retried: 25, last_failed_at: "2026-07-25T09:00:00Z" }))
    );
    expect(exhausted.has("retry→archived")).toBe(true);

    const manual = journeyEdges(deriveJourney(input({ state: "archived", retried: 0 })));
    expect(manual.size).toBe(0); // no path is drawn when none is provable
  });

  it("draws no edges for a task still in its group", () => {
    const edges = journeyEdges(deriveJourney(input({ state: "aggregating", group: "g" })));
    expect(edges.size).toBe(0);
  });
});
