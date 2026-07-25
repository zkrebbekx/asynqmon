import { describe, it, expect } from "vitest";
import {
  DEFAULT_CORRELATION_KEYS,
  distinctQueues,
  extractCorrelation,
  mergeFlowTasks,
  observedAt,
} from "./correlation";

describe("extractCorrelation", () => {
  it("finds a trace_id in a JSON payload", () => {
    expect(extractCorrelation('{"invoice_id":"inv_1","trace_id":"tr-7f3e9a"}')).toEqual({
      key: "trace_id",
      value: "tr-7f3e9a",
      nested: false,
    });
  });

  it("recognizes correlation_id and request_id", () => {
    expect(extractCorrelation('{"correlation_id":"c-1"}')).toEqual({
      key: "correlation_id",
      value: "c-1",
      nested: false,
    });
    expect(extractCorrelation('{"request_id":"r-1"}')).toEqual({
      key: "request_id",
      value: "r-1",
      nested: false,
    });
  });

  it("prefers trace_id over the other keys when several are present", () => {
    const payload = '{"request_id":"r-1","trace_id":"t-1","correlation_id":"c-1"}';
    expect(extractCorrelation(payload)).toEqual({ key: "trace_id", value: "t-1", nested: false });
  });

  it("stringifies numeric ids like the metadata chips do", () => {
    expect(extractCorrelation('{"request_id":12345}')).toEqual({
      key: "request_id",
      value: "12345",
      nested: false,
    });
  });

  it("ignores empty values", () => {
    expect(extractCorrelation('{"trace_id":""}')).toBeNull();
  });

  it("returns null for non-JSON payloads", () => {
    expect(extractCorrelation("plain text")).toBeNull();
    expect(extractCorrelation("")).toBeNull();
  });

  describe("configurable key list", () => {
    it("honors a custom list from the deployment", () => {
      const payload = '{"order_ref":"ord-9","trace_id":"t-1"}';
      expect(extractCorrelation(payload, ["order_ref"])).toEqual({
        key: "order_ref",
        value: "ord-9",
        nested: false,
      });
    });

    it("ignores default keys not present in the configured list", () => {
      expect(extractCorrelation('{"trace_id":"t-1"}', ["order_ref"])).toBeNull();
    });

    it("respects the configured priority order, not payload order", () => {
      const payload = '{"trace_id":"t-1","order_ref":"ord-9"}';
      expect(extractCorrelation(payload, ["order_ref", "trace_id"])).toEqual({
        key: "order_ref",
        value: "ord-9",
        nested: false,
      });
    });

    it("defaults to DEFAULT_CORRELATION_KEYS when no list is passed", () => {
      expect(DEFAULT_CORRELATION_KEYS).toEqual(["trace_id", "correlation_id", "request_id"]);
    });
  });

  describe("nested payloads (one level deep)", () => {
    it("finds a correlation id one level deep and marks it nested", () => {
      expect(extractCorrelation('{"meta":{"trace_id":"t-nested"}}')).toEqual({
        key: "trace_id",
        value: "t-nested",
        nested: true,
      });
    });

    it("prefers any top-level hit over a nested one", () => {
      const payload = '{"request_id":"r-top","meta":{"trace_id":"t-nested"}}';
      expect(extractCorrelation(payload)).toEqual({
        key: "request_id",
        value: "r-top",
        nested: false,
      });
    });

    it("does not descend two levels deep", () => {
      expect(extractCorrelation('{"outer":{"inner":{"trace_id":"t-deep"}}}')).toBeNull();
    });

    it("ignores nested objects, arrays, and empty nested values", () => {
      expect(extractCorrelation('{"trace_id":{"nested":true}}')).toBeNull();
      expect(extractCorrelation('{"meta":["trace_id"]}')).toBeNull();
      expect(extractCorrelation('{"meta":{"trace_id":""}}')).toBeNull();
    });
  });
});

// ----------------------------------------------------------------------------
// Flow-tab merge helpers
// ----------------------------------------------------------------------------

const t = (id: string, queue: string, completed_at = "", last_failed_at = "") => ({
  id,
  queue,
  completed_at,
  last_failed_at,
});

describe("mergeFlowTasks", () => {
  it("dedupes a task appearing in several per-state result lists by id", () => {
    const merged = mergeFlowTasks([
      [t("a", "q1", "2026-07-25T10:00:00Z")],
      [t("a", "q1", "2026-07-25T10:00:00Z"), t("b", "q2", "2026-07-25T11:00:00Z")],
    ]);
    expect(merged.map((x) => x.id)).toEqual(["a", "b"]);
  });

  it("keeps the first occurrence when a duplicate id carries different data", () => {
    const merged = mergeFlowTasks([
      [t("a", "q1", "2026-07-25T10:00:00Z")],
      [{ ...t("a", "q1"), last_failed_at: "2026-07-25T09:00:00Z" }],
    ]);
    expect(merged).toHaveLength(1);
    expect(merged[0].completed_at).toBe("2026-07-25T10:00:00Z");
  });

  it("sorts by observed time ascending", () => {
    const merged = mergeFlowTasks([
      [t("late", "q", "2026-07-25T12:00:00Z")],
      [t("early", "q", "2026-07-25T09:00:00Z")],
      [t("mid", "q", "", "2026-07-25T10:30:00Z")],
    ]);
    expect(merged.map((x) => x.id)).toEqual(["early", "mid", "late"]);
  });

  it("breaks timestamp ties by id so the order is stable across polls", () => {
    const ts = "2026-07-25T10:00:00Z";
    const merged = mergeFlowTasks([[t("bbb", "q", ts)], [t("aaa", "q", ts)]]);
    expect(merged.map((x) => x.id)).toEqual(["aaa", "bbb"]);
    // Same input in the other arrival order yields the same result.
    const swapped = mergeFlowTasks([[t("aaa", "q", ts)], [t("bbb", "q", ts)]]);
    expect(swapped.map((x) => x.id)).toEqual(["aaa", "bbb"]);
  });

  it("sorts tasks without an observed timestamp last", () => {
    const merged = mergeFlowTasks([
      [t("unseen", "q")],
      [t("zero", "q", "0001-01-01T00:00:00Z")],
      [t("seen", "q", "2026-07-25T10:00:00Z")],
    ]);
    expect(merged.map((x) => x.id)).toEqual(["seen", "unseen", "zero"]);
  });
});

describe("observedAt", () => {
  it("prefers completed_at, falls back to last_failed_at, treats zero time as unseen", () => {
    expect(observedAt(t("a", "q", "2026-07-25T10:00:00Z", "2026-07-25T09:00:00Z"))).toBe(
      "2026-07-25T10:00:00Z"
    );
    expect(observedAt(t("a", "q", "", "2026-07-25T09:00:00Z"))).toBe("2026-07-25T09:00:00Z");
    expect(observedAt(t("a", "q", "0001-01-01T00:00:00Z"))).toBe("");
    expect(observedAt(t("a", "q"))).toBe("");
  });
});

describe("distinctQueues", () => {
  it("counts the distinct queues a flow spans", () => {
    expect(distinctQueues([t("a", "emails"), t("b", "emails"), t("c", "billing")])).toBe(2);
    expect(distinctQueues([])).toBe(0);
  });
});
