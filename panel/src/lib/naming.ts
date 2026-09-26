// The server name rule (spec §2, §22) as the API enforces it in internal/naming:
// 3–32 lowercase letters, digits or hyphens, no hyphen at either end, and none
// of the names the platform keeps for itself. The same rule covers subdomains.
// The create form checks it as the person types, so a name the API would
// answer with 400 never reaches it.

// RESERVED_SERVER_NAMES mirrors `reserved` in internal/naming/naming.go.
export const RESERVED_SERVER_NAMES: readonly string[] = [
  "login",
  "lobby",
  "admin",
  "panel",
  "console",
  "api",
  "felis",
  "velocity",
  "registry",
  "internal",
  "www",
];

const SERVER_NAME_RE = /^[a-z0-9-]{3,32}$/;

export type ServerNameIssue = "shape" | "reserved";

/** serverNameIssue says why the API would refuse this name, or null if it takes it. */
export function serverNameIssue(name: string): ServerNameIssue | null {
  if (!SERVER_NAME_RE.test(name) || name.startsWith("-") || name.endsWith("-")) return "shape";
  if (RESERVED_SERVER_NAMES.includes(name)) return "reserved";
  return null;
}
