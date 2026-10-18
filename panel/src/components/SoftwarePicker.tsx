import { useEffect, useMemo, useState } from "react";
import { AlertTriangle, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { cn } from "@/lib/utils";
import type { SoftwareRequest, SoftwareType, SoftwareVersionInfo } from "@/lib/types";
import { CustomJarField, jarPathValid } from "@/components/CustomJarField";

export { jarPathValid };

/** The order the platform offers server software in (internal/software Types). */
export const SOFTWARE_TYPES: SoftwareType[] = ["paper", "purpur", "fabric", "quilt", "forge", "neoforge", "vanilla", "custom"];

/** Types that pick a build (Paper, Purpur) or a loader version on top of the game version. */
const WITH_BUILDS: SoftwareType[] = ["paper", "purpur", "fabric", "quilt", "forge", "neoforge"];
const LOADERS: SoftwareType[] = ["fabric", "quilt", "forge", "neoforge"];
const RUNTIMES = [8, 17, 21, 25] as const;
type Runtime = (typeof RUNTIMES)[number];

export interface SoftwareChoice {
  type: SoftwareType | "";
  minecraftVersion: string;
  build: string;
  java: Runtime | 0;
  jar: string;
}

export const EMPTY_SOFTWARE: SoftwareChoice = { type: "", minecraftVersion: "", build: "", java: 0, jar: "" };

/** softwareReady reports whether a choice names everything a request needs. */
export function softwareReady(c: SoftwareChoice): boolean {
  if (!c.type) return false;
  if (c.type === "custom") return jarPathValid(c.jar.trim()) && c.java !== 0;
  return !!c.minecraftVersion && (!WITH_BUILDS.includes(c.type) || !!c.build) && c.java !== 0;
}

/** softwareRequest turns a ready choice into the request body. */
export function softwareRequest(c: SoftwareChoice): SoftwareRequest {
  const type = c.type as SoftwareType;
  if (type === "custom") return { type, jar: c.jar.trim(), java: c.java as Runtime };
  return { type, minecraftVersion: c.minecraftVersion, build: c.build || undefined, java: c.java as Runtime };
}

interface Props {
  value: SoftwareChoice;
  onChange: (next: SoftwareChoice) => void;
  idPrefix: string;
  disabled?: boolean;
  /** A custom jar to upload (CustomJarField): the dialog sends it once the
   *  server exists, or on save. Without onJarFile no upload is offered. */
  jarFile?: File | null;
  onJarFile?: (file: File | null) => void;
  /** The server whose files are listed for a jar already there. */
  jarServer?: string;
}

export function SoftwarePicker({ value, onChange, idPrefix, disabled = false, jarFile = null, onJarFile, jarServer }: Props) {
  const { t } = useTranslation("servers");
  const [unstable, setUnstable] = useState(false);
  const type = value.type;
  const versioned = !!type && type !== "custom";

  const versions = useAsync(() => (versioned ? api.softwareVersions(type) : Promise.resolve(null)), [type]);
  const detail = useAsync<SoftwareVersionInfo | null>(
    () => (versioned && value.minecraftVersion ? api.softwareVersion(type, value.minecraftVersion) : Promise.resolve(null)),
    [type, value.minecraftVersion],
  );

  const shown = useMemo(() => {
    const all = versions.data ?? [];
    return unstable ? all : all.filter((v) => v.stable || v.id === value.minecraftVersion);
  }, [versions.data, unstable, value.minecraftVersion]);

  // Once a version's details arrive, the build and Java it needs are filled in
  // unless the current choice still fits.
  const info = detail.data;
  useEffect(() => {
    if (!info || info.version !== value.minecraftVersion) return;
    let next = value;
    if (WITH_BUILDS.includes(value.type as SoftwareType) && !info.builds.some((b) => b.id === value.build)) {
      const rec = info.builds.find((b) => b.recommended) ?? info.builds[0];
      next = { ...next, build: rec?.id ?? "" };
    }
    if (next.java === 0 || next.java < info.requiredJava) {
      next = { ...next, java: (info.java ?? 0) as Runtime | 0 };
    }
    if (next !== value) onChange(next);
  }, [info, value, onChange]);

  function pickType(next: SoftwareType) {
    if (next === value.type) return;
    onChange({ ...EMPTY_SOFTWARE, type: next, java: next === "custom" ? 21 : 0 });
  }

  const runtimes = RUNTIMES.filter((r) => type === "custom" || !info || r >= info.requiredJava);
  const buildLabel = LOADERS.includes(type as SoftwareType) ? t("software_build_loader") : t("software_build");

  return (
    <div className="grid gap-4">
      <div className="grid gap-2">
        <Label id={`${idPrefix}-type-label`}>{t("software_title")}</Label>
        <div role="radiogroup" aria-labelledby={`${idPrefix}-type-label`} className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          {SOFTWARE_TYPES.map((st) => {
            const checked = type === st;
            return (
              <button
                key={st}
                type="button"
                role="radio"
                aria-checked={checked}
                aria-labelledby={`${idPrefix}-type-${st}`}
                aria-describedby={`${idPrefix}-type-${st}-desc`}
                disabled={disabled}
                onClick={() => pickType(st)}
                className={cn(
                  "flex flex-col items-start gap-0.5 rounded-lg border p-2.5 text-left transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60",
                  checked ? "border-primary bg-primary/5 ring-1 ring-primary" : "border-border hover:border-primary/40 hover:bg-accent",
                )}
              >
                <span id={`${idPrefix}-type-${st}`} className="text-sm font-medium">{t(`software_type_${st}`)}</span>
                <span id={`${idPrefix}-type-${st}-desc`} className="text-[11px] leading-snug text-muted-foreground">{t(`software_type_${st}_desc`)}</span>
              </button>
            );
          })}
        </div>
      </div>

      {versioned && (
        <div className="grid gap-2">
          <Label htmlFor={`${idPrefix}-version`}>{t("software_version")}</Label>
          <Select
            value={value.minecraftVersion}
            disabled={disabled || versions.loading || !!versions.error}
            onValueChange={(v) => onChange({ ...value, minecraftVersion: v, build: "", java: 0 })}
          >
            <SelectTrigger id={`${idPrefix}-version`}>
              <SelectValue placeholder={versions.loading ? t("software_version_loading") : t("software_version_choose")} />
            </SelectTrigger>
            <SelectContent>
              {shown.map((v) => (
                <SelectItem key={v.id} value={v.id}>
                  {v.stable ? v.id : t("software_version_unstable", { id: v.id })}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <label className="flex cursor-pointer items-center gap-2 text-xs text-muted-foreground">
            <Checkbox checked={unstable} disabled={disabled} onChange={(e) => setUnstable(e.target.checked)} />
            {t("software_show_unstable")}
          </label>
          {!!versions.error && (
            <div role="alert" className="flex items-center justify-between gap-3 text-xs text-destructive">
              <span>{t("software_version_failed", { reason: humanizeError(versions.error) })}</span>
              <Button variant="outline" size="sm" className="shrink-0" onClick={versions.reload}>
                {t("common:try_again")}
              </Button>
            </div>
          )}
        </div>
      )}

      {versioned && value.minecraftVersion && WITH_BUILDS.includes(type as SoftwareType) && (
        <div className="grid gap-2">
          <Label htmlFor={`${idPrefix}-build`}>{buildLabel}</Label>
          <Select value={value.build} disabled={disabled || !info} onValueChange={(v) => onChange({ ...value, build: v })}>
            <SelectTrigger id={`${idPrefix}-build`}>
              <SelectValue placeholder={detail.loading ? t("software_version_loading") : ""} />
            </SelectTrigger>
            <SelectContent>
              {(info?.builds ?? []).map((b) => (
                <SelectItem key={b.id} value={b.id}>
                  {b.recommended ? t("software_build_recommended", { id: b.id }) : b.stable ? b.id : t("software_build_unstable", { id: b.id })}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}

      {type === "custom" && (
        <CustomJarField
          idPrefix={idPrefix}
          path={value.jar}
          onPath={(jar) => onChange({ ...value, jar })}
          file={jarFile}
          onFile={onJarFile}
          server={jarServer}
          disabled={disabled}
        />
      )}

      {(type === "custom" || (versioned && info)) && (
        <div className="grid gap-2">
          <Label htmlFor={`${idPrefix}-java`}>{t("software_java")}</Label>
          <Select
            value={value.java ? String(value.java) : ""}
            disabled={disabled}
            onValueChange={(v) => onChange({ ...value, java: Number(v) as Runtime })}
          >
            <SelectTrigger id={`${idPrefix}-java`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {runtimes.map((r) => (
                <SelectItem key={r} value={String(r)}>
                  {info && r === info.java ? t("software_java_recommended", { version: r }) : t("software_java_option", { version: r })}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {info && <p className="text-xs text-muted-foreground">{t("software_java_requirement", { version: info.requiredJava })}</p>}
        </div>
      )}

      {versioned && info && (
        info.forwarding === "velocity" ? (
          <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
            <ShieldCheck className="mt-px h-3.5 w-3.5 shrink-0 text-emerald-600 dark:text-emerald-400" />
            {t("software_forwarding_velocity")}
          </p>
        ) : (
          <p role="note" className="flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 p-2.5 text-xs text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
            {t("software_forwarding_none")}
          </p>
        )
      )}
      {!!detail.error && <p role="alert" className="text-xs text-destructive">{humanizeError(detail.error)}</p>}
    </div>
  );
}

/** EulaCheckbox records the creator's acceptance of the Minecraft EULA, which a
 *  server built from server software runs under. */
export function EulaCheckbox({ checked, onChange, disabled }: { checked: boolean; onChange: (v: boolean) => void; disabled?: boolean }) {
  const { t } = useTranslation("servers");
  return (
    <label className="flex cursor-pointer items-start gap-2 text-sm">
      <Checkbox className="mt-0.5" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span>
        {t("software_eula")}{" "}
        <a href="https://aka.ms/MinecraftEULA" target="_blank" rel="noreferrer" className="text-primary underline-offset-2 hover:underline">
          {t("software_eula_link")}
        </a>
      </span>
    </label>
  );
}
