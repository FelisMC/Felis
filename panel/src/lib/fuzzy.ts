
// Prefer contiguous matches, then tight early subsequences; return -1 for no match.
// This is the model fzf and command palettes use.
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

// Require every query term in at least one field, summing each term's best match.
// terms is expected pre-split and lowercased.
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
