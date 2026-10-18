import type { ServerFileEntry } from "@/lib/types";

export function joinPath(dir: string, name: string): string {
  return dir === "" ? name : `${dir}/${name}`;
}

export function parentOf(dir: string): string {
  const i = dir.lastIndexOf("/");
  return i === -1 ? "" : dir.slice(0, i);
}

/** The file whose platform secret the read path refuses outright
 *  (fileedit.secretConfigPath). */
export const SECRET_CONFIG_PATH = "config/paper-global.yml";

// Renaming guarded files could bypass read protection; neither backend nor UI permits it.
const MANAGED_PATHS = new Set([SECRET_CONFIG_PATH, "config", "server.properties"]);

export function isManaged(path: string): boolean {
  return MANAGED_PATHS.has(path);
}

/** The longest name a Linux filesystem takes, in bytes (NAME_MAX). */
const NAME_MAX = 255;

/** nameTooLong says whether a name is past NAME_MAX. A Mac or Windows disk
 *  counts in characters, so a file picked there can be. */
export function nameTooLong(name: string): boolean {
  return new TextEncoder().encode(name).length > NAME_MAX;
}

export type NameProblem = "name_required" | "name_slash" | "name_dots" | "name_too_long" | "name_taken";

// nameProblem returns null for valid names; the current entry is excluded from rename conflicts.
export function nameProblem(
  raw: string,
  entries: readonly ServerFileEntry[] | null,
  current?: string,
): NameProblem | null {
  const name = raw.trim();
  if (name === "") return "name_required";
  if (name.includes("/")) return "name_slash";
  if (name === "." || name === "..") return "name_dots";
  if (nameTooLong(name)) return "name_too_long";
  if (name !== current && entries?.some((e) => e.name === name)) return "name_taken";
  return null;
}

/** sortEntries puts folders first, then orders names the way people count ("r.2.0" before "r.10.0"), with case second ("apple" before "Banned"). The server lists in byte order. */
export function sortEntries(entries: readonly ServerFileEntry[]): ServerFileEntry[] {
  return [...entries].sort(
    (a, b) =>
      Number(b.is_dir) - Number(a.is_dir) ||
      a.name.localeCompare(b.name, undefined, { numeric: true }),
  );
}
