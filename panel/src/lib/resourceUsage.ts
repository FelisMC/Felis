import { formatBytes } from "./format";

export function cpuUsage(milli: number | undefined, limit?: string): string {
  if (milli === undefined) return "—";
  const max = limit?.endsWith("m") ? Number(limit.slice(0, -1)) : Number(limit) * 1000;
  return max > 0 ? `${(milli / max * 100).toFixed(1)}%` : `${milli} mCPU`;
}

export function memoryLimit(limit?: string): number | null {
  const numeric = Number(limit);
  if (Number.isFinite(numeric) && numeric > 0) return numeric;
  const m = /^(\d+(?:\.\d+)?)([KMGTPE]i?|k)?$/.exec(limit ?? "");
  if (!m) return null;
  const unit = m[2] ?? "";
  const power = "KMGTPE".indexOf(unit[0]?.toUpperCase() ?? "") + 1;
  return Number(m[1]) * (unit ? (unit.endsWith("i") ? 1024 : 1000) ** power : 1);
}

export function memoryUsage(bytes: number | undefined, limit?: string): string {
  if (bytes === undefined) return "—";
  const max = memoryLimit(limit);
  return max ? `${formatBytes(bytes)} / ${formatBytes(max)}` : formatBytes(bytes);
}
