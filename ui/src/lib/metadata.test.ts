import { describe, it, expect } from "vitest";
import { parseMetadata, matchesMetadata, collectMetadata, metaId } from "./metadata";

describe("parseMetadata", () => {
  it("extracts top-level scalar pairs from a JSON payload", () => {
    const pairs = parseMetadata('{"user_id":1002,"email":"u@example.com","active":true}');
    expect(pairs).toEqual([
      { key: "user_id", value: "1002" },
      { key: "email", value: "u@example.com" },
      { key: "active", value: "true" },
    ]);
  });

  it("skips nested objects and arrays", () => {
    const pairs = parseMetadata('{"a":1,"nested":{"x":1},"list":[1,2],"b":"ok"}');
    expect(pairs).toEqual([
      { key: "a", value: "1" },
      { key: "b", value: "ok" },
    ]);
  });

  it("returns nothing for non-JSON or non-object payloads", () => {
    expect(parseMetadata("plain text")).toEqual([]);
    expect(parseMetadata("[1,2,3]")).toEqual([]);
    expect(parseMetadata("")).toEqual([]);
  });
});

describe("matchesMetadata", () => {
  const payload = '{"user_id":1002,"region":"eu"}';

  it("matches when all filters are present (AND)", () => {
    expect(matchesMetadata(payload, [{ key: "region", value: "eu" }])).toBe(true);
    expect(
      matchesMetadata(payload, [
        { key: "region", value: "eu" },
        { key: "user_id", value: "1002" },
      ])
    ).toBe(true);
  });

  it("fails when any filter is absent", () => {
    expect(
      matchesMetadata(payload, [
        { key: "region", value: "eu" },
        { key: "user_id", value: "9999" },
      ])
    ).toBe(false);
  });

  it("matches everything when there are no filters", () => {
    expect(matchesMetadata("anything", [])).toBe(true);
  });
});

describe("collectMetadata", () => {
  it("dedupes and orders pairs by frequency", () => {
    const payloads = [
      '{"region":"eu","tier":"gold"}',
      '{"region":"eu","tier":"silver"}',
      '{"region":"us"}',
    ];
    const pairs = collectMetadata(payloads);
    // region=eu appears twice -> first
    expect(pairs[0]).toEqual({ key: "region", value: "eu" });
    expect(pairs).toContainEqual({ key: "tier", value: "gold" });
    expect(pairs).toContainEqual({ key: "region", value: "us" });
  });

  it("respects the limit", () => {
    const payloads = ['{"a":1,"b":2,"c":3,"d":4}'];
    expect(collectMetadata(payloads, 2)).toHaveLength(2);
  });
});

describe("metaId", () => {
  it("formats a stable id", () => {
    expect(metaId({ key: "region", value: "eu" })).toBe("region=eu");
  });
});
