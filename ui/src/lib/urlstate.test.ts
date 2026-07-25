import { describe, it, expect } from "vitest";
import {
  ConsoleState,
  DEFAULT_CONSOLE_STATE,
  parseConsoleState,
  serializeConsoleState,
  parsePeek,
  serializePeek,
  serializeMetaPair,
  DirectoryState,
  DEFAULT_DIRECTORY_STATE,
  parseDirectoryState,
  serializeDirectoryState,
} from "./urlstate";

describe("parseConsoleState", () => {
  it("returns defaults for empty params", () => {
    expect(parseConsoleState(new URLSearchParams())).toEqual(DEFAULT_CONSOLE_STATE);
  });

  it("reads every field from the URL", () => {
    const params = new URLSearchParams(
      "q=email&state=retry&queue=billing&meta=region:eu&meta=tier:gold&page=3&size=50&mode=group:error"
    );
    expect(parseConsoleState(params)).toEqual({
      q: "email",
      state: "retry",
      queue: "billing",
      meta: [
        { key: "region", value: "eu" },
        { key: "tier", value: "gold" },
      ],
      page: 2, // 1-based in the URL, 0-based internally
      size: 50,
      mode: "group:error",
    });
  });

  it("degrades invalid values to defaults instead of crashing", () => {
    const params = new URLSearchParams("state=bogus&size=7&page=zero&mode=nope&meta=novalue");
    const state = parseConsoleState(params);
    expect(state.state).toBe("pending");
    expect(state.size).toBe(20);
    expect(state.page).toBe(0);
    expect(state.mode).toBe("rows");
    expect(state.meta).toEqual([]); // malformed meta (no colon) dropped
  });

  it("keeps colons inside metadata values (split on first colon only)", () => {
    const params = new URLSearchParams();
    params.append("meta", "url:https://example.com");
    expect(parseConsoleState(params).meta).toEqual([
      { key: "url", value: "https://example.com" },
    ]);
  });
});

describe("serializeConsoleState", () => {
  it("omits default values so shared URLs stay short", () => {
    expect(serializeConsoleState(DEFAULT_CONSOLE_STATE).toString()).toBe("");
  });

  it("round-trips a full state exactly", () => {
    const state: ConsoleState = {
      q: "gateway timeout",
      state: "archived",
      queue: "billing:invoice",
      meta: [{ key: "region", value: "eu" }],
      page: 4,
      size: 100,
      mode: "group:type",
    };
    expect(parseConsoleState(serializeConsoleState(state))).toEqual(state);
  });

  it("writes the page 1-based for humans", () => {
    const params = serializeConsoleState({ ...DEFAULT_CONSOLE_STATE, page: 2 });
    expect(params.get("page")).toBe("3");
  });

  it("preserves params it does not own (peek deep link)", () => {
    const base = new URLSearchParams("peek=email/8f3a&state=retry");
    const params = serializeConsoleState({ ...DEFAULT_CONSOLE_STATE, q: "x" }, base);
    expect(params.get("peek")).toBe("email/8f3a");
    expect(params.get("q")).toBe("x");
    // Stale console params from the base are cleared, not merged.
    expect(params.get("state")).toBeNull();
  });

  it("clears previous meta params before writing the new set", () => {
    const base = new URLSearchParams("meta=old:1&meta=old:2");
    const params = serializeConsoleState(
      { ...DEFAULT_CONSOLE_STATE, meta: [{ key: "new", value: "3" }] },
      base
    );
    expect(params.getAll("meta")).toEqual(["new:3"]);
  });
});

describe("serializeMetaPair", () => {
  it("uses the API's key:value wire format", () => {
    expect(serializeMetaPair({ key: "region", value: "eu" })).toBe("region:eu");
  });
});

describe("parseDirectoryState", () => {
  it("returns defaults for empty params", () => {
    expect(parseDirectoryState(new URLSearchParams())).toEqual(DEFAULT_DIRECTORY_STATE);
  });

  it("reads every field from the URL", () => {
    const params = new URLSearchParams(
      "sort=consumers&dir=asc&f=consumers%3D0+pending%3E10000&cursor=abc123&limit=50"
    );
    expect(parseDirectoryState(params)).toEqual({
      sort: "consumers",
      dir: "asc",
      f: "consumers=0 pending>10000",
      cursor: "abc123",
      limit: 50,
    });
  });

  it("degrades invalid values to defaults instead of crashing", () => {
    const params = new URLSearchParams("sort=bogus&dir=sideways&limit=7");
    const state = parseDirectoryState(params);
    expect(state.sort).toBe("oldest_pending_age");
    expect(state.dir).toBe("desc");
    expect(state.limit).toBe(100);
  });
});

describe("serializeDirectoryState", () => {
  it("omits default values so shared URLs stay short", () => {
    expect(serializeDirectoryState(DEFAULT_DIRECTORY_STATE).toString()).toBe("");
  });

  it("round-trips a full state exactly", () => {
    const state: DirectoryState = {
      sort: "error_rate",
      dir: "asc",
      f: "name~email consumers=0",
      cursor: "opaque/cursor+1==",
      limit: 200,
    };
    expect(parseDirectoryState(serializeDirectoryState(state))).toEqual(state);
  });

  it("preserves params it does not own", () => {
    const base = new URLSearchParams("peek=email/8f3a&sort=pending&cursor=old");
    const params = serializeDirectoryState(
      { ...DEFAULT_DIRECTORY_STATE, f: "consumers=0" },
      base
    );
    expect(params.get("peek")).toBe("email/8f3a");
    expect(params.get("f")).toBe("consumers=0");
    // Stale directory params from the base are cleared, not merged.
    expect(params.get("sort")).toBeNull();
    expect(params.get("cursor")).toBeNull();
  });
});

describe("peek param", () => {
  it("round-trips queue/id", () => {
    const target = { queue: "billing:invoice", id: "8f3a2c1d-9b41-4e02" };
    expect(parsePeek(serializePeek(target))).toEqual(target);
  });

  it("splits on the LAST slash so queue names may contain slashes", () => {
    expect(parsePeek("team/billing/8f3a2c1d")).toEqual({
      queue: "team/billing",
      id: "8f3a2c1d",
    });
  });

  it("rejects malformed values", () => {
    expect(parsePeek(null)).toBeNull();
    expect(parsePeek("")).toBeNull();
    expect(parsePeek("noslash")).toBeNull();
    expect(parsePeek("/id-only")).toBeNull();
    expect(parsePeek("queue-only/")).toBeNull();
  });
});
