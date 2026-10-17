import { Input } from "@/components/ui/input";

/** parseGiB reads a typed size in GiB: a whole or decimal number, else null. */
export function parseGiB(s: string): number | null {
  const v = s.trim();
  return /^\d+(\.\d+)?$/.test(v) ? Number(v) : null;
}

/** gibQuantity is the Kubernetes quantity of n GiB: whole numbers in Gi, the
 *  rest in whole Mi (1.5 → 1536Mi), which the API takes as either. */
export function gibQuantity(n: number): string {
  return Number.isInteger(n) ? `${n}Gi` : `${Math.round(n * 1024)}Mi`;
}

/** quantityGiB shows a server's quantity ("4Gi", "1536Mi", "2G") as GiB for
 *  the input, or "" when it is not one the panel reads. */
export function quantityGiB(q: string | undefined): string {
  const m = /^(\d+(?:\.\d+)?)(Gi|Mi|G|M)$/.exec(q?.trim() ?? "");
  if (!m) return "";
  const n = Number(m[1]);
  const gib = m[2] === "Gi" ? n : m[2] === "Mi" ? n / 1024 : m[2] === "G" ? (n * 1e9) / 2 ** 30 : (n * 1e6) / 2 ** 30;
  return String(Math.round(gib * 100) / 100);
}

/** GiBInput takes a size in GiB as text, with the unit beside the number. */
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
  return (
    <div className="relative">
      <Input
        id={id}
        inputMode="decimal"
        autoComplete="off"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        aria-invalid={invalid || undefined}
        aria-describedby={describedBy}
        className="pr-12 tabular-nums"
      />
      <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-sm text-muted-foreground">GiB</span>
    </div>
  );
}
