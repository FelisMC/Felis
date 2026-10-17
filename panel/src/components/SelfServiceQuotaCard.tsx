import { useEffect, useState } from "react";
import { Loader2, Save, ServerCog } from "lucide-react";
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
import { useAsync, useRefreshSignal, useUnsavedGuard } from "@/lib/hooks";
import type { SelfServiceQuotaSettings } from "@/lib/types";

/** The caps as typed: whole servers, GB of memory and storage, CPU cores. An
 *  empty field is unlimited. */
interface Draft {
  enabled: boolean;
  servers: string;
  memoryGB: string;
  cpuCores: string;
  storageGB: string;
}

/** showCap is a cap as the box shows it: empty for unlimited, else in units of per. */
export const showCap = (v: number | null | undefined, per = 1) => (v == null ? "" : String(v / per));

function draftOf(q: SelfServiceQuotaSettings): Draft {
  return {
    enabled: q.enabled,
    servers: showCap(q.max_servers),
    memoryGB: showCap(q.max_memory_mb, 1024),
    cpuCores: showCap(q.max_cpu_milli, 1000),
    storageGB: showCap(q.max_storage_gb),
  };
}

/** parseCap reads one cap: empty is unlimited (null); otherwise a non-negative
 *  number, in whole units of the cap's own unit after scaling. */
export function parseCap(v: string, scale = 1): number | null | undefined {
  const s = v.trim();
  if (s === "") return null;
  if (!/^\d+(\.\d+)?$/.test(s)) return undefined;
  const n = Math.round(Number(s) * scale);
  return Number.isSafeInteger(n) && n <= 2147483647 ? n : undefined;
}

// SelfServiceQuotaCard is the Owner's default quota: once on, every user without a
// quota of their own may create servers within it, and claims and resizes are held
// to it as well. Off, those users cannot create servers.
export function SelfServiceQuotaCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getSelfServiceQuota, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<SelfServiceQuotaSettings | null>(null);
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
  useRefreshSignal(query.reload, dirty);

  const caps = draft && {
    max_servers: parseCap(draft.servers),
    max_memory_mb: parseCap(draft.memoryGB, 1024),
    max_cpu_milli: parseCap(draft.cpuCores, 1000),
    max_storage_gb: parseCap(draft.storageGB),
  };
  const valid = !!caps && Object.values(caps).every((v) => v !== undefined) && (draft?.servers.trim() === "" || /^\d+$/.test(draft!.servers.trim()));

  function set<K extends keyof Draft>(key: K, value: Draft[K]) {
    setDraft((d) => d && { ...d, [key]: value });
    setMessage(null);
  }
  async function save() {
    if (!saved || !draft || !caps || !dirty || !valid || saving) return;
    setSaving(true);
    setMessage(null);
    try {
      const next = await reauth.guard(() =>
        api.setSelfServiceQuota({
          enabled: draft.enabled,
          max_servers: caps.max_servers as number | null,
          max_memory_mb: caps.max_memory_mb as number | null,
          max_cpu_milli: caps.max_cpu_milli as number | null,
          max_storage_gb: caps.max_storage_gb as number | null,
          revision: saved.revision,
        }),
      );
      setSaved(next);
      setDraft(draftOf(next));
      setMessage({ kind: "success", text: t("self_service_saved") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setSaving(false);
    }
  }

  const disabled = !draft || saving;
  const field = (id: string, key: "servers" | "memoryGB" | "cpuCores" | "storageGB", label: string) => (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        inputMode="decimal"
        placeholder={t("self_service_unlimited")}
        value={draft?.[key] ?? ""}
        disabled={disabled || !draft?.enabled}
        onChange={(e) => set(key, e.target.value)}
      />
    </div>
  );
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><ServerCog className="h-4 w-4" /></span>
        <CardTitle>{t("self_service_title")}</CardTitle>
        {saved && <Badge variant={saved.enabled ? "default" : "muted"}>{t(saved.enabled ? "self_service_on" : "self_service_off")}</Badge>}
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("self_service_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("self_service_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        <label htmlFor="self-service-enabled" className="flex min-h-12 cursor-pointer items-center justify-between gap-4 text-sm">
          <span className="space-y-1"><span className="block">{t("self_service_enabled")}</span><span className="block text-xs leading-relaxed text-muted-foreground">{t("self_service_enabled_hint")}</span></span>
          <Switch id="self-service-enabled" checked={draft?.enabled ?? false} disabled={disabled} onCheckedChange={(value) => set("enabled", value)} />
        </label>
        <div className="grid gap-5 border-t border-border/60 pt-4 md:grid-cols-2">
          {field("self-service-servers", "servers", t("self_service_servers"))}
          {field("self-service-memory", "memoryGB", t("self_service_memory"))}
          {field("self-service-cpu", "cpuCores", t("self_service_cpu"))}
          {field("self-service-storage", "storageGB", t("self_service_storage"))}
        </div>
        <p className="text-xs leading-relaxed text-muted-foreground">{t("self_service_caps_hint")}</p>
        {draft && !valid && <MessageLine kind="error" message={t("self_service_invalid")} />}
        {message && <MessageLine kind={message.kind} message={message.text} />}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t(dirty ? "failover_unsaved" : "failover_live")}</p>
        <div className="flex flex-wrap gap-2">
          {dirty && <Button variant="outline" disabled={saving} onClick={() => { if (saved) setDraft(draftOf(saved)); setMessage(null); }}>{t("platform_discard")}</Button>}
          <Button disabled={disabled || !valid || !dirty} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t("self_service_save")}</Button>
        </div>
      </CardFooter>
      {reauth.dialog}
    </Card>
  );
}
