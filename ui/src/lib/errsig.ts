// Pure logic for the Errors screen (§3.6): trend-chip mapping, lower-bound
// count rendering, ~PLACEHOLDER~ template segmentation, blast-radius labels,
// and — most load-bearing — signature-scoped bulk-verb construction: turning
// a normalized template into the AQL scope a BulkJobModal job runs with.
// Pure functions so all of it is unit-testable without rendering.

/**************************************************************
                        Trend chips
 **************************************************************/

export type SigTrend = "new" | "growing" | "steady" | "recovering";

// trendTone maps a trend badge onto the console's chip tones. Growing is the
// incident (crit); new is a warning-grade heads-up; recovering is good news;
// steady — and anything a future backend might send — renders muted.
export function trendTone(trend: string): "crit" | "warn" | "good" | "mut" {
  switch (trend) {
    case "growing":
      return "crit";
    case "new":
      return "warn";
    case "recovering":
      return "good";
    default:
      return "mut";
  }
}

/**************************************************************
                    Counts & labels
 **************************************************************/

// formatSigCount renders a signature count, prefixing "≥" when any source
// queue is archive-trimmed (§3.6: lower bounds are never shown as exact).
export function formatSigCount(count: number, isLowerBound: boolean): string {
  const n = Number.isFinite(count) ? count.toLocaleString("en-US") : "—";
  return isLowerBound ? `≥${n}` : n;
}

// blastLabel renders the blast radius the mockup's way: "2q × 3t"
// (distinct queues × distinct task types).
export function blastLabel(cells: { queue: string; type: string }[]): string {
  const queues = new Set<string>();
  const types = new Set<string>();
  for (const c of cells) {
    queues.add(c.queue);
    types.add(c.type);
  }
  return `${queues.size}q × ${types.size}t`;
}

/**************************************************************
                Template segmentation (~PLACEHOLDER~)
 **************************************************************/

export interface TemplateSegment {
  text: string;
  placeholder: boolean;
}

const PLACEHOLDER_RE = /(~(?:N|ID|HOST|S)~)/g;

// templateSegments splits a template into literal and placeholder segments
// so the view can highlight ~HOST~/~N~/~ID~/~S~ tokens.
export function templateSegments(template: string): TemplateSegment[] {
  const out: TemplateSegment[] = [];
  for (const part of template.split(PLACEHOLDER_RE)) {
    if (part === "") continue;
    // Never .test() with the /g regex — its lastIndex is stateful.
    out.push({ text: part, placeholder: /^~(?:N|ID|HOST|S)~$/.test(part) });
  }
  return out;
}

/**************************************************************
            Signature-scoped verb construction (§3.6)
 **************************************************************/

// The states signature-scoped verbs can act on: AQL's `error~` predicate is
// answerable only where asynq stores an ErrorMsg (retry, archived).
export const SIG_SCOPE_STATES = ["retry", "archived"] as const;
export type SigScopeState = (typeof SIG_SCOPE_STATES)[number];

// Per-state verb capability, mirroring the backend matrix (jobs/job.go
// verbCaps): retry supports run/archive/delete; archived supports run/delete
// (archiving the archived is meaningless).
export function sigVerbsForState(state: SigScopeState): ("run" | "archive" | "delete")[] {
  if (state === "archived") return ["run", "delete"];
  return ["run", "archive", "delete"];
}

// Minimum literal length worth scoping on: shorter fragments over-match.
const MIN_LITERAL = 4;

// sigLiteralPrefix extracts the raw-error literal the scope matches on.
// `error~` is a substring match over the RAW error message, and templates
// contain placeholders that never literally occur in raw errors — so the
// scope uses the template's literal PREFIX (text before the first
// placeholder), falling back to the longest literal segment anywhere when
// the prefix is too short. AQL quoted strings have no escape mechanism, so
// anything from the first double quote on is dropped. Returns "" when no
// usable literal exists (the view disables the verbs and says why).
export function sigLiteralPrefix(template: string): string {
  const segments = template.split(PLACEHOLDER_RE).filter((s) => !/^~(?:N|ID|HOST|S)~$/.test(s));
  const clean = (s: string) => {
    const q = s.indexOf('"');
    return (q >= 0 ? s.slice(0, q) : s).trim();
  };
  const prefixSource = template.split(PLACEHOLDER_RE)[0] ?? "";
  const prefix = clean(prefixSource);
  if (prefix.length >= MIN_LITERAL) return prefix;
  let best = "";
  for (const seg of segments) {
    const c = clean(seg);
    if (c.length > best.length) best = c;
  }
  return best.length >= MIN_LITERAL ? best : "";
}

// sigErrorClause builds the bare predicate for a signature:
//   error~"<raw template prefix>"
// Null when the template has no usable literal. This is what a BulkJobModal
// scope carries as its aql — the scope's own `state` field names the state,
// so the clause must not repeat it (the modal echoes both). The predicate
// deliberately matches the raw-error PREFIX, which can be broader than the
// exact signature — the job's preview and per-task re-verification are the
// safety net.
export function sigErrorClause(template: string): string | null {
  const prefix = sigLiteralPrefix(template);
  if (prefix === "") return null;
  return `error~"${prefix}"`;
}

// sigScopeAql is the self-contained form for surfaces where the query IS the
// whole scope (the Tasks console's q param):
//   state=retry error~"<raw template prefix>"
export function sigScopeAql(template: string, state: SigScopeState): string | null {
  const clause = sigErrorClause(template);
  if (clause === null) return null;
  return `state=${state} ${clause}`;
}

/**************************************************************
              Sample-peek link (Tasks console drawer)
 **************************************************************/

// sigTasksSearch builds the /tasks search string for a signature: the scope
// query pre-filled (when constructible) plus an optional peek target. The
// drawer resolves peek by queue/id independently of the list, so a ref whose
// task has since moved state still opens.
export function sigTasksSearch(
  template: string,
  state: SigScopeState,
  peek?: { queue: string; id: string }
): string {
  const params = new URLSearchParams();
  const aql = sigScopeAql(template, state);
  if (aql) params.set("q", aql);
  if (peek) params.set("peek", `${peek.queue}/${peek.id}`);
  const s = params.toString();
  return s ? `?${s}` : "";
}
