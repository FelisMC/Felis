import { useEffect, useState } from "react";
import { ChevronDown, ChevronUp, Loader2, Save, ServerCog } from "lucide-react";
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

interface Draft {
  enabled: boolean;
  servers: string;
  memoryGB: string;
  cpuCores: string;
  storageGB: string;
}

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

export function parseCap(v: string, scale = 1): number | null | undefined {
  const s = v.trim();
  if (s === "") return null;
  if (!/^\d+(\.\d+)?$/.test(s)) return undefined;
  const n = Math.round(Number(s) * scale);
  return Number.isSafeInteger(n) && n <= 2147483647 ? n : undefined;
}

// Default quota governs users without individual quotas, including create, claim, and resize admission.
export function SelfServiceQuotaCard({ collapsible = false }: { collapsible?: boolean }) {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getSelfServiceQuota, []);
  const reauth = useReauth();
  const [expanded, setExpanded] = useState(!collapsible);
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
    <div className="space-y-1.5">
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
    <Card className="overflow-hidden">
      <CardHeader className="flex-row items-center justify-between gap-3">
        <div className="flex min-w-0 flex-1 items-center gap-2.5">
          <span className="shrink-0 text-primary"><ServerCog className="h-3.5 w-3.5" /></span>
          <CardTitle className="shrink-0">{t("self_service_title")}</CardTitle>
          {saved && <Badge className="shrink-0" variant={saved.enabled ? "default" : "muted"}>{t(saved.enabled ? "self_service_on" : "self_service_off")}</Badge>}
          {collapsible && !expanded && (
            <span className="min-w-0 truncate text-xs text-muted-foreground">{t("self_service_description")}</span>
          )}
        </div>
        {collapsible && (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="shrink-0"
            aria-expanded={expanded}
            onClick={() => setExpanded((v) => !v)}
          >
            {expanded ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
          </Button>
        )}
      </CardHeader>
      {expanded && (
        <>
          <CardContent className="space-y-4">
            <p className="text-xs leading-relaxed text-muted-foreground">{t("self_service_description")}</p>
            {query.loading && <p role="status" className="flex items-center gap-2 text-xs text-muted-foreground"><Loader2 className="h-3.5 w-3.5 animate-spin" />{t("self_service_loading")}</p>}
            {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
            <label htmlFor="self-service-enabled" className="flex min-h-10 cursor-pointer items-center justify-between gap-4 text-xs">
              <span className="space-y-0.5"><span className="block font-medium text-foreground">{t("self_service_enabled")}</span><span className="block text-xs leading-relaxed text-muted-foreground">{t("self_service_enabled_hint")}</span></span>
              <Switch id="self-service-enabled" checked={draft?.enabled ?? false} disabled={disabled} onCheckedChange={(value) => set("enabled", value)} />
            </label>
            <div className="grid gap-4 border-t border-border/60 pt-3.5 md:grid-cols-2">
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
              {dirty && <Button variant="outline" size="sm" disabled={saving} onClick={() => { if (saved) setDraft(draftOf(saved)); setMessage(null); }}>{t("platform_discard")}</Button>}
              <Button size="sm" disabled={disabled || !valid || !dirty} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t("self_service_save")}</Button>
            </div>
          </CardFooter>
        </>
      )}
      {reauth.dialog}
    </Card>
  );
}
