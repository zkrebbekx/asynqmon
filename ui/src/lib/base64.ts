// Base64 payload-field detection for the task drawer (decoded-by-default
// with a raw toggle). Detection is deliberately layered so false positives
// are statistically negligible — a wrongly-"decoded" field is worse than a
// raw one:
//
//  1. shape: a JSON string value, ≥16 chars, strict base64 charset
//     (standard `A-Za-z0-9+/` or URL-safe `A-Za-z0-9_-`, padded or
//     unpadded, with a length base64 can actually produce), decoding to
//     ≥16 bytes;
//  2. character-class guard on the ENCODED text: at least two of
//     {lowercase, uppercase, digit} or an explicit base64-only character
//     (`+ / =`) — real base64 of real content is essentially never a single
//     class, while single-class strings (slugs, long words) are the main
//     false-positive source;
//  3. the decoded bytes must be strict, fully-printable UTF-8 (tab/newline
//     allowed, no other control bytes). For N random decoded bytes the
//     chance of passing is ~(95/256)^N — under 2e-7 at the 16-byte minimum
//     — which is what makes hex strings, dashless UUIDs, and ids that
//     merely LOOK base64-ish reliably fail;
//  4. content guard on the DECODED text: it must parse as a JSON object or
//     array, or contain at least one whitespace or punctuation character.
//     Real text has spaces and punctuation; a run of printable garbage that
//     survived layer 3 almost never does.
//
// Anything that fails any layer stays raw, silently. The UI labels what was
// decoded and keeps the raw view one click away, so the transformation is
// always verifiable.

const STD_B64 = /^[A-Za-z0-9+/]+={0,2}$/;
const URL_B64 = /^[A-Za-z0-9_-]+={0,2}$/;
// Whitespace or Unicode punctuation anywhere in the decoded text (layer 4).
const TEXT_MARK = /[\s\p{P}]/u;

const MIN_ENCODED_LEN = 16;
const MIN_DECODED_BYTES = 16;

// hasControlChar reports whether text contains a C0 control (other than
// tab, LF, CR) or DEL. Written as a loop rather than a regex so the intent
// is explicit and lint-clean.
function hasControlChar(text: string): boolean {
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i);
    if (c === 0x09 || c === 0x0a || c === 0x0d) continue;
    if (c < 0x20 || c === 0x7f) return true;
  }
  return false;
}

// looksLikeText is layer 4: JSON object/array, or at least one whitespace
// or punctuation character.
function looksLikeText(text: string): boolean {
  const t = text.trim();
  if (t.startsWith("{") || t.startsWith("[")) {
    try {
      const parsed = JSON.parse(t);
      if (parsed !== null && typeof parsed === "object") return true;
    } catch {
      /* fall through to the punctuation check */
    }
  }
  return TEXT_MARK.test(text);
}

// tryDecodeBase64Text returns the decoded text when `s` passes every layer,
// or null. Exported for direct testing.
export function tryDecodeBase64Text(s: string): string | null {
  if (s.length < MIN_ENCODED_LEN) return null;

  if (!STD_B64.test(s) && !URL_B64.test(s)) return null;
  // Accept padded and unpadded forms of both alphabets (base64url is
  // typically unpadded, and unpadded standard base64 is common too); a
  // length that no base64 encoding can produce (% 4 === 1 after stripping
  // padding) is rejected.
  const un = s.replace(/=+$/, "");
  if (un.length % 4 === 1) return null;
  let normalized = un.replace(/-/g, "+").replace(/_/g, "/");
  normalized += "=".repeat((4 - (normalized.length % 4)) % 4);

  // Class guard (layer 2), on the original text.
  const classes =
    Number(/[a-z]/.test(s)) + Number(/[A-Z]/.test(s)) + Number(/[0-9]/.test(s));
  if (classes < 2 && !/[+/=]/.test(s)) return null;

  let bytes: Uint8Array;
  try {
    const bin = atob(normalized);
    bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  } catch {
    return null;
  }
  if (bytes.length < MIN_DECODED_BYTES) return null;

  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return null;
  }
  if (hasControlChar(text)) return null;
  if (!looksLikeText(text)) return null;
  return text;
}

export interface DecodedFields {
  // Pretty-printed JSON (or plain text for a bare-string payload) with every
  // detected field replaced by its decoded form.
  text: string;
  // Dotted paths of the fields that were decoded, in encounter order.
  paths: string[];
}

type JsonValue = null | boolean | number | string | JsonValue[] | { [k: string]: JsonValue };

function walk(value: JsonValue, path: string, paths: string[]): JsonValue {
  if (typeof value === "string") {
    const decoded = tryDecodeBase64Text(value);
    if (decoded === null) return value;
    paths.push(path || "$");
    // Base64-of-JSON is the common real-world case (nested envelopes):
    // embed the parsed structure so it reads naturally.
    try {
      const parsed = JSON.parse(decoded);
      if (parsed !== null && typeof parsed === "object") return parsed as JsonValue;
    } catch {
      /* plain text — use as-is */
    }
    return decoded;
  }
  if (Array.isArray(value)) {
    return value.map((v, i) => walk(v, `${path}[${i}]`, paths));
  }
  if (value !== null && typeof value === "object") {
    const out: { [k: string]: JsonValue } = {};
    for (const [k, v] of Object.entries(value)) {
      out[k] = walk(v, path ? `${path}.${k}` : k, paths);
    }
    return out;
  }
  return value;
}

// decodeBase64Fields inspects a payload/result string and returns the
// decoded rendering when at least one base64 field was detected, else null
// (meaning: nothing to toggle — show raw only). Handles both JSON payloads
// (fields decoded recursively) and a payload that IS one bare base64 string.
export function decodeBase64Fields(raw: string): DecodedFields | null {
  const trimmed = raw.trim();

  // Whole-payload base64 (no JSON framing).
  if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) {
    const decoded = tryDecodeBase64Text(trimmed);
    if (decoded === null) return null;
    try {
      const parsed = JSON.parse(decoded);
      if (parsed !== null && typeof parsed === "object") {
        return { text: JSON.stringify(parsed, null, 2), paths: ["$"] };
      }
    } catch {
      /* plain text */
    }
    return { text: decoded, paths: ["$"] };
  }

  let obj: JsonValue;
  try {
    obj = JSON.parse(trimmed);
  } catch {
    return null; // not valid JSON (e.g. server-truncated) — raw only
  }
  const paths: string[] = [];
  const transformed = walk(obj, "", paths);
  if (paths.length === 0) return null;
  return { text: JSON.stringify(transformed, null, 2), paths };
}
