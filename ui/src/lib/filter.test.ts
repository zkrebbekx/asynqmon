import { describe, it, expect } from "vitest";
import { matchesQuery } from "./filter";

describe("matchesQuery", () => {
  it("matches an empty or whitespace query against anything", () => {
    expect(matchesQuery("anything", "")).toBe(true);
    expect(matchesQuery("anything", "   ")).toBe(true);
  });

  it("matches case-insensitively", () => {
    expect(matchesQuery("email:Welcome", "WELCOME")).toBe(true);
    expect(matchesQuery("Critical", "crit")).toBe(true);
  });

  it("trims surrounding whitespace from the query", () => {
    expect(matchesQuery("report:generate", "  report  ")).toBe(true);
  });

  it("returns false when the substring is absent", () => {
    expect(matchesQuery("email:welcome", "payment")).toBe(false);
  });

  it("works on decoded payload contents", () => {
    expect(matchesQuery('{"user_id":1002,"email":"u@example.com"}', "1002")).toBe(true);
    expect(matchesQuery('{"user_id":1002}', "9999")).toBe(false);
  });
});
