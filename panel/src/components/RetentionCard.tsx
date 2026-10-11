import { useEffect, useState } from "react";
import { Archive, Loader2, RotateCcw, Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { InlineConfirm } from "@/components/InlineConfirm";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useUnsavedGuard } from "@/lib/hooks";
import type { ArchivePolicy, ArchivePolicySettings } from "@/lib/types";

type Key = keyof ArchivePolicy;

// Each field with the bounds the server enforces; allowZero admits 0 below min (off, none
// or forever).
const FIELDS: { key: Key; min: number; max: number; allowZero?: boolean }[] = [
  { key: "manualKeep", min: 1, max: 100 },
  { key: "manualRetentionDays", min: 1, max: 3650 },
  { key: "manualCooldownMinutes", min: 0, max: 1440 },
  { key: "scheduledEveryHours", min: 0, max: 720 },
  { key: "scheduledKeep", min: 1, max: 100 },
  { key: "scheduledRetentionDays", min: 1, max: 3650 },
  { key: "releasedRetentionDays", min: 1, max: 3650 },
  { key: "maxStoreGiB", min: 0, max: 1 << 20 },
  { key: "auditRetentionDays", min: 30, max: 3650, allowZero: true },
];

const GROUPS: { title: string; keys: Key[] }[] = [
  { title: "retention_group_manual", keys: ["manualKeep", "manualRetentionDays", "manualCooldownMinutes"] },
  { title: "retention_group_scheduled", keys: ["scheduledEveryHours", "scheduledKeep", "scheduledRetentionDays"] },
  { title: "retention_group_store", keys: ["releasedRetentionDays", "maxStoreGiB", "auditRetentionDays"] },
];

type Draft = Record<Key, string>;

function draftOf(p: ArchivePolicy): Draft {
  return Object.fromEntries(FIELDS.map((f) => [f.key, String(p[f.key])])) as Draft;
}

function policyOf(d: Draft): ArchivePolicy {
  const out = {} as ArchivePolicy;
  for (const f of FIELDS) out[f.key] = Number(d[f.key]);
  return out;
}

function fieldValid(key: Key, value: string): boolean {
  const f = FIELDS.find((x) => x.key === key)!;
  if (!/^\d+$/.test(value)) return false;
  const n = Number(value);
  return (n >= f.min && n <= f.max) || (!!f.allowZero && n === 0);
}

// RetentionCard is the Owner's backup and audit retention: how many backups of each kind a
// server keeps and for how long, how often the scheduled restore points come, the backup
// store's cap and how long audit rows stay. It overrides felis.toml's [archive] and
// [audit]; clearing it hands them back.
export function RetentionCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getArchivePolicy, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<ArchivePolicySettings | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [busy, setBusy] = useState<"save" | "clear" | null>(null);
  const [confirmClear, setConfirmClear] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  useEffect(() => {
    if (query.data) {
      setSaved(query.data);
      setDraft(draftOf(query.data));
    }
  }, [query.data]);
  const dirty = saved !== null && draft !== null && JSON.stringify(draft) !== JSON.stringify(draftOf(saved));
  useUnsavedGuard(dirty);
  const invalid = draft ? FIELDS.filter((f) => !fieldValid(f.key, draft[f.key])).map((f) => f.key) : [];

  async function run(kind: "save" | "clear") {
    if (!saved || !draft || busy) return;
    setBusy(kind);
    setMessage(null);
    try {
      const next = await reauth.guard(() => kind === "save"
        ? api.setArchivePolicy({ ...policyOf(draft), revision: saved.revision })
        : api.clearArchivePolicy());
      setSaved(next);
      setDraft(draftOf(next));
      setConfirmClear(false);
      setMessage({ kind: "success", text: t(kind === "save" ? "retention_saved" : "retention_cleared") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setBusy(null);
    }
  }

  const disabled = !draft || busy !== null;
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><Archive className="h-4 w-4" /></span>
        <CardTitle>{t("retention_title")}</CardTitle>
        {saved && <Badge variant={saved.source === "panel" ? "default" : "muted"}>{t(`retention_source_${saved.source}`)}</Badge>}
      </CardHeader>
      <CardContent className="space-y-6">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("retention_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("retention_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        {GROUPS.map((g) => (
          <fieldset key={g.title} className="space-y-3 border-t border-border/60 pt-4 first-of-type:border-t-0 first-of-type:pt-0">
            <legend className="text-sm font-medium">{t(g.title)}</legend>
            <div className="grid gap-5 md:grid-cols-3">
              {g.keys.map((key) => {
                const f = FIELDS.find((x) => x.key === key)!;
                const bad = invalid.includes(key);
                return (
                  <div key={key} className="space-y-2">
                    <Label htmlFor={`retention-${key}`}>{t(`retention_${key}`)}</Label>
                    <Input id={`retention-${key}`} type="number" min={f.allowZero ? 0 : f.min} max={f.max} step={1} value={draft?.[key] ?? ""} disabled={disabled} aria-invalid={bad}
                      onChange={(e) => { const value = e.target.value; setDraft((d) => d && { ...d, [key]: value }); setMessage(null); }} />
                    <p className={bad ? "text-xs leading-relaxed text-destructive" : "text-xs leading-relaxed text-muted-foreground"}>{t(`retention_${key}_hint`)}</p>
                  </div>
                );
              })}
            </div>
          </fieldset>
        ))}
        {message && <MessageLine kind={message.kind} message={message.text} />}
        {confirmClear && (
          <div className="space-y-2 rounded-md border p-3">
            <p className="text-sm text-muted-foreground">{t("retention_clear_confirm")}</p>
            <InlineConfirm open confirming={busy === "clear"} onConfirm={() => void run("clear")} onCancel={() => setConfirmClear(false)} confirmLabel={t("retention_clear")} cancelLabel={t("common:cancel")} className="flex justify-end gap-2" />
          </div>
        )}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t(dirty ? "retention_unsaved" : "retention_live")}</p>
        <div className="flex flex-wrap gap-2">
          {saved?.source === "panel" && !confirmClear && !dirty && (
            <Button variant="outline" disabled={disabled} onClick={() => setConfirmClear(true)}><RotateCcw />{t("retention_clear")}</Button>
          )}
          {dirty && <Button variant="outline" disabled={disabled} onClick={() => { if (saved) setDraft(draftOf(saved)); setMessage(null); }}>{t("platform_discard")}</Button>}
          <Button disabled={disabled || !dirty || invalid.length > 0} onClick={() => void run("save")}>{busy === "save" ? <Loader2 className="animate-spin" /> : <Save />}{t("retention_save")}</Button>
        </div>
      </CardFooter>
      {reauth.dialog}
    </Card>
  );
}
