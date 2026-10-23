import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Loader2, RefreshCw, Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { InlineConfirm } from "@/components/InlineConfirm";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { useUnsavedGuard } from "@/lib/hooks";
import type { PaperGlobalConfig, PaperGlobalValues } from "@/lib/types";
import { ConfigField } from "./ConfigField";
import { configProblem, configScalar, type ConfigFieldSpec } from "./configFields";
import { PAPER_GLOBAL_FIELDS } from "./paperGlobalFields";

const fieldKey = (field: ConfigFieldSpec) => field.path.join(".") as keyof PaperGlobalValues;

export function PaperGlobalConfigDialog({ name, onClose, onSaved, disabled = false, worldConfirmation, needsWorldConfirmation, beforeSave }: {
  name: string;
  onClose: () => void;
  onSaved: () => void;
  disabled?: boolean;
  worldConfirmation?: ReactNode;
  needsWorldConfirmation: boolean;
  beforeSave: () => void;
}) {
  const { t } = useTranslation("files");
  const [config, setConfig] = useState<PaperGlobalConfig | null>(null);
  const [changes, setChanges] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<"read" | "save" | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [conflict, setConflict] = useState(false);
  const [discard, setDiscard] = useState(false);
  const generation = useRef(0);
  const inFlight = useRef(false);
  const fields = PAPER_GLOBAL_FIELDS.filter((field) => {
    const value = config?.values[fieldKey(field)];
    if (field.type === "boolean") return typeof value === "boolean";
    if (field.type === "text") return typeof value === "string";
    return typeof value === "number" && Number.isFinite(value) && (field.type !== "integer" || Number.isSafeInteger(value));
  });
  const errors = new Map(fields.flatMap((field) => {
    const key = fieldKey(field);
    const problem = changes[key] !== undefined ? configProblem(field, changes[key]) : undefined;
    return problem ? [[key, problem] as const] : [];
  }));
  const pending: [keyof PaperGlobalValues, string | number | boolean][] = [];
  for (const field of fields) {
    const key = fieldKey(field);
    if (changes[key] === undefined || errors.has(key)) continue;
    const scalar = configScalar(field, changes[key]);
    const next = typeof scalar === "bigint" ? Number(scalar) : scalar;
    if (!Object.is(next, config?.values[key])) pending.push([key, next]);
  }
  const patch = Object.fromEntries(pending) as PaperGlobalValues;
  const dirty = errors.size > 0 || Object.keys(patch).length > 0;
  useUnsavedGuard(dirty);

  const read = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    const ticket = ++generation.current;
    setBusy("read");
    setError(null);
    try {
      const latest = await api.serverPaperGlobal(name);
      if (ticket !== generation.current) return;
      setConfig(latest);
      setChanges({});
      setConflict(false);
      setDiscard(false);
    } catch (reason) {
      if (ticket === generation.current) setError(reason);
    } finally {
      if (ticket === generation.current) { inFlight.current = false; setBusy(null); }
    }
  }, [name]);
  useEffect(() => {
    void read();
    return () => { generation.current += 1; inFlight.current = false; };
  }, [read]);

  function close() {
    if (inFlight.current) return;
    if (dirty) setDiscard(true);
    else onClose();
  }

  async function save(overwrite = false) {
    if (!config || inFlight.current || disabled || needsWorldConfirmation || !dirty || errors.size > 0 || (conflict && !overwrite)) return;
    inFlight.current = true;
    const ticket = ++generation.current;
    setBusy("save");
    setError(null);
    try {
      beforeSave();
      const sha = overwrite ? (await api.serverPaperGlobal(name)).sha256 : config.sha256;
      await api.patchServerPaperGlobal(name, patch, sha);
      if (ticket === generation.current) onSaved();
    } catch (reason) {
      if (ticket !== generation.current) return;
      if ((reason as { code?: string }).code === "file_changed") setConflict(true);
      else setError(reason);
    } finally {
      if (ticket === generation.current) { inFlight.current = false; setBusy(null); }
    }
  }

  return <Dialog open onOpenChange={(open) => { if (!open) close(); }}>
    <DialogContent className="max-w-4xl" onKeyDown={(event) => {
      if (event.defaultPrevented || !(event.ctrlKey || event.metaKey) || event.key.toLowerCase() !== "s") return;
      event.preventDefault();
      void save();
    }}>
      <DialogHeader><DialogTitle>{t("paper_global_title")}</DialogTitle></DialogHeader>
      <code className="break-all text-xs text-muted-foreground">config/paper-global.yml</code>
      <p className="text-sm leading-relaxed text-muted-foreground">{t("paper_global_managed_hint")}</p>
      {disabled && <p role="status" className="text-sm text-muted-foreground">{t("paper_global_unavailable")}</p>}
      {worldConfirmation}
      {conflict && <div role="alert" className="grid gap-3 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-sm">
        <p className="font-medium">{t("conflict_title")}</p><p>{t("paper_global_conflict")}</p>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="outline" disabled={busy !== null || disabled} onClick={() => void read()}><RefreshCw className="h-4 w-4" />{t("conflict_reload")}</Button>
          <Button size="sm" variant="destructive" disabled={busy !== null || disabled || needsWorldConfirmation || errors.size > 0 || !dirty} onClick={() => void save(true)}>
            <Save className="h-4 w-4" />{t("conflict_overwrite")}
          </Button>
        </div>
      </div>}
      {error !== null && <MessageLine kind="error" message={humanizeError(error)} />}
      {!config ? busy ? (
        <p role="status" className="flex items-center justify-center gap-2 py-8 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("opening_file")}</p>
      ) : (
        <Button variant="outline" disabled={disabled} onClick={() => void read()}>{t("common:retry")}</Button>
      ) : <div className="max-h-[50vh] min-w-0 space-y-4 overflow-y-auto rounded-md border border-border p-3 sm:p-4">
        <p className="text-xs leading-relaxed text-muted-foreground">{t("paper_global_form_hint")}</p>
        {fields.length === 0 && <p className="text-sm text-muted-foreground">{t("paper_global_no_fields")}</p>}
        <fieldset disabled={busy !== null || disabled} className="grid min-w-0 gap-4 sm:grid-cols-2">
          {fields.map((field) => {
            const key = fieldKey(field);
            return <ConfigField key={key} field={field} value={changes[key] ?? String(config.values[key])} problem={errors.get(key)} disabled={busy !== null || disabled}
              onChange={(value) => {
                if (inFlight.current || disabled) return;
                setChanges((previous) => {
                  const next = { ...previous };
                  if (value === String(config.values[key])) delete next[key];
                  else next[key] = value;
                  return next;
                });
              }} />;
          })}
        </fieldset>
      </div>}
      <DialogFooter className="flex-wrap gap-2">
        {discard ? <div role="alert" className="flex flex-wrap items-center justify-end gap-2">
          <span className="text-sm font-medium">{t("discard_prompt")}</span>
          <InlineConfirm open confirming={false} onConfirm={onClose} onCancel={() => setDiscard(false)} confirmLabel={t("discard")} cancelLabel={t("keep_editing")} />
        </div> : <>
          <Button variant="outline" disabled={busy !== null} onClick={close}>{t("common:cancel")}</Button>
          <Button disabled={busy !== null || disabled || needsWorldConfirmation || conflict || !dirty || errors.size > 0} onClick={() => void save()}>
            {busy === "save" ? <Loader2 className="h-4 w-4 animate-spin" /> : <Save className="h-4 w-4" />}{busy === "save" ? t("saving") : t("save")}
          </Button>
        </>}
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}
