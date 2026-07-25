import { describe, it, expect } from "vitest";
import { extractCorrelation } from "./correlation";

describe("extractCorrelation", () => {
  it("finds a trace_id in a JSON payload", () => {
    expect(extractCorrelation('{"invoice_id":"inv_1","trace_id":"tr-7f3e9a"}')).toEqual({
      key: "trace_id",
      value: "tr-7f3e9a",
    });
  });

  it("recognizes correlation_id and request_id", () => {
    expect(extractCorrelation('{"correlation_id":"c-1"}')).toEqual({
      key: "correlation_id",
      value: "c-1",
    });
    expect(extractCorrelation('{"request_id":"r-1"}')).toEqual({
      key: "request_id",
      value: "r-1",
    });
  });

  it("prefers trace_id over the other keys when several are present", () => {
    const payload = '{"request_id":"r-1","trace_id":"t-1","correlation_id":"c-1"}';
    expect(extractCorrelation(payload)).toEqual({ key: "trace_id", value: "t-1" });
  });

  it("stringifies numeric ids like the metadata chips do", () => {
    expect(extractCorrelation('{"request_id":12345}')).toEqual({
      key: "request_id",
      value: "12345",
    });
  });

  it("ignores nested and empty values", () => {
    expect(extractCorrelation('{"trace_id":{"nested":true}}')).toBeNull();
    expect(extractCorrelation('{"trace_id":""}')).toBeNull();
  });

  it("returns null for non-JSON payloads", () => {
    expect(extractCorrelation("plain text")).toBeNull();
    expect(extractCorrelation("")).toBeNull();
  });
});
