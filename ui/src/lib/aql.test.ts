import { describe, it, expect } from "vitest";
import {
  tokenizeAql,
  parseAqlClauses,
  isAqlQuery,
  quoteAqlValue,
  getClause,
  setClause,
  removeClause,
  metaClausesOf,
  hasMetaClause,
  addMetaClause,
  removeMetaClause,
  aqlFromLegacy,
  isAqlRejection,
  caretLine,
} from "./aql";

describe("tokenizeAql", () => {
  it("splits on whitespace outside quotes with positions", () => {
    const toks = tokenizeAql('queue=email error~"gateway timeout"');
    expect(toks).toEqual([
      { text: "queue=email", pos: 0 },
      { text: 'error~"gateway timeout"', pos: 12 },
    ]);
  });

  it("handles empty input", () => {
    expect(tokenizeAql("")).toEqual([]);
    expect(tokenizeAql("   ")).toEqual([]);
  });
});

describe("parseAqlClauses", () => {
  it("parses field/op/value with unquoting", () => {
    const cs = parseAqlClauses('queue=email retries>=3 error~"gateway timeout" past_due bare');
    expect(cs.map((c) => [c.field, c.op, c.value])).toEqual([
      ["queue", "=", "email"],
      ["retries", ">=", "3"],
      ["error", "~", "gateway timeout"],
      ["past_due", "", ""],
      ["", "", "bare"],
    ]);
  });

  it("never throws on malformed input (backend owns validation)", () => {
    expect(() => parseAqlClauses('=weird "unterminated')).not.toThrow();
  });
});

describe("isAqlQuery", () => {
  it("detects clause-shaped and flag tokens", () => {
    expect(isAqlQuery("queue=email")).toBe(true);
    expect(isAqlQuery("past_due")).toBe(true);
    expect(isAqlQuery("orphaned")).toBe(true);
    expect(isAqlQuery("meta.region=eu")).toBe(true);
  });

  it("treats plain free text as legacy (substring semantics preserved)", () => {
    expect(isAqlQuery("gateway timeout")).toBe(false);
    expect(isAqlQuery("email:send")).toBe(false);
    expect(isAqlQuery("")).toBe(false);
  });
});

describe("clause editing (chips COMPOSE INTO the text)", () => {
  it("setClause appends when absent", () => {
    expect(setClause("queue=email", "state", "=", "retry")).toBe("queue=email state=retry");
  });

  it("setClause replaces in place when present", () => {
    expect(setClause("state=retry error~boom", "state", "=", "archived")).toBe(
      "state=archived error~boom"
    );
  });

  it("setClause quotes values with spaces", () => {
    expect(setClause("", "type", "=", "a b")).toBe('type="a b"');
  });

  it("setClause with empty value removes the clause", () => {
    expect(setClause("queue=email state=retry", "queue", "=", "")).toBe("state=retry");
  });

  it("removeClause drops every occurrence of the field", () => {
    expect(removeClause("queue=a state=retry queue=b", "queue")).toBe("state=retry");
  });

  it("getClause finds the first occurrence", () => {
    expect(getClause("queue=email state=retry", "state")?.value).toBe("retry");
    expect(getClause("queue=email", "state")).toBeUndefined();
  });

  it("round-trips: add a state pill, add a chip, remove both — text unchanged", () => {
    const start = 'queue=email error~"gateway timeout"';
    let q = setClause(start, "state", "=", "retry");
    q = addMetaClause(q, "region", "eu");
    expect(q).toBe('queue=email error~"gateway timeout" state=retry meta.region=eu');
    q = removeMetaClause(q, "region", "eu");
    q = removeClause(q, "state");
    expect(q).toBe(start);
  });
});

describe("meta chips ⇄ meta.* clauses", () => {
  it("metaClausesOf extracts key/value pairs", () => {
    expect(metaClausesOf('meta.region=eu meta.note="a b" queue=x')).toEqual([
      { key: "region", value: "eu" },
      { key: "note", value: "a b" },
    ]);
  });

  it("addMetaClause is idempotent per key=value", () => {
    const q = addMetaClause("queue=x", "region", "eu");
    expect(addMetaClause(q, "region", "eu")).toBe(q);
  });

  it("hasMetaClause matches exact key and value", () => {
    expect(hasMetaClause("meta.region=eu", "region", "eu")).toBe(true);
    expect(hasMetaClause("meta.region=eu", "region", "us")).toBe(false);
  });

  it("removeMetaClause removes only the matching pair", () => {
    expect(removeMetaClause("meta.region=eu meta.region=us", "region", "eu")).toBe(
      "meta.region=us"
    );
  });

  it("quotes chip values containing spaces", () => {
    expect(addMetaClause("", "note", "hello world")).toBe('meta.note="hello world"');
  });
});

describe("aqlFromLegacy (URL migration)", () => {
  it("prepends queue/state/meta as clauses", () => {
    expect(
      aqlFromLegacy({
        queue: "billing",
        state: "retry",
        meta: [{ key: "region", value: "eu" }],
        freeText: "email",
      })
    ).toBe("queue=billing state=retry meta.region=eu email");
  });

  it("skips queue=all (the default)", () => {
    expect(aqlFromLegacy({ queue: "all", state: "retry" })).toBe("state=retry");
  });

  it("keeps standalone free text verbatim (legacy substring semantics)", () => {
    expect(aqlFromLegacy({ freeText: "gateway timeout" })).toBe("gateway timeout");
  });

  it("quotes multi-word free text when combined with clauses", () => {
    expect(aqlFromLegacy({ state: "retry", freeText: "gateway timeout" })).toBe(
      'state=retry "gateway timeout"'
    );
  });
});

describe("rejection rendering ({error, position, hint})", () => {
  it("isAqlRejection recognizes the structured 400 body", () => {
    expect(isAqlRejection({ error: "asynq stores no enqueue timestamp", position: 0 })).toBe(true);
    expect(isAqlRejection({ error: "nope" })).toBe(false);
    expect(isAqlRejection("string body")).toBe(false);
    expect(isAqlRejection(null)).toBe(false);
  });

  it("caretLine puts the caret under the offending clause", () => {
    expect(caretLine("queue=email enqueued<-2h", 12)).toBe("            ^");
  });

  it("caretLine clamps out-of-range positions", () => {
    expect(caretLine("ab", 99)).toBe("  ^");
    expect(caretLine("ab", -5)).toBe("^");
  });
});

describe("quoteAqlValue", () => {
  it("quotes only when needed", () => {
    expect(quoteAqlValue("plain")).toBe("plain");
    expect(quoteAqlValue("two words")).toBe('"two words"');
  });
});
