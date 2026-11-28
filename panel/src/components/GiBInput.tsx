import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

const SIZE_WITH_UNIT_RE = /^(\d+(?:\.\d+)?)\s*(gib|gi|gb|g|mib|mi|mb|m)?$/i;
const MIB_UNITS = new Set(["mib", "mi", "mb", "m"]);

export function hasExplicitUnit(s: string): boolean {
  const m = SIZE_WITH_UNIT_RE.exec(s.trim());
  return Boolean(m?.[2]);
}

// Bare numbers default to GiB; GiB/GB/G map to GiB and MiB/MB/M map to MiB.
export function parseGiB(s: string): number | null {
  const m = SIZE_WITH_UNIT_RE.exec(s.trim());
  if (!m) return null;
  const n = Number(m[1]);
  if (!Number.isFinite(n)) return null;
  const unit = m[2]?.toLowerCase();
  if (unit && MIB_UNITS.has(unit)) return n / 1024;
  return n;
}

export function isWholeGiB(n: number): boolean {
  return Math.abs(n - Math.round(n)) < 1e-9;
}

export function gibQuantity(n: number): string {
  return isWholeGiB(n) ? `${Math.round(n)}Gi` : `${Math.round(n * 1024)}Mi`;
}

export function quantityGiB(q: string | undefined): string {
  const m = /^(\d+(?:\.\d+)?)(GiB|Gi|GB|G|MiB|Mi|MB|M)$/i.exec(q?.trim() ?? "");
  if (!m) return "";
  const n = Number(m[1]);
  const unit = m[2];
  const lower = unit.toLowerCase();
  const gib =
    lower === "gi" || lower === "gib"
      ? n
      : lower === "mi" || lower === "mib"
        ? n / 1024
        : lower === "g" || lower === "gb"
          ? (n * 1e9) / 2 ** 30
          : (n * 1e6) / 2 ** 30;
  return String(Math.round(gib * 100) / 100);
}

export function GiBInput({
  id,
  value,
  onChange,
  invalid,
  describedBy,
  disabled,
}: {
  id: string;
  value: string;
  onChange: (value: string) => void;
  invalid?: boolean;
  describedBy?: string;
  disabled?: boolean;
}) {
  const showSuffix = !hasExplicitUnit(value);
  return (
    <div className="relative">
      <Input
        id={id}
        inputMode="text"
        autoComplete="off"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        aria-invalid={invalid || undefined}
        aria-describedby={describedBy}
        className={cn("tabular-nums", showSuffix && "pr-12")}
      />
      {showSuffix && (
        <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-sm text-muted-foreground">
          GiB
        </span>
      )}
    </div>
  );
}
