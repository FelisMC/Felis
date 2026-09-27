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

/** The paths the read path guards by name (fileedit.guardedPaths): under another
 *  name it would hand back what it withholds, so the server refuses to move them
 *  (400 bad_path) and the page does not offer to. */
const MANAGED_PATHS = new Set([SECRET_CONFIG_PATH, "config", "server.properties"]);

export function isManaged(path: string): boolean {
  return MANAGED_PATHS.has(path);
}

/** The longest name a Linux filesystem takes, in bytes (NAME_MAX). */
const NAME_MAX = 255;

export type NameProblem = "name_required" | "name_slash" | "name_dots" | "name_too_long" | "name_taken";

/** nameProblem says why name cannot be used for a new entry in a folder listing
 *  `entries`, or null when it can. `current` is the entry being renamed, whose own
 *  name is not a clash. The name is taken as typed less surrounding spaces, which
 *  is what the page sends. */
export function nameProblem(
  raw: string,
  entries: readonly ServerFileEntry[] | null,
  current?: string,
): NameProblem | null {
  const name = raw.trim();
  if (name === "") return "name_required";
  if (name.includes("/")) return "name_slash";
  if (name === "." || name === "..") return "name_dots";
  if (new TextEncoder().encode(name).length > NAME_MAX) return "name_too_long";
  if (name !== current && entries?.some((e) => e.name === name)) return "name_taken";
  return null;
}

/** sortEntries puts folders first, then orders names the way people count
 *  ("r.2.0" before "r.10.0"), with case second to the letters ("apple" before
 *  "Banned"). The server lists in byte order. */
export function sortEntries(entries: readonly ServerFileEntry[]): ServerFileEntry[] {
  return [...entries].sort(
    (a, b) =>
      Number(b.is_dir) - Number(a.is_dir) ||
      a.name.localeCompare(b.name, undefined, { numeric: true }),
  );
}
