// Small pure formatters for human-facing sizes and times. Kept dependency-free and
// injectable (the caller passes `now` / `locale`) so they are deterministic under
// test rather than reading the wall clock or ambient locale themselves.

/** formatBytes renders a byte count in binary units (B / KiB / MiB / GiB…), the
 *  unit world archives are sized in. One decimal below 10 (1.4 GiB) and none above
 *  (140 MiB) — enough to tell backups apart without noise. A negative or non-finite
 *  input renders as an em dash rather than "NaN". */
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

/** formatRelative renders an ISO timestamp relative to `now` (ms epoch) — "3 days
 *  ago", "in 30 days" — localized via Intl.RelativeTimeFormat, so zh-CN reads
 *  "30 天后" / "3 天前" for free. `now` and `locale` are injected so the result is
 *  deterministic in tests. Returns "" for an unparseable input so a caller can fall
 *  back to nothing rather than surfacing "Invalid Date". */
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

/** isExpired reports whether an ISO retention deadline is at or before `now`. A
 *  present backup is normally still within retention (the reaper deletes expired
 *  ones), but the panel guards the edge so a just-expired row reads honestly rather
 *  than offering a restore that would 404. */
export function isExpired(iso: string, now: number): boolean {
  const t = new Date(iso).getTime();
  return Number.isFinite(t) && t <= now;
}

/** splitImageRef separates a server's image into the tag it was chosen by and the
 *  build it is pinned to. felis-api stores `name:tag@sha256:<hex>`; the digest is
 *  64 hex characters no one reads, so `short` keeps the first 12, the length
 *  `docker images` shows. A ref without a digest comes back with `short` empty. */
export function splitImageRef(ref: string): { tag: string; digest: string; short: string } {
  const at = ref.indexOf("@");
  if (at < 0) return { tag: ref, digest: "", short: "" };
  const digest = ref.slice(at + 1);
  return { tag: ref.slice(0, at), digest, short: digest.replace(/^sha256:/, "").slice(0, 12) };
}
