// Subsequence fuzzy matcher for the ⌘K palette (build contract §4.1): the
// query must appear in the text as an in-order subsequence; the score ranks
// consecutive runs, word-boundary hits and early matches above scattered
// ones. Pure functions, fully unit-testable.

export interface FuzzyMatch {
  score: number;
  // Character indices of the text that matched, for <mark> rendering.
  indices: number[];
}

// Characters that start a "word" inside queue names and view labels.
const BOUNDARIES = new Set(["-", "_", ":", ".", "/", " "]);

/**
 * fuzzyMatch returns null when `query` is not an in-order subsequence of
 * `text` (case-insensitive). Scoring, per matched char:
 *   +1  base
 *   +2  consecutive with the previous matched char
 *   +1.5 at a word boundary (text start or preceded by -_:./ )
 * minus a penalty for how late the first match starts and a mild penalty for
 * text length, so `bil` ranks "billing:invoice" above "global-billing".
 * An empty query matches everything with score 0.
 */
export function fuzzyMatch(query: string, text: string): FuzzyMatch | null {
  if (query === "") return { score: 0, indices: [] };
  const q = query.toLowerCase();
  const t = text.toLowerCase();

  // Greedy left-to-right match anchored at `start`. Returns null when the
  // query is not a subsequence of text[start:].
  const matchFrom = (start: number): FuzzyMatch | null => {
    const indices: number[] = [];
    let score = 0;
    let ti = start;
    for (let qi = 0; qi < q.length; qi++) {
      const c = q[qi];
      let found = -1;
      while (ti < t.length) {
        if (t[ti] === c) {
          found = ti;
          break;
        }
        ti++;
      }
      if (found === -1) return null;
      score += 1;
      if (indices.length > 0 && found === indices[indices.length - 1] + 1) score += 2;
      if (found === 0 || BOUNDARIES.has(t[found - 1])) score += 1.5;
      indices.push(found);
      ti = found + 1;
    }
    // Earlier first-match and shorter text rank higher on equal hits.
    score -= indices[0] * 0.1;
    score -= text.length * 0.01;
    return { score, indices };
  };

  // A single greedy pass anchors on the FIRST occurrence of q[0] and can
  // miss a better-aligned run later in the text ("inv" should anchor on the
  // "invoice" in "billing:invoice", not the first "i"). Try every anchor of
  // the first query char and keep the best-scoring alignment.
  let best: FuzzyMatch | null = null;
  for (let start = 0; start < t.length; start++) {
    if (t[start] !== q[0]) continue;
    const m = matchFrom(start);
    if (m === null) break; // no anchor further right can succeed either
    if (best === null || m.score > best.score) best = m;
  }
  return best;
}

export interface FuzzyResult<T> {
  item: T;
  match: FuzzyMatch;
}

/**
 * fuzzyFilter matches `query` against `key(item)` for every item, dropping
 * non-matches, sorting by score descending (stable — original order breaks
 * ties), capped at `limit`.
 */
export function fuzzyFilter<T>(
  query: string,
  items: readonly T[],
  key: (item: T) => string,
  limit = Infinity
): FuzzyResult<T>[] {
  const out: FuzzyResult<T>[] = [];
  for (const item of items) {
    const match = fuzzyMatch(query, key(item));
    if (match) out.push({ item, match });
  }
  // Array.prototype.sort is stable per spec — ties keep input order.
  out.sort((a, b) => b.match.score - a.match.score);
  return out.slice(0, limit === Infinity ? out.length : limit);
}
