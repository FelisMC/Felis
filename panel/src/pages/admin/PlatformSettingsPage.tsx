import { Link } from "react-router-dom";
import { useEffect, useState } from "react";
import { Loader2, RefreshCw, Save, Settings } from "lucide-react";
import { useTranslation } from "react-i18next";
import { PlayerEntrySettings } from "@/components/PlayerEntrySettings";
import { NodeControlPanel } from "@/components/NodeControlPanel";
import { DistributedNodes } from "@/components/DistributedNodes";
import { Badge } from "@/components/ui/badge";
import { PageHeader } from "@/components/PageHeader";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useConfig, useUnsavedGuard } from "@/lib/hooks";
import type { WakePolicySettings } from "@/lib/types";

export function PlatformSettingsPage() {
  const { t } = useTranslation("admin");
  const config = useConfig();
  const query = useAsync(api.getWakePolicy, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<WakePolicySettings | null>(null);
  const [limit, setLimit] = useState("");
  const [cooldown, setCooldown] = useState("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const dirty = saved !== null && (limit !== String(saved.maxRunningServers) || cooldown !== String(saved.wakeCooldownSeconds));
  useUnsavedGuard(dirty);
  function load(policy: WakePolicySettings) {
    setSaved(policy);
    setLimit(String(policy.maxRunningServers));
    setCooldown(String(policy.wakeCooldownSeconds));
  }
  useEffect(() => {
    if (query.data) load(query.data);
  }, [query.data]);
  const busy = query.loading || saving;
  const valid = /^\d+$/.test(limit) && Number(limit) <= 10000 && /^\d+$/.test(cooldown) && Number(cooldown) <= 3600;
  async function save() {
    if (!saved || !dirty || !valid || busy) return;
    setSaving(true);
    setMessage(null);
    try {
      const policy = await reauth.guard(() => api.setWakePolicy({ maxRunningServers: Number(limit), wakeCooldownSeconds: Number(cooldown), revision: saved.revision }));
      load(policy);
      setMessage({ kind: "success", text: t("platform_saved") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally { setSaving(false); }
  }
  return <div className="space-y-6">
    <PageHeader icon={Settings} title={t("platform_title")} subtitle={t("platform_subtitle")} actions={<Button variant="outline" size="sm" disabled={dirty || busy} onClick={query.reload}><RefreshCw />{t("platform_reload")}</Button>} />
    <PlayerEntrySettings />
    <Card><CardHeader><CardTitle>{t("platform_wake_title")}</CardTitle></CardHeader><CardContent className="space-y-6">
      {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("platform_loading")}</p>}
      <div className="grid gap-6 md:grid-cols-2">
        <div className="space-y-2"><Label htmlFor="running-limit">{t("platform_limit")}</Label><Input id="running-limit" type="number" min={0} max={10000} step={1} value={limit} disabled={!saved || busy} onChange={(e) => { setLimit(e.target.value); setMessage(null); }} /><p className="text-xs leading-relaxed text-muted-foreground">{t("platform_limit_hint")}</p></div>
        <div className="space-y-2"><Label htmlFor="wake-cooldown">{t("platform_cooldown")}</Label><Input id="wake-cooldown" type="number" min={0} max={3600} step={1} value={cooldown} disabled={!saved || busy} onChange={(e) => { setCooldown(e.target.value); setMessage(null); }} /><p className="text-xs leading-relaxed text-muted-foreground">{t("platform_cooldown_hint")}</p></div>
      </div>
      <p className="text-sm leading-relaxed text-muted-foreground">{t("platform_scope")}</p>
    </CardContent></Card>
    {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
    {message && <MessageLine kind={message.kind} message={message.text} />}
    <Card><CardFooter className="border-t-0">
      <p className="text-xs text-muted-foreground">{t(dirty ? "platform_unsaved" : "platform_live")}</p>
      <div className="flex gap-2">
        {dirty && <Button variant="outline" disabled={busy} onClick={() => { if (saved) load(saved); setMessage(null); }}>{t("platform_discard")}</Button>}
        <Button disabled={!dirty || !valid || busy} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t("platform_save")}</Button>
      </div>
    </CardFooter></Card>
    <Card>
      <CardHeader><CardTitle>{t("platform_distribution_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-4">
        <Badge variant={config?.distributed && !config.fallback ? "default" : "muted"}>
          {t(!config || config.fallback ? "platform_distribution_unknown" : config.distributed ? "platform_distribution_enabled" : "platform_distribution_disabled")}
        </Badge>
        <p className="text-sm leading-relaxed text-muted-foreground">{t("platform_distribution_architecture")}</p>
        <p className="text-sm leading-relaxed text-muted-foreground">{t("platform_distribution_management")}</p>
        <Button asChild variant="outline" size="sm"><Link to="/servers">{t("platform_distribution_servers")}</Link></Button>
      </CardContent>
    </Card>
    <NodeControlPanel distributed={!config || config.fallback ? null : !!config.distributed} />
    {config?.distributed && !config.fallback && <DistributedNodes />}
    {reauth.dialog}
  </div>;
}
