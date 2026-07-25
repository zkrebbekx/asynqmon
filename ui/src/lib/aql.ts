// Client-side AQL clause model (build contract §3.4, phase 6).
//
// The BACKEND parser (Go package aql) is the single authority on validity —
// this module never validates fields or states. It exists so the console can
// treat the query STRING as the single source of truth: facet chips, state
// pills, and drawer pivots all COMPOSE INTO the text by editing clauses, and
// the URL carries nothing but the text. Pure string functions, fully
// unit-testable.

import { MetaPair } from "./metadata";

export interface AqlClause {
  field: string; // "" for free text
  op: string; // "", "=", "~", ">", ">=", "<", "<="
  value: string; // unquoted
  raw: string; // original token text
  pos: number; // char offset of the token in the query
}

// Flag clauses take no operator or value.
const FLAGS = new Set(["past_due", "orphaned"]);

// Two-char operators first so ">=" is not read as ">" + "=…".
const OPS = [">=", "<=", "=", "~", ">", "<"];

const CLAUSE_SHAPED = /^[A-Za-z_][A-Za-z0-9_.]*(>=|<=|=|~|>|<)/;

/** Quote-aware tokenizer: splits on whitespace outside double quotes,
 * keeping character offsets. An unterminated quote runs to end of string
 * (lenient — the backend reports the real error). */
export function tokenizeAql(q: string): { text: string; pos: number }[] {
  const tokens: { text: string; pos: number }[] = [];
  let i = 0;
  while (i < q.length) {
    if (q[i] === " " || q[i] === "\t") {
      i++;
      continue;
    }
    const start = i;
    let inQuote = false;
    while (i < q.length) {
      const c = q[i];
      if (c === '"') inQuote = !inQuote;
      else if ((c === " " || c === "\t") && !inQuote) break;
      i++;
    }
    tokens.push({ text: q.slice(start, i), pos: start });
  }
  return tokens;
}

function unquote(v: string): string {
  if (v.length >= 2 && v.startsWith('"') && v.endsWith('"')) return v.slice(1, -1);
  return v;
}

/** Loose clause parse — never throws, never validates. */
export function parseAqlClauses(q: string): AqlClause[] {
  return tokenizeAql(q).map((t) => {
    if (FLAGS.has(t.text)) return { field: t.text, op: "", value: "", raw: t.text, pos: t.pos };
    // Find the first operator outside quotes.
    let inQuote = false;
    for (let i = 0; i < t.text.length; i++) {
      const c = t.text[i];
      if (c === '"') {
        inQuote = !inQuote;
        continue;
      }
      if (inQuote) continue;
      const op = OPS.find((o) => t.text.startsWith(o, i));
      if (op && i > 0) {
        return {
          field: t.text.slice(0, i),
          op,
          value: unquote(t.text.slice(i + op.length)),
          raw: t.text,
          pos: t.pos,
        };
      }
      if (op && i === 0) break; // no field: free text
    }
    return { field: "", op: "", value: unquote(t.text), raw: t.text, pos: t.pos };
  });
}

/** Whether the string should be treated as AQL (mirrors the backend's
 * IsQuery): at least one clause-shaped or flag token. Plain free text keeps
 * legacy substring semantics. */
export function isAqlQuery(q: string): boolean {
  return tokenizeAql(q).some((t) => CLAUSE_SHAPED.test(t.text) || FLAGS.has(t.text));
}

/** Quote a value when it needs it (whitespace). */
export function quoteAqlValue(v: string): string {
  return /\s/.test(v) ? `"${v}"` : v;
}

/** First clause with the given field, if any. */
export function getClause(q: string, field: string): AqlClause | undefined {
  return parseAqlClauses(q).find((c) => c.field === field);
}

function rebuild(tokens: string[]): string {
  return tokens.filter((t) => t !== "").join(" ");
}

/** Set (replace-in-place or append) the clause for a field. An empty value
 * removes it. Fields are single-occurrence under this helper (queue=, state=);
 * for repeatable meta clauses use addMetaClause/removeMetaClause. */
export function setClause(q: string, field: string, op: string, value: string): string {
  const clauses = parseAqlClauses(q);
  const next = quoteAqlValue(value);
  let replaced = false;
  const tokens = clauses
    .map((c) => {
      if (c.field !== field) return c.raw;
      if (replaced) return ""; // drop duplicates
      replaced = true;
      return value === "" ? "" : `${field}${op}${next}`;
    })
    .filter((t) => t !== "");
  if (!replaced && value !== "") tokens.push(`${field}${op}${next}`);
  return rebuild(tokens);
}

/** Remove every clause for a field (optionally only those matching value). */
export function removeClause(q: string, field: string, value?: string): string {
  const tokens = parseAqlClauses(q)
    .filter((c) => !(c.field === field && (value === undefined || c.value === value)))
    .map((c) => c.raw);
  return rebuild(tokens);
}

/**************************************************************
                 Metadata chips ⇄ meta.* clauses
 **************************************************************/

export function metaClausesOf(q: string): MetaPair[] {
  return parseAqlClauses(q)
    .filter((c) => c.field.startsWith("meta.") && c.field.length > 5 && c.op === "=")
    .map((c) => ({ key: c.field.slice(5), value: c.value }));
}

export function hasMetaClause(q: string, key: string, value: string): boolean {
  return metaClausesOf(q).some((p) => p.key === key && p.value === value);
}

export function addMetaClause(q: string, key: string, value: string): string {
  if (hasMetaClause(q, key, value)) return q;
  const token = `meta.${key}=${quoteAqlValue(value)}`;
  return q === "" ? token : `${q} ${token}`;
}

export function removeMetaClause(q: string, key: string, value: string): string {
  const tokens = parseAqlClauses(q)
    .filter((c) => !(c.field === `meta.${key}` && c.op === "=" && c.value === value))
    .map((c) => c.raw);
  return rebuild(tokens);
}

/**************************************************************
                Legacy-URL migration (phase 6)
 **************************************************************/

/** Build one AQL string from pre-phase-6 URL params: queue=X and state=Y are
 * prepended as clauses, meta chips become meta.k=v clauses, and old
 * free-text q survives as a (quoted, when combined) free-text token. */
export function aqlFromLegacy(parts: {
  queue?: string;
  state?: string;
  meta?: MetaPair[];
  freeText?: string;
}): string {
  const tokens: string[] = [];
  if (parts.queue && parts.queue !== "all") tokens.push(`queue=${quoteAqlValue(parts.queue)}`);
  if (parts.state) tokens.push(`state=${parts.state}`);
  for (const p of parts.meta ?? []) tokens.push(`meta.${p.key}=${quoteAqlValue(p.value)}`);
  const free = (parts.freeText ?? "").trim();
  if (free !== "") {
    // Standalone free text stays verbatim (legacy substring semantics on the
    // backend). Combined with clauses it must be one quoted token so the AQL
    // parser reads it as a single free-text clause.
    if (tokens.length === 0) return free;
    tokens.push(/\s/.test(free) ? `"${free}"` : free);
  }
  return tokens.join(" ");
}

/**************************************************************
             Rejection rendering ({error, position, hint})
 **************************************************************/

export interface AqlRejection {
  error: string;
  position: number;
  hint?: string;
}

/** True when an API error body is the structured AQL rejection. */
export function isAqlRejection(data: unknown): data is AqlRejection {
  return (
    typeof data === "object" &&
    data !== null &&
    typeof (data as AqlRejection).error === "string" &&
    typeof (data as AqlRejection).position === "number"
  );
}

/** The caret line rendered under the echoed query: spaces up to the
 * rejection position, then a caret. Position is clamped into the query. */
export function caretLine(query: string, position: number): string {
  const p = Math.max(0, Math.min(position, Math.max(0, query.length)));
  return " ".repeat(p) + "^";
}
