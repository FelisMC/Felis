// Server tag rules; keep them aligned with cleanServerTags in internal/api/handlers_tags.go.
export const MAX_SERVER_TAGS = 6;
export const MAX_SERVER_TAG_LENGTH = 20;

const FORBIDDEN = /[,\p{C}\p{Zl}\p{Zp}]/u;

export type TagIssue = "length" | "characters";

/** tagIssue says why the API would refuse this trimmed tag, or null if it takes it. */
export function tagIssue(tag: string): TagIssue | null {
  const length = [...tag].length;
  if (length < 1 || length > MAX_SERVER_TAG_LENGTH) return "length";
  return FORBIDDEN.test(tag) ? "characters" : null;
}

/** mergeTags appends tags the way the API keeps them: trimmed, a repeat in any letter case dropped, the first spelling kept. */
export function mergeTags(current: readonly string[], added: readonly string[]): string[] {
  const merged = [...current];
  const seen = new Set(merged.map((tag) => tag.toLowerCase()));
  for (const raw of added) {
    const tag = raw.trim();
    if (tag === "" || seen.has(tag.toLowerCase())) continue;
    seen.add(tag.toLowerCase());
    merged.push(tag);
  }
  return merged;
}
