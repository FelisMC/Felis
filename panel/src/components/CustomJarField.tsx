import { useEffect, useRef, useState, type DragEvent } from "react";
import { FileArchive, Loader2, Upload, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { SegmentedControl } from "@/components/SegmentedControl";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import { cn } from "@/lib/utils";
import type { ServerFileEntry } from "@/lib/types";

/** jarPathValid mirrors the API's rule (software.ValidateJarPath): a relative
 *  path inside the server directory, ending in .jar, outside .felis. */
export function jarPathValid(p: string): boolean {
  if (!p || p.length > 255 || p.startsWith("/") || p.includes("\\") || !p.toLowerCase().endsWith(".jar")) return false;
  const parts = p.split("/");
  return parts.every((s) => s !== "" && s !== "." && s !== "..") && parts[0] !== ".felis";
}

type Mode = "existing" | "upload" | "path";

interface Props {
  idPrefix: string;
  /** The jar's path in the server's files, relative to its root. */
  path: string;
  onPath: (path: string) => void;
  /** A jar to upload (the dialog sends it once the server exists or on save);
   *  without onFile the field offers no upload. */
  file: File | null;
  onFile?: (file: File | null) => void;
  /** The server whose files hold the jar: its root is listed so a jar already
   *  there is picked rather than typed. A server still running cannot be read,
   *  and then only a path can be given. */
  server?: string;
  disabled?: boolean;
}

/** CustomJarField says which jar a custom server starts: one uploaded here, one
 *  already in the server's files, or a path typed by hand. */
export function CustomJarField({ idPrefix, path, onPath, file, onFile, server, disabled = false }: Props) {
  const { t } = useTranslation("servers");
  const listing = useAsync(() => (server ? api.listServerFiles(server, "") : Promise.resolve(null)), [server]);
  const jars: ServerFileEntry[] = (listing.data?.entries ?? []).filter(
    (e) => !e.is_dir && e.name.toLowerCase().endsWith(".jar"),
  );
  const readable = !!server && !listing.error;
  const canUpload = !!onFile && (!server || readable);
  const modes: Mode[] = [...(readable ? (["existing"] as const) : []), ...(canUpload ? (["upload"] as const) : []), "path"];
  const [chosen, setChosen] = useState<Mode>(server ? "existing" : "upload");
  const mode = modes.includes(chosen) ? chosen : modes[0];
  const [moving, setMoving] = useState(false);
  const [dragging, setDragging] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  // A lone jar in the server's root is the one meant.
  const lone = mode === "existing" && !path && jars.length === 1 ? jars[0].name : null;
  useEffect(() => {
    if (lone) onPath(lone);
  }, [lone, onPath]);

  function switchTo(next: Mode) {
    setChosen(next);
    setMoving(false);
    if (file) onFile?.(null);
    if (next === "upload" || (next === "existing" && !jars.some((j) => j.name === path))) onPath("");
  }
  function take(next: File | null) {
    if (!onFile) return;
    onFile(next);
    // The jar lands in the server's root under its own name unless another
    // place was given; removing it takes that path away too.
    if (next && (!path || path === file?.name)) onPath(next.name);
    if (!next && path === file?.name) onPath("");
  }
  function drag(e: DragEvent) {
    e.preventDefault();
    e.stopPropagation();
    if (disabled) return;
    setDragging(e.type === "dragenter" || e.type === "dragover");
    if (e.type === "drop") take(e.dataTransfer.files?.[0] ?? null);
  }

  const replaces = !!file && jars.some((j) => j.name === path);
  const pathInvalid = !!path && !jarPathValid(path.trim());

  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-sm font-medium leading-none">{t("software_jar_file")}</p>
        {modes.length > 1 && (
          <SegmentedControl
            label={t("software_jar_source")}
            value={mode}
            onChange={switchTo}
            segments={modes.map((key) => ({ key, label: t(`software_jar_mode_${key}`) }))}
            className="h-9"
          />
        )}
      </div>

      {server && listing.error != null && (
        <p className="text-xs text-muted-foreground">{t("software_jar_running")}</p>
      )}

      {mode === "existing" && (
        listing.loading && !listing.data ? (
          <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            {t("software_jar_reading")}
          </p>
        ) : jars.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border px-4 py-5 text-center text-sm text-muted-foreground">
            {t(canUpload ? "software_jar_none" : "software_jar_none_path")}
          </p>
        ) : (
          <div role="radiogroup" aria-label={t("software_jar_mode_existing")} className="grid gap-2">
            {jars.map((jar) => {
              const checked = path === jar.name;
              return (
                <button
                  key={jar.name}
                  type="button"
                  role="radio"
                  aria-checked={checked}
                  disabled={disabled}
                  onClick={() => onPath(jar.name)}
                  className={cn(
                    "flex items-center gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                    checked ? "border-primary bg-primary/5" : "border-border hover:bg-muted/40",
                  )}
                >
                  <FileArchive className={cn("h-5 w-5 shrink-0", checked ? "text-primary" : "text-muted-foreground")} />
                  <span className="min-w-0 flex-1 truncate text-sm font-medium">{jar.name}</span>
                  <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{formatBytes(jar.size)}</span>
                </button>
              );
            })}
          </div>
        )
      )}

      {mode === "upload" && (
        <>
          {/* The real file input stays in the tab order, visually hidden, so a
              keyboard reaches the picker; the drop zone shows its focus ring. */}
          <input
            ref={input}
            type="file"
            accept=".jar,application/java-archive"
            aria-label={t("software_jar_file")}
            className="peer sr-only"
            disabled={disabled}
            onChange={(e) => {
              const next = e.target.files?.[0] ?? null;
              e.target.value = "";
              if (next) take(next);
            }}
          />
          {file ? (
            <div className="grid gap-2">
              <div className="flex items-center gap-3 rounded-lg border border-border bg-muted/30 px-3 py-2.5">
                <FileArchive className="h-8 w-8 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium" title={file.name}>{file.name}</p>
                  <p className="truncate text-xs text-muted-foreground">
                    {formatBytes(file.size)} · {t("software_jar_upload_to", { path: path || file.name })}
                  </p>
                </div>
                <Button type="button" variant="outline" size="sm" disabled={disabled} onClick={() => input.current?.click()}>
                  {t("software_jar_file_change")}
                </Button>
                <Button type="button" variant="ghost" size="icon" className="h-8 w-8 shrink-0" disabled={disabled}
                  aria-label={t("software_jar_file_clear")} onClick={() => take(null)}>
                  <X className="h-4 w-4" />
                </Button>
              </div>
              {replaces && <p className="text-xs text-amber-700 dark:text-amber-300">{t("software_jar_replaces")}</p>}
              {moving ? (
                <JarPath idPrefix={idPrefix} path={path} onPath={onPath} invalid={pathInvalid} disabled={disabled} hint={t("software_jar_move_hint")} />
              ) : (
                <button type="button" disabled={disabled} onClick={() => setMoving(true)}
                  className="justify-self-start text-xs font-medium text-primary underline-offset-4 hover:underline disabled:opacity-50">
                  {t("software_jar_move")}
                </button>
              )}
            </div>
          ) : (
            <div
              onClick={() => !disabled && input.current?.click()}
              onDragEnter={drag}
              onDragOver={drag}
              onDragLeave={drag}
              onDrop={drag}
              className={cn(
                "flex min-h-[120px] cursor-pointer flex-col items-center justify-center gap-1.5 rounded-lg border border-dashed px-4 py-6 text-center transition-colors",
                "peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-focus-visible:ring-offset-2 peer-focus-visible:ring-offset-background",
                dragging ? "border-primary bg-primary/5" : "border-border hover:bg-muted/30",
                disabled && "cursor-not-allowed opacity-50",
              )}
            >
              <Upload className="h-6 w-6 text-muted-foreground" />
              <p className="text-sm font-medium">{t("software_jar_drop")}</p>
              <p className="text-xs text-muted-foreground">{t(server ? "software_jar_drop_hint_save" : "software_jar_drop_hint_create")}</p>
            </div>
          )}
        </>
      )}

      {mode === "path" && (
        <JarPath idPrefix={idPrefix} path={path} onPath={onPath} invalid={pathInvalid} disabled={disabled} hint={t("software_jar_path_hint")} />
      )}

      <p className="text-xs text-muted-foreground">{t("software_identity_none_custom")}</p>
    </div>
  );
}

function JarPath({ idPrefix, path, onPath, invalid, disabled, hint }: {
  idPrefix: string;
  path: string;
  onPath: (path: string) => void;
  invalid: boolean;
  disabled: boolean;
  hint: string;
}) {
  const { t } = useTranslation("servers");
  return (
    <div className="grid gap-2">
      <Label htmlFor={`${idPrefix}-jar`}>{t("software_jar")}</Label>
      <Input
        id={`${idPrefix}-jar`}
        placeholder={t("software_jar_placeholder")}
        value={path}
        disabled={disabled}
        onChange={(e) => onPath(e.target.value)}
        aria-invalid={invalid || undefined}
        aria-describedby={`${idPrefix}-jar-hint`}
      />
      <p id={`${idPrefix}-jar-hint`} className={cn("text-xs", invalid ? "text-destructive" : "text-muted-foreground")}>
        {invalid ? t("software_jar_invalid") : hint}
      </p>
    </div>
  );
}
