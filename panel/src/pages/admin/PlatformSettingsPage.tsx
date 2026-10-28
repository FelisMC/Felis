import { Link, useSearchParams } from "react-router-dom";
import { useEffect, useState } from "react";
import { BellRing, Loader2, Network, Palette, RefreshCw, Route, Save, Settings, ShieldCheck } from "lucide-react";
import { PageTabs } from "@/components/PageTabs";
import { AccessCard, ConvergeCard, CredentialsCard, ProxyCard, StorageCard, useOverviewError } from "./platform/HostConfigCards";
import { useTranslation } from "react-i18next";
import { PlayerEntrySettings } from "@/components/PlayerEntrySettings";
import { NodeControlPanel } from "@/components/NodeControlPanel";
import { DistributedNodes } from "@/components/DistributedNodes";
import { MailSettingsCard } from "@/components/MailSettingsCard";
import { RetentionCard } from "@/components/RetentionCard";
import { HeartbeatCard } from "@/components/HeartbeatCard";
import { FailoverPolicyCard } from "@/components/FailoverPolicyCard";
import { DefaultLanguageCard } from "@/components/DefaultLanguageCard";
import { BrandingCard } from "@/components/BrandingCard";
import { Badge } from "@/components/ui/badge";
import { PageHeader } from "@/components/PageHeader";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError } from "@/lib/api";
import { RefreshContext, useAsync, useConfig, usePageRefresh, useUnsavedGuard } from "@/lib/hooks";
import type { WakePolicySettings } from "@/lib/types";

const platformTabs = [
  { key: "entry", icon: Route },
  { key: "notices", icon: BellRing },
  { key: "access", icon: ShieldCheck },
  { key: "nodes", icon: Network },
  { key: "branding", icon: Palette },
] as const;
type PlatformTab = (typeof platformTabs)[number]["key"];

export function PlatformSettingsPage() {
  const { t } = useTranslation("admin");
  const config = useConfig();
  const query = useAsync(api.getWakePolicy, []);
  const reauth = useReauth();
  const cards = usePageRefresh();
  const [params, setParams] = useSearchParams();
  const requested = params.get("tab");
  const tab: PlatformTab = platformTabs.some((x) => x.key === requested) ? (requested as PlatformTab) : "entry";
  const overview = useAsync(api.hostOverview, []);
  const overviewError = useOverviewError(overview);
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
  return <RefreshContext.Provider value={cards.scope}><div className="space-y-6">
    <PageHeader icon={Settings} title={t("platform_title")} subtitle={t("platform_subtitle")} actions={<Button variant="outline" size="sm" disabled={dirty || cards.dirty || busy} onClick={() => { query.reload(); overview.reload(); cards.refresh(); }}><RefreshCw />{t("platform_reload")}</Button>}
      tabs={<PageTabs label={t("platform_tabs")} idPrefix="platform" value={tab} tabs={platformTabs.map(({ key, icon }) => ({ key, icon, label: t(`platform_tab_${key}`) }))}
        onChange={(key) => setParams((p) => { p.set("tab", key); return p; }, { replace: true })} />} />
    <div role="tabpanel" id="platform-panel-entry" aria-labelledby="platform-tab-entry" hidden={tab !== "entry"} className="space-y-6">
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
    <DefaultLanguageCard />
    </div>
    <div role="tabpanel" id="platform-panel-notices" aria-labelledby="platform-tab-notices" hidden={tab !== "notices"} className="space-y-6">
    <MailSettingsCard />
    <HeartbeatCard />
    <RetentionCard />
    </div>
    <div role="tabpanel" id="platform-panel-access" aria-labelledby="platform-tab-access" hidden={tab !== "access"} className="space-y-6">
    {overviewError && <MessageLine kind="error" message={overviewError} />}
    <AccessCard overview={overview} />
    <StorageCard overview={overview} />
    <CredentialsCard />
    <ProxyCard overview={overview} />
    <ConvergeCard />
    </div>
    <div role="tabpanel" id="platform-panel-nodes" aria-labelledby="platform-tab-nodes" hidden={tab !== "nodes"} className="space-y-6">
    <Card>
      <CardHeader><CardTitle>{t("platform_distribution_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap gap-2">
          <Badge variant={config?.distributed && !config.fallback ? "default" : "muted"}>
            {t(!config || config.fallback ? "platform_distribution_unknown" : config.distributed ? "platform_distribution_enabled" : "platform_distribution_disabled")}
          </Badge>
          {config?.distributed && !config.fallback && <Badge variant={config.controllerHA ? "default" : "muted"}>{t(config.controllerHA ? "platform_distribution_ha_enabled" : "platform_distribution_ha_disabled")}</Badge>}
        </div>
        <p className="text-sm leading-relaxed text-muted-foreground">{t("platform_distribution_architecture")}</p>
        <p className="text-sm leading-relaxed text-muted-foreground">{t("platform_distribution_management")}</p>
        <Button asChild variant="outline" size="sm"><Link to="/servers">{t("platform_distribution_servers")}</Link></Button>
      </CardContent>
    </Card>
    <NodeControlPanel distributed={!config || config.fallback ? null : !!config.distributed} controllerHA={!!config?.controllerHA} />
    {config?.distributed && !config.fallback && <DistributedNodes />}
    {config?.distributed && !config.fallback && <FailoverPolicyCard />}
    </div>
    <div role="tabpanel" id="platform-panel-branding" aria-labelledby="platform-tab-branding" hidden={tab !== "branding"} className="space-y-6">
    <BrandingCard />
    </div>
    {reauth.dialog}
  </div></RefreshContext.Provider>;
}
