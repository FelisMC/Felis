import { useEffect, useState } from "react";
import { Loader2, Save, ShieldAlert } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useUnsavedGuard } from "@/lib/hooks";
import type { FailoverPolicySettings } from "@/lib/types";

interface Draft {
  automatic: boolean;
  afterMinutes: string;
  replicaMinutes: string;
}

function draftOf(p: FailoverPolicySettings): Draft {
  return { automatic: p.automatic, afterMinutes: String(p.afterMinutes), replicaMinutes: String(p.replicaMinutes) };
}

/** validDraft mirrors the server's bounds: automatic failover waits 10–1440 minutes, and the
 *  replicas run every 15–1440 minutes or not at all (0). */
function validDraft(d: Draft): boolean {
  if (!/^\d+$/.test(d.afterMinutes) || !/^\d+$/.test(d.replicaMinutes)) return false;
  const after = Number(d.afterMinutes);
  const replica = Number(d.replicaMinutes);
  return after >= 10 && after <= 1440 && (replica === 0 || (replica >= 15 && replica <= 1440));
}

// FailoverPolicyCard is the Owner's failover setting for distributed mode: whether a lost
// worker is failed over without waiting for the Owner, after how long, and how often the
// worlds running on workers are copied live so a failover loses little.
export function FailoverPolicyCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getFailoverPolicy, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<FailoverPolicySettings | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  useEffect(() => {
    if (query.data) {
      setSaved(query.data);
      setDraft(draftOf(query.data));
    }
  }, [query.data]);
  const dirty = saved !== null && draft !== null && JSON.stringify(draft) !== JSON.stringify(draftOf(saved));
  useUnsavedGuard(dirty);
  const valid = !!draft && validDraft(draft);

  function set<K extends keyof Draft>(key: K, value: Draft[K]) {
    setDraft((d) => d && { ...d, [key]: value });
    setMessage(null);
  }
  async function save() {
    if (!saved || !draft || !dirty || !valid || saving) return;
    setSaving(true);
    setMessage(null);
    try {
      const next = await reauth.guard(() => api.setFailoverPolicy({
        automatic: draft.automatic, afterMinutes: Number(draft.afterMinutes), replicaMinutes: Number(draft.replicaMinutes), revision: saved.revision,
      }));
      setSaved(next);
      setDraft(draftOf(next));
      setMessage({ kind: "success", text: t("failover_saved") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setSaving(false);
    }
  }

  const disabled = !draft || saving;
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><ShieldAlert className="h-4 w-4" /></span>
        <CardTitle>{t("failover_title")}</CardTitle>
        {saved && <Badge variant={saved.automatic ? "default" : "muted"}>{t(saved.automatic ? "failover_mode_automatic" : "failover_mode_manual")}</Badge>}
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("failover_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("failover_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        <label htmlFor="failover-auto" className="flex min-h-12 cursor-pointer items-center justify-between gap-4 text-sm">
          <span className="space-y-1"><span className="block">{t("failover_auto")}</span><span className="block text-xs leading-relaxed text-muted-foreground">{t("failover_auto_hint")}</span></span>
          <Switch id="failover-auto" checked={draft?.automatic ?? false} disabled={disabled} onCheckedChange={(value) => set("automatic", value)} />
        </label>
        <div className="grid gap-5 border-t border-border/60 pt-4 md:grid-cols-2">
          <div className="space-y-2">
            <Label htmlFor="failover-after">{t("failover_after")}</Label>
            <Input id="failover-after" type="number" min={10} max={1440} step={1} value={draft?.afterMinutes ?? ""} disabled={disabled || !draft?.automatic} onChange={(e) => set("afterMinutes", e.target.value)} />
            <p className="text-xs leading-relaxed text-muted-foreground">{t("failover_after_hint")}</p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="failover-replica">{t("failover_replica")}</Label>
            <Input id="failover-replica" type="number" min={0} max={1440} step={1} value={draft?.replicaMinutes ?? ""} disabled={disabled} onChange={(e) => set("replicaMinutes", e.target.value)} />
            <p className="text-xs leading-relaxed text-muted-foreground">{t("failover_replica_hint")}</p>
          </div>
        </div>
        {draft && !valid && <MessageLine kind="error" message={t("failover_invalid")} />}
        {message && <MessageLine kind={message.kind} message={message.text} />}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t(dirty ? "failover_unsaved" : "failover_live")}</p>
        <div className="flex flex-wrap gap-2">
          {dirty && <Button variant="outline" disabled={saving} onClick={() => { if (saved) setDraft(draftOf(saved)); setMessage(null); }}>{t("platform_discard")}</Button>}
          <Button disabled={disabled || !valid || !dirty} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t("failover_save")}</Button>
        </div>
      </CardFooter>
      {reauth.dialog}
    </Card>
  );
}
