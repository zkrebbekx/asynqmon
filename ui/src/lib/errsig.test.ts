import { describe, expect, it } from "vitest";
import {
  blastLabel,
  formatSigCount,
  sigErrorClause,
  sigLiteralPrefix,
  sigScopeAql,
  sigTasksSearch,
  sigVerbsForState,
  templateSegments,
  trendTone,
} from "./errsig";

describe("trendTone (trend chip mapping)", () => {
  it("maps growing to crit — the incident tone", () => {
    expect(trendTone("growing")).toBe("crit");
  });
  it("maps new to warn", () => {
    expect(trendTone("new")).toBe("warn");
  });
  it("maps recovering to good", () => {
    expect(trendTone("recovering")).toBe("good");
  });
  it("maps steady to muted", () => {
    expect(trendTone("steady")).toBe("mut");
  });
  it("degrades unknown future trends to muted, never crashing", () => {
    expect(trendTone("volatile")).toBe("mut");
    expect(trendTone("")).toBe("mut");
  });
});

describe("formatSigCount (lower-bound rendering)", () => {
  it("renders exact counts plainly", () => {
    expect(formatSigCount(1203, false)).toBe("1,203");
  });
  it("prefixes ≥ when any source queue is archive-trimmed", () => {
    expect(formatSigCount(9634, true)).toBe("≥9,634");
  });
  it("renders zero honestly", () => {
    expect(formatSigCount(0, false)).toBe("0");
  });
  it("degrades non-finite counts to a dash", () => {
    expect(formatSigCount(Number.NaN, false)).toBe("—");
  });
});

describe("blastLabel", () => {
  it("counts distinct queues × distinct types", () => {
    expect(
      blastLabel([
        { queue: "billing", type: "invoice:charge" },
        { queue: "billing", type: "invoice:retry" },
        { queue: "payments", type: "invoice:refund" },
      ])
    ).toBe("2q × 3t");
  });
  it("renders an empty matrix as 0q × 0t", () => {
    expect(blastLabel([])).toBe("0q × 0t");
  });
});

describe("templateSegments (~PLACEHOLDER~ highlighting)", () => {
  it("splits literals and placeholders", () => {
    expect(templateSegments("connection refused to ~HOST~")).toEqual([
      { text: "connection refused to ", placeholder: false },
      { text: "~HOST~", placeholder: true },
    ]);
  });
  it("handles multiple placeholder kinds", () => {
    const segs = templateSegments("job ~ID~ failed after ~N~s on ~HOST~");
    expect(segs.filter((s) => s.placeholder).map((s) => s.text)).toEqual([
      "~ID~",
      "~N~",
      "~HOST~",
    ]);
  });
  it("treats an unknown tilde token as a literal", () => {
    expect(templateSegments("~WAT~ happened")).toEqual([
      { text: "~WAT~ happened", placeholder: false },
    ]);
  });
  it("handles a template with no placeholders", () => {
    expect(templateSegments("plain failure")).toEqual([
      { text: "plain failure", placeholder: false },
    ]);
  });
});

describe("sigVerbsForState (verbs per state capability)", () => {
  it("offers run/archive/delete on retry", () => {
    expect(sigVerbsForState("retry")).toEqual(["run", "archive", "delete"]);
  });
  it("offers run/delete on archived — archiving the archived is meaningless", () => {
    expect(sigVerbsForState("archived")).toEqual(["run", "delete"]);
  });
});

describe("sigLiteralPrefix (raw template prefix extraction)", () => {
  it("takes the literal before the first placeholder", () => {
    expect(sigLiteralPrefix("gateway timeout POST https://~HOST~/v2/charges")).toBe(
      "gateway timeout POST https://"
    );
  });
  it("trims trailing whitespace from the prefix", () => {
    expect(sigLiteralPrefix("connection refused to ~HOST~")).toBe(
      "connection refused to"
    );
  });
  it("falls back to the longest literal segment when the template starts with a placeholder", () => {
    expect(sigLiteralPrefix("~HOST~ unreachable after timeout")).toBe(
      "unreachable after timeout"
    );
  });
  it("cuts at the first double quote — AQL quoted strings cannot escape quotes", () => {
    expect(sigLiteralPrefix('missing closing " in config file')).toBe(
      "missing closing"
    );
  });
  it("returns empty for an all-placeholder template", () => {
    expect(sigLiteralPrefix("~ID~ ~N~")).toBe("");
  });
  it("returns empty when every literal is too short to scope on", () => {
    expect(sigLiteralPrefix("~ID~ at ~HOST~")).toBe("");
  });
});

describe("sigErrorClause (BulkJobModal scope aql — state-free)", () => {
  it("builds the bare predicate without repeating the scope's state", () => {
    expect(sigErrorClause("gateway timeout POST https://~HOST~/v2/charges")).toBe(
      'error~"gateway timeout POST https://"'
    );
  });
  it("returns null when no usable literal exists", () => {
    expect(sigErrorClause("~ID~ ~N~")).toBeNull();
  });
});

describe("sigScopeAql (verb scope construction)", () => {
  it("builds the §3.6 retry scope verbatim", () => {
    expect(sigScopeAql("gateway timeout POST https://~HOST~/v2/charges", "retry")).toBe(
      'state=retry error~"gateway timeout POST https://"'
    );
  });
  it("builds the archived scope with the same prefix", () => {
    expect(sigScopeAql("connection refused to ~HOST~", "archived")).toBe(
      'state=archived error~"connection refused to"'
    );
  });
  it("returns null when no usable literal exists — verbs must disable", () => {
    expect(sigScopeAql("~ID~ ~N~", "retry")).toBeNull();
  });
  it("never emits an unterminated quote", () => {
    const aql = sigScopeAql('bad token " quote everywhere', "retry");
    expect(aql).toBe('state=retry error~"bad token"');
    expect((aql!.match(/"/g) ?? []).length % 2).toBe(0);
  });
});

describe("sigTasksSearch (sample peek link)", () => {
  it("carries the scope query and the peek target", () => {
    const s = sigTasksSearch("connection refused to ~HOST~", "archived", {
      queue: "billing",
      id: "8f3a2c",
    });
    const params = new URLSearchParams(s);
    expect(params.get("q")).toBe('state=archived error~"connection refused to"');
    expect(params.get("peek")).toBe("billing/8f3a2c");
  });
  it("omits the query for unscopeable templates but keeps the peek", () => {
    const s = sigTasksSearch("~ID~ ~N~", "retry", { queue: "q", id: "t1" });
    const params = new URLSearchParams(s);
    expect(params.get("q")).toBeNull();
    expect(params.get("peek")).toBe("q/t1");
  });
  it("returns an empty string when there is nothing to link", () => {
    expect(sigTasksSearch("~ID~", "retry")).toBe("");
  });
});
