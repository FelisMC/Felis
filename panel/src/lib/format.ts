
// Use binary units; invalid byte counts display an em dash.
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes < 1024) return `${Math.round(bytes)} B`;
  const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
  let n = bytes / 1024;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n < 10 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}

// Time buckets for formatRelative, smallest first. Each entry: use `unit` (dividing
// the delta by `div` seconds) while the absolute delta is under `limit` seconds.
const DIVISIONS: { limit: number; div: number; unit: Intl.RelativeTimeFormatUnit }[] = [
  { limit: 60, div: 1, unit: "second" },
  { limit: 3600, div: 60, unit: "minute" },
  { limit: 86400, div: 3600, unit: "hour" },
  { limit: 2592000, div: 86400, unit: "day" },
  { limit: 31536000, div: 2592000, unit: "month" },
  { limit: Infinity, div: 31536000, unit: "year" },
];

// Inject time and locale for deterministic formatting; invalid timestamps return an empty string.
export function formatRelative(iso: string, now: number, locale: string): string {
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then)) return "";
  const deltaSec = (then - now) / 1000; // negative = in the past
  const abs = Math.abs(deltaSec);
  const rtf = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
  for (const { limit, div, unit } of DIVISIONS) {
    if (abs < limit) return rtf.format(Math.round(deltaSec / div), unit);
  }
  return "";
}

/** formatAbsolute renders an ISO timestamp as a full localized date-time, for the
 *  `title` tooltip behind a relative label. Empty string on an unparseable input. */
export function formatAbsolute(iso: string, locale: string): string {
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return "";
  return d.toLocaleString(locale);
}

export function isExpired(iso: string, now: number): boolean {
  const t = new Date(iso).getTime();
  return Number.isFinite(t) && t <= now;
}

// Separate tag and pinned digest; unpinned references have no short digest.
export function splitImageRef(ref: string): { tag: string; digest: string; short: string } {
  const at = ref.indexOf("@");
  if (at < 0) return { tag: ref, digest: "", short: "" };
  const digest = ref.slice(at + 1);
  return { tag: ref.slice(0, at), digest, short: digest.replace(/^sha256:/, "").slice(0, 12) };
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** formatDuration names a span of seconds in the two largest units, such as "9 h 14 min". */
export function formatDuration(seconds: number, t: Translate): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return t("common:duration_hours", { h, m });
  if (m > 0) return t("common:duration_minutes", { m, s });
  return t("common:duration_seconds", { s });
}
