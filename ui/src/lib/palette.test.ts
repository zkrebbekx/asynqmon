import { describe, it, expect } from "vitest";
import { parseTaskIdInput, STATE_BULK_VERBS } from "./palette";

const UUID = "8f3a2b1c-4d5e-6f70-8192-a3b4c5d6e7f8";

describe("parseTaskIdInput", () => {
  it("detects a pasted task UUID", () => {
    expect(parseTaskIdInput(UUID)).toEqual({ id: UUID });
  });

  it("trims whitespace and normalizes case", () => {
    expect(parseTaskIdInput(`  ${UUID.toUpperCase()}  `)).toEqual({ id: UUID });
  });

  it("accepts the drawer's queue/uuid peek format, carrying the queue", () => {
    expect(parseTaskIdInput(`email:default/${UUID}`)).toEqual({
      id: UUID,
      queue: "email:default",
    });
    // Queue names may themselves contain slashes — the ID is after the LAST.
    expect(parseTaskIdInput(`team/email/${UUID}`)).toEqual({
      id: UUID,
      queue: "team/email",
    });
  });

  it("rejects partial UUIDs, free text, and queue-only strings", () => {
    expect(parseTaskIdInput("8f3a2b1c")).toBeNull();
    expect(parseTaskIdInput("gateway timeout")).toBeNull();
    expect(parseTaskIdInput("")).toBeNull();
    expect(parseTaskIdInput("email:default/")).toBeNull();
    expect(parseTaskIdInput(`${UUID}x`)).toBeNull();
    expect(parseTaskIdInput(`bad queue/${UUID}`)).toBeNull(); // spaces: not a peek ref
  });

  it("rejects non-hex uuid shapes", () => {
    expect(parseTaskIdInput("8f3a2b1c-4d5e-6f70-8192-zzzzzzzzzzzz")).toBeNull();
  });
});

describe("STATE_BULK_VERBS", () => {
  it("covers all seven states with their §3.4 capability rows", () => {
    expect(Object.keys(STATE_BULK_VERBS).sort()).toEqual([
      "active",
      "aggregating",
      "archived",
      "completed",
      "pending",
      "retry",
      "scheduled",
    ]);
    expect(STATE_BULK_VERBS.active).toEqual(["cancel"]);
    expect(STATE_BULK_VERBS.archived).toEqual(["run", "delete"]);
    expect(STATE_BULK_VERBS.completed).toEqual(["delete"]);
  });
});
