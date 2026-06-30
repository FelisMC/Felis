// A dependency-free fuzzy matcher for client-side list search (the SysAdmin fleet
// table, and reusable elsewhere). Two levels: fuzzyScore ranks one string against
// one query; matchScore ranks a record (several fields) against a multi-term query.

/** fuzzyScore ranks how well `query` matches `text`, case-insensitively. It returns
 *  a score (higher = better) or -1 for no match. A contiguous substring scores
 *  highest (earlier / at a word boundary is better); failing that, a subsequence
 *  match — the query's chars appear in order but not necessarily adjacent, so "srv"
 *  matches "survival" — still matches, scored by how tight and early the run is.
 *  This is the model fuzzy finders (fzf, command palettes) use. */
export function fuzzyScore(text: string, query: string): number {
  const t = text.toLowerCase();
  const q = query.toLowerCase();
  if (!q) return 0;
  if (!t) return -1;
  const idx = t.indexOf(q);
  if (idx !== -1) {
    // Substring hit dominates: base 1000, earlier is better, plus a word-start
    // bonus (start of string or just after a separator like -, _, ., @, /).
    const atBoundary = idx === 0 || /[-_.@/\s]/.test(t[idx - 1]);
    return 1000 - idx + (atBoundary ? 100 : 0);
  }
  // Subsequence fallback: every query char must appear in order; reward adjacency.
  let cursor = 0;
  let score = 0;
  let run = 0;
  for (const ch of q) {
    const found = t.indexOf(ch, cursor);
    if (found === -1) return -1;
    run = found === cursor ? run + 1 : 0;
    score += 1 + run * 2;
    cursor = found + 1;
  }
  return score;
}

/** matchScore is a record's overall relevance for a multi-term query, across its
 *  searchable `fields`. EVERY term must hit at least one field (AND across terms,
 *  OR across fields) — so "owner surv" finds a server named survival owned by
 *  owner@… — and the score is the sum of each term's best field score. Returns -1
 *  if any term fails to match any field. `terms` is expected pre-split + lowercased
 *  (callers usually `query.trim().toLowerCase().split(/\s+/).filter(Boolean)`). */
export function matchScore(fields: string[], terms: string[]): number {
  let total = 0;
  for (const term of terms) {
    let best = -1;
    for (const f of fields) {
      const s = fuzzyScore(f, term);
      if (s > best) best = s;
    }
    if (best < 0) return -1;
    total += best;
  }
  return total;
}
