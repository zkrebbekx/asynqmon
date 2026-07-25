import { describe, it, expect } from "vitest";
import { fuzzyMatch, fuzzyFilter } from "./fuzzy";

describe("fuzzyMatch", () => {
  it("matches an in-order subsequence, case-insensitively", () => {
    const m = fuzzyMatch("bil", "Billing:invoice");
    expect(m).not.toBeNull();
    expect(m!.indices).toEqual([0, 1, 2]);
  });

  it("returns null when the query is not a subsequence", () => {
    expect(fuzzyMatch("xyz", "billing:invoice")).toBeNull();
    expect(fuzzyMatch("lib", "bil")).toBeNull(); // order matters
  });

  it("matches everything on an empty query with score 0", () => {
    expect(fuzzyMatch("", "anything")).toEqual({ score: 0, indices: [] });
  });

  it("scores consecutive runs above scattered hits", () => {
    const consecutive = fuzzyMatch("inv", "billing:invoice")!;
    const scattered = fuzzyMatch("inv", "identity-navigator")!;
    expect(consecutive.score).toBeGreaterThan(scattered.score);
  });

  it("scores early matches above late matches", () => {
    const early = fuzzyMatch("bil", "billing:ledger")!;
    const late = fuzzyMatch("bil", "global:billing")!;
    expect(early.score).toBeGreaterThan(late.score);
  });

  it("rewards word-boundary hits after separators", () => {
    // "inv" at a ":" boundary vs buried mid-word.
    const boundary = fuzzyMatch("inv", "b:invoice")!;
    const buried = fuzzyMatch("inv", "beinvoice")!;
    expect(boundary.score).toBeGreaterThan(buried.score);
  });

  it("reports the matched indices for highlight rendering", () => {
    const m = fuzzyMatch("bnv", "billing:invoice")!;
    expect(m.indices).toEqual([0, 5, 10]); // b … n … v
  });

  it("anchors on the best-aligned run, not the first char hit", () => {
    // The consecutive "inv" run in "invoice" must win over the earlier
    // scattered i…n…v — and beat a scattered match in another string.
    const m = fuzzyMatch("inv", "billing:invoice")!;
    expect(m.indices).toEqual([8, 9, 10]);
  });
});

describe("fuzzyFilter", () => {
  const queues = ["email:default", "billing:invoice", "billing:ledger", "audit"];

  it("drops non-matches, keeping only subsequence hits", () => {
    const out = fuzzyFilter("bil", queues, (q) => q);
    expect(out.map((r) => r.item).sort()).toEqual(["billing:invoice", "billing:ledger"]);
  });

  it("sorts by score descending", () => {
    const out = fuzzyFilter("led", queues, (q) => q);
    expect(out[0].item).toBe("billing:ledger"); // boundary run beats scatter
  });

  it("keeps input order on ties (stable sort)", () => {
    const out = fuzzyFilter("", queues, (q) => q);
    expect(out.map((r) => r.item)).toEqual(queues);
  });

  it("caps results at the limit", () => {
    const out = fuzzyFilter("i", queues, (q) => q, 2);
    expect(out).toHaveLength(2);
  });

  it("matches through a key function on objects", () => {
    const items = [{ name: "email:default" }, { name: "billing:invoice" }];
    const out = fuzzyFilter("email", items, (i) => i.name);
    expect(out).toHaveLength(1);
    expect(out[0].item.name).toBe("email:default");
  });
});
