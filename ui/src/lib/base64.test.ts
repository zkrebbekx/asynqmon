import { describe, expect, it } from "vitest";
import { decodeBase64Fields, tryDecodeBase64Text } from "./base64";

const b64 = (s: string) => Buffer.from(s, "utf-8").toString("base64");
const b64url = (s: string) => Buffer.from(s, "utf-8").toString("base64url");

describe("tryDecodeBase64Text", () => {
  it("decodes base64 of JSON and of plain text", () => {
    const json = '{"card_last4":"4242","network":"visa"}';
    expect(tryDecodeBase64Text(b64(json))).toBe(json);
    const text = "plain text message with spaces";
    expect(tryDecodeBase64Text(b64(text))).toBe(text);
  });

  it("decodes URL-safe base64 (unpadded)", () => {
    const text = "subject?query=1&flag=true"; // encodes with -_ under base64url
    expect(tryDecodeBase64Text(b64url(text))).toBe(text);
  });

  it("rejects short strings and non-base64 charsets", () => {
    expect(tryDecodeBase64Text("dGVzdA==")).toBeNull(); // "test" — too short
    expect(tryDecodeBase64Text("2026-08-22T14:11:00Z")).toBeNull(); // ':' not base64
    expect(tryDecodeBase64Text("hello world padded")).toBeNull(); // spaces
  });

  it("rejects single-character-class strings (slugs, long words)", () => {
    expect(tryDecodeBase64Text("internationalization")).toBeNull();
    expect(tryDecodeBase64Text("organizationsetting")).toBeNull();
    expect(tryDecodeBase64Text("ABCDEFGHIJKLMNOPQRST")).toBeNull();
  });

  it("rejects hex strings, dashless UUIDs, and prefixed ids", () => {
    expect(tryDecodeBase64Text("deadbeefdeadbeef")).toBeNull();
    expect(tryDecodeBase64Text("0123456789abcdef0123456789abcdef")).toBeNull();
    expect(tryDecodeBase64Text("550e8400e29b41d4a716446655440000")).toBeNull();
    expect(tryDecodeBase64Text("550e8400-e29b-41d4-a716-446655440000")).toBeNull();
    expect(tryDecodeBase64Text("req_9f8e7d6c5b4a3f2e")).toBeNull();
  });

  it("rejects base64 of binary content (non-printable decode)", () => {
    const binary = Buffer.from([0x1f, 0x8b, 0x08, 0x00, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]);
    expect(tryDecodeBase64Text(binary.toString("base64"))).toBeNull();
  });
});

describe("decodeBase64Fields", () => {
  it("replaces detected fields, embeds base64-of-JSON as structure, and reports paths", () => {
    const inner = { event: "order.updated", items: [1, 2, 3] };
    const payload = JSON.stringify({
      envelope: b64(JSON.stringify(inner)),
      note: b64("human readable annotation text"),
      order_id: "ord_00042",
      nested: { blob: b64("nested decoded value here") },
    });
    const out = decodeBase64Fields(payload);
    expect(out).not.toBeNull();
    expect(out!.paths).toEqual(["envelope", "note", "nested.blob"]);
    const parsed = JSON.parse(out!.text);
    expect(parsed.envelope).toEqual(inner);
    expect(parsed.note).toBe("human readable annotation text");
    expect(parsed.order_id).toBe("ord_00042"); // untouched
    expect(parsed.nested.blob).toBe("nested decoded value here");
  });

  it("returns null when nothing is detected (plain payloads stay raw-only)", () => {
    expect(decodeBase64Fields('{"user_id":42,"template":"welcome"}')).toBeNull();
    expect(decodeBase64Fields("not json and not base64!")).toBeNull();
  });

  it("handles a payload that is one bare base64 string", () => {
    const inner = '{"wrapped":true,"kind":"bare"}';
    const out = decodeBase64Fields(b64(inner));
    expect(out).not.toBeNull();
    expect(out!.paths).toEqual(["$"]);
    expect(JSON.parse(out!.text)).toEqual({ wrapped: true, kind: "bare" });
  });

  it("returns null for truncated (unparseable) JSON", () => {
    expect(decodeBase64Fields('{"envelope":"' + b64("x".repeat(40)) + "…")).toBeNull();
  });
});
