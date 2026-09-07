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

  it("requires at least 16 decoded bytes", () => {
    expect(tryDecodeBase64Text(b64("fifteen bytes!!"))).toBeNull(); // 15 bytes
    expect(tryDecodeBase64Text(b64("sixteen bytes!!!"))).toBe("sixteen bytes!!!"); // 16 bytes
  });

  it("requires JSON or a space/punctuation mark in decoded plain text", () => {
    // Printable, but no whitespace or punctuation: stays raw.
    expect(tryDecodeBase64Text(b64("abcdefghijklmnopqrstuvwx"))).toBeNull();
    expect(tryDecodeBase64Text(b64("Zx9Qk2Lm7Vb4Tn8Rw1Yp5Hc3"))).toBeNull();
    // A single space or punctuation mark is enough.
    expect(tryDecodeBase64Text(b64("abcdefghijklmnop qrstuvwx"))).toBe("abcdefghijklmnop qrstuvwx");
    expect(tryDecodeBase64Text(b64("abcdefghijklmnop.qrstuvwx"))).toBe("abcdefghijklmnop.qrstuvwx");
    // JSON with no spaces still decodes (a JSON object is text by definition).
    const json = '{"a":1,"b":"xyzxyz"}';
    expect(tryDecodeBase64Text(b64(json))).toBe(json);
  });

  // Regression probe for the false-positive rate (review #54.7): random
  // [A-Za-z0-9]{16} and {24} strings decode to 12 and 18 bytes; with the
  // 16-byte floor and the text guard, none of 20,000 seeded samples per
  // length may decode. The PRNG is seeded (mulberry32) so a failure is
  // reproducible.
  it("decodes zero of 20,000 seeded random alphanumeric strings", () => {
    const mulberry32 = (seed: number) => () => {
      seed |= 0;
      seed = (seed + 0x6d2b79f5) | 0;
      let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
    const ALPHANUM = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
    for (const len of [16, 24]) {
      const rand = mulberry32(0x5eed + len);
      let decoded = 0;
      for (let i = 0; i < 20_000; i++) {
        let s = "";
        for (let j = 0; j < len; j++) s += ALPHANUM[Math.floor(rand() * ALPHANUM.length)];
        if (tryDecodeBase64Text(s) !== null) decoded++;
      }
      expect(decoded, `random [A-Za-z0-9]{${len}}`).toBe(0);
    }
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
