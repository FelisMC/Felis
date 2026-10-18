import { useEffect, useState } from "react";
import { Cpu, Globe2, HardDriveUpload, KeyRound, Loader2, Save, SearchCheck, Wrench } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { HostTaskNotice, HostTaskStatus, useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";
import { humanizeError } from "@/lib/api";
import type { AsyncState } from "@/lib/hooks";
import { useUnsavedGuard } from "@/lib/hooks";
import type { HostOverview } from "@/lib/types";
import { cn } from "@/lib/utils";

type Overview = AsyncState<HostOverview>;
const dnsName = /^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/;

/** Choice is a row of mutually exclusive options that read as one control. */
function Choice<T extends string>({ value, options, onChange, disabled, label, describe }: { value: T; options: readonly T[]; onChange: (v: T) => void; disabled?: boolean; label: string; describe: (v: T) => string }) {
  return (
    <div role="radiogroup" aria-label={label} className="grid gap-2 sm:grid-cols-3">
      {options.map((o) => (
        <Button key={o} type="button" role="radio" aria-checked={value === o} variant="outline" disabled={disabled}
          className={cn("h-auto justify-start whitespace-normal px-3 py-2 text-left active:scale-100", value === o && "border-primary/40 bg-primary/5 ring-1 ring-primary/20")}
          onClick={() => onChange(o)}>{describe(o)}</Button>
      ))}
    </div>
  );
}

export function AccessCard({ overview }: { overview: Overview }) {
  const { t } = useTranslation("admin");
  const host = useHostTask(["connection-configure", "domain-check", "domain-set"]);
  const current = overview.data?.connection;
  const [mode, setMode] = useState<"local" | "reverse-proxy" | "cloudflare">("local");
  const [panelHost, setPanelHost] = useState("");
  const [adminHost, setAdminHost] = useState("");
  const [identity, setIdentity] = useState("");
  const [accountID, setAccountID] = useState("");
  const [apiToken, setApiToken] = useState("");
  const [domain, setDomain] = useState("");
  const [confirm, setConfirm] = useState<"connection" | "domain" | null>(null);
  useEffect(() => {
    if (current) {
      setMode(current.mode);
      setPanelHost(current.panelHost);
      setAdminHost(current.adminHost);
    }
  }, [current]);
  const changed = !!current && (mode !== current.mode || panelHost !== current.panelHost || adminHost !== current.adminHost || mode === "cloudflare");
  useUnsavedGuard(apiToken !== "");
  const hostsValid = (!panelHost || dnsName.test(panelHost)) && (mode === "local" || dnsName.test(adminHost));
  const cloudflareValid = mode !== "cloudflare" || (/^[a-f0-9]{32}$/.test(accountID) && /^([A-Za-z0-9._%+-]+)?@[A-Za-z0-9.-]+\.[A-Za-z]{2,63}$/.test(identity) && apiToken !== "");
  const domainValid = dnsName.test(domain) && domain !== overview.data?.rootDomain;
  async function apply() {
    const params: Record<string, string> = { mode };
    if (panelHost) params.panelHost = panelHost;
    if (adminHost) params.adminHost = adminHost;
    if (mode === "cloudflare") {
      params.identity = identity;
      params.accountID = accountID;
    }
    const task = await host.start({ action: "connection-configure", params, secrets: mode === "cloudflare" ? { apiToken } : undefined, confirmMaintenance: true });
    if (task) setApiToken("");
  }
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><Globe2 className="h-4 w-4 text-primary" aria-hidden="true" />{t("access_title")}</CardTitle>
        {current && <Badge variant="muted">{t(`access_mode_${current.mode}`)}</Badge>}
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("access_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        {overview.data && <p className="text-sm">{t("access_root_domain", { domain: overview.data.rootDomain || "—" })}</p>}
        <Choice value={mode} options={["local", "reverse-proxy", "cloudflare"] as const} onChange={setMode} disabled={host.busy} label={t("access_mode")} describe={(m) => t(`access_mode_${m}`)} />
        <p className="text-xs leading-relaxed text-muted-foreground">{t(`access_mode_${mode}_hint`)}</p>
        <div className="grid gap-4 md:grid-cols-2">
          <div className="space-y-2"><Label htmlFor="access-panel-host">{t("access_panel_host")}</Label><Input id="access-panel-host" value={panelHost} disabled={host.busy} onChange={(e) => setPanelHost(e.target.value.trim().toLowerCase())} placeholder="console.example.com" /></div>
          <div className="space-y-2"><Label htmlFor="access-admin-host">{t("access_admin_host")}</Label><Input id="access-admin-host" value={adminHost} disabled={host.busy} onChange={(e) => setAdminHost(e.target.value.trim().toLowerCase())} placeholder="op.console.example.com" /></div>
          {mode === "cloudflare" && <>
            <div className="space-y-2"><Label htmlFor="access-identity">{t("access_identity")}</Label><Input id="access-identity" value={identity} disabled={host.busy} onChange={(e) => setIdentity(e.target.value.trim())} placeholder="owner@example.com · @example.com" /><p className="text-xs text-muted-foreground">{t("access_identity_hint")}</p></div>
            <div className="space-y-2"><Label htmlFor="access-account">{t("access_account")}</Label><Input id="access-account" value={accountID} disabled={host.busy} onChange={(e) => setAccountID(e.target.value.trim().toLowerCase())} className="font-mono text-xs" /></div>
            <div className="space-y-2 md:col-span-2"><Label htmlFor="access-token">{t("access_token")}</Label><Input id="access-token" type="password" value={apiToken} autoComplete="off" disabled={host.busy} onChange={(e) => setApiToken(e.target.value.trim())} /><p className="text-xs leading-relaxed text-muted-foreground">{t("access_token_hint")}</p></div>
          </>}
        </div>
        <Button disabled={!host.available || host.busy || !changed || !hostsValid || !cloudflareValid} onClick={() => setConfirm("connection")}>
          {host.starting ? <Loader2 className="animate-spin" /> : <Save />}{t("access_apply")}
        </Button>

        <section aria-label={t("domain_title")} className="space-y-4 border-t border-border pt-5">
          <div className="space-y-1"><h3 className="text-sm font-semibold">{t("domain_title")}</h3><p className="text-xs leading-relaxed text-muted-foreground">{t("domain_hint")}</p></div>
          <div className="flex flex-wrap items-end gap-3">
            <div className="min-w-56 flex-1 space-y-2"><Label htmlFor="domain-new">{t("domain_new")}</Label><Input id="domain-new" value={domain} disabled={host.busy} onChange={(e) => setDomain(e.target.value.trim().toLowerCase())} placeholder="example.com" /></div>
            <Button variant="outline" disabled={!host.available || host.busy} onClick={() => void host.start({ action: "domain-check", confirmMaintenance: false })}><SearchCheck />{t("domain_check")}</Button>
            <Button variant="destructive" disabled={!host.available || host.busy || !domainValid} onClick={() => setConfirm("domain")}><Globe2 />{t("domain_set")}</Button>
          </div>
        </section>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog
        open={confirm !== null}
        onOpenChange={(open) => { if (!open) setConfirm(null); }}
        title={t(confirm === "domain" ? "domain_confirm_title" : "access_confirm_title")}
        description={confirm === "domain" ? t("domain_confirm", { domain }) : t("access_confirm")}
        confirmLabel={t(confirm === "domain" ? "domain_set" : "access_apply")}
        onConfirm={async () => {
          if (confirm === "domain") await host.start({ action: "domain-set", params: { domain }, confirmMaintenance: true });
          else await apply();
        }}
      />
      {host.reauth.dialog}
    </Card>
  );
}

/** StorageCard chooses where build-context uploads are kept: the uploads volume on this
 *  host, or an S3-compatible bucket. Existing uploads are not moved. */
export function StorageCard({ overview }: { overview: Overview }) {
  const { t } = useTranslation("admin");
  const host = useHostTask(["storage-configure"]);
  const current = overview.data?.storage;
  const [mode, setMode] = useState<"local" | "s3">("local");
  const [endpoint, setEndpoint] = useState("");
  const [bucket, setBucket] = useState("");
  const [region, setRegion] = useState("");
  const [accessKey, setAccessKey] = useState("");
  const [secretKey, setSecretKey] = useState("");
  const [confirming, setConfirming] = useState(false);
  useEffect(() => {
    if (current) {
      setMode(current.mode);
      setEndpoint(current.endpoint ?? "");
      setBucket(current.bucket ?? "");
      setRegion(current.region ?? "");
    }
  }, [current]);
  useUnsavedGuard(accessKey !== "" || secretKey !== "");
  const valid = mode === "local" ? current?.mode !== "local" : /^https?:\/\/\S+$/.test(endpoint) && /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(bucket) && !!accessKey && !!secretKey;
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><HardDriveUpload className="h-4 w-4 text-primary" aria-hidden="true" />{t("storage_title")}</CardTitle>
        {current && <Badge variant="muted">{current.mode === "s3" ? `s3://${current.bucket}` : t("storage_local")}</Badge>}
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("storage_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <Choice value={mode} options={["local", "s3"] as const} onChange={setMode} disabled={host.busy} label={t("storage_mode")} describe={(m) => t(m === "s3" ? "storage_s3" : "storage_local")} />
        {mode === "s3" && (
          <div className="grid gap-4 md:grid-cols-2">
            <div className="space-y-2"><Label htmlFor="storage-endpoint">{t("offsite_endpoint")}</Label><Input id="storage-endpoint" value={endpoint} disabled={host.busy} onChange={(e) => setEndpoint(e.target.value.trim())} placeholder="https://s3.example.com" /></div>
            <div className="space-y-2"><Label htmlFor="storage-bucket">{t("offsite_bucket")}</Label><Input id="storage-bucket" value={bucket} disabled={host.busy} onChange={(e) => setBucket(e.target.value.trim())} /></div>
            <div className="space-y-2"><Label htmlFor="storage-region">{t("offsite_region")}</Label><Input id="storage-region" value={region} disabled={host.busy} onChange={(e) => setRegion(e.target.value.trim())} placeholder="auto" /></div>
            <div className="space-y-2"><Label htmlFor="storage-access">{t("offsite_access_key")}</Label><Input id="storage-access" value={accessKey} autoComplete="off" disabled={host.busy} onChange={(e) => setAccessKey(e.target.value.trim())} /></div>
            <div className="space-y-2 md:col-span-2"><Label htmlFor="storage-secret">{t("offsite_secret_key")}</Label><Input id="storage-secret" type="password" value={secretKey} autoComplete="off" disabled={host.busy} onChange={(e) => setSecretKey(e.target.value.trim())} /><p className="text-xs text-muted-foreground">{t("storage_keys_hint")}</p></div>
          </div>
        )}
        <Button disabled={!host.available || host.busy || !valid} onClick={() => setConfirming(true)}>{host.starting ? <Loader2 className="animate-spin" /> : <Save />}{t("storage_apply")}</Button>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog open={confirming} onOpenChange={setConfirming} title={t("storage_confirm_title")} description={t("storage_confirm")} confirmLabel={t("storage_apply")} confirmVariant="default"
        onConfirm={async () => {
          const params: Record<string, string> = { mode };
          if (mode === "s3") Object.assign(params, { endpoint, bucket, ...(region ? { region } : {}) });
          if (await host.start({ action: "storage-configure", params, secrets: mode === "s3" ? { accessKey, secretKey } : undefined, confirmMaintenance: true })) {
            setAccessKey("");
            setSecretKey("");
            overview.reload();
          }
        }} />
      {host.reauth.dialog}
    </Card>
  );
}

const credentials = ["ops", "velocity", "limbo", "build", "registry", "forwarding", "db"] as const;

/** CredentialsCard replaces one generated credential and restarts what reads it. Each
 *  row says what the rotation interrupts. */
export function CredentialsCard() {
  const { t } = useTranslation("admin");
  const host = useHostTask(["rotate-token"]);
  const [kind, setKind] = useState<(typeof credentials)[number] | null>(null);
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2"><KeyRound className="h-4 w-4 text-primary" aria-hidden="true" />{t("credentials_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("credentials_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <ul className="divide-y divide-border rounded-lg border border-border">
          {credentials.map((k) => (
            <li key={k} className="flex flex-wrap items-center justify-between gap-3 p-3">
              <div className="min-w-0 space-y-1">
                <p className="text-sm font-medium">{t(`credential_${k}`)}</p>
                <p className="text-xs leading-relaxed text-muted-foreground">{t(`credential_${k}_impact`)}</p>
              </div>
              <Button variant="outline" size="sm" disabled={!host.available || host.busy} onClick={() => setKind(k)}>{t("credentials_rotate")}</Button>
            </li>
          ))}
        </ul>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog open={kind !== null} onOpenChange={(open) => { if (!open) setKind(null); }}
        title={t("credentials_confirm_title", { name: kind ? t(`credential_${kind}`) : "" })}
        description={kind ? t(`credential_${kind}_impact`) : ""}
        confirmLabel={t("credentials_rotate")}
        onConfirm={async () => { if (kind) await host.start({ action: "rotate-token", params: { kind }, confirmMaintenance: true }); }} />
      {host.reauth.dialog}
    </Card>
  );
}

/** ProxyCard sets the game proxy's heap ceiling; a change restarts the proxy, which
 *  disconnects every player. */
export function ProxyCard({ overview }: { overview: Overview }) {
  const { t } = useTranslation("admin");
  const host = useHostTask(["host-settings"]);
  const current = overview.data?.settings.velocityXmx ?? "";
  const [xmx, setXmx] = useState("");
  const [confirming, setConfirming] = useState(false);
  useEffect(() => { setXmx(current); }, [current]);
  const n = Number(xmx.replace(/[MG]$/, ""));
  const valid = /^[1-9][0-9]{0,4}[MG]$/.test(xmx) && !(xmx.endsWith("M") && n < 256) && !(xmx.endsWith("G") && n > 64) && xmx !== current;
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2"><Cpu className="h-4 w-4 text-primary" aria-hidden="true" />{t("proxy_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("proxy_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <div className="flex flex-wrap items-end gap-3">
          <div className="w-40 space-y-2"><Label htmlFor="proxy-xmx">{t("proxy_heap")}</Label><Input id="proxy-xmx" value={xmx} disabled={!current || host.busy} onChange={(e) => setXmx(e.target.value.trim().toUpperCase())} placeholder="1G" /></div>
          <Button disabled={!host.available || host.busy || !valid} onClick={() => setConfirming(true)}>{host.starting ? <Loader2 className="animate-spin" /> : <Save />}{t("proxy_apply")}</Button>
        </div>
        <p className="text-xs leading-relaxed text-muted-foreground">{t("proxy_heap_hint")}</p>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog open={confirming} onOpenChange={setConfirming} title={t("proxy_confirm_title")} description={t("proxy_confirm")} confirmLabel={t("proxy_apply")}
        onConfirm={async () => { if (await host.start({ action: "host-settings", params: { velocityXmx: xmx }, confirmMaintenance: true })) overview.reload(); }} />
      {host.reauth.dialog}
    </Card>
  );
}

// Preserve existing server settings; filling missing RCON configuration is opt-in.
export function ConvergeCard() {
  const { t } = useTranslation("admin");
  const host = useHostTask(["converge"]);
  const [rcon, setRcon] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const start = () => host.start({ action: "converge", params: { userRcon: String(rcon) }, confirmMaintenance: rcon });
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2"><Wrench className="h-4 w-4 text-primary" aria-hidden="true" />{t("converge_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("converge_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <div className="flex items-start justify-between gap-4 rounded-lg border border-border p-4">
          <div className="space-y-1"><Label htmlFor="converge-rcon">{t("converge_rcon")}</Label><p className="text-xs leading-relaxed text-muted-foreground">{t("converge_rcon_hint")}</p></div>
          <Switch id="converge-rcon" checked={rcon} disabled={host.busy} onCheckedChange={setRcon} />
        </div>
        <Button disabled={!host.available || host.busy} onClick={() => (rcon ? setConfirming(true) : void start())}>{host.starting ? <Loader2 className="animate-spin" /> : <Wrench />}{t("converge_run")}</Button>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog open={confirming} onOpenChange={setConfirming} title={t("converge_confirm_title")} description={t("converge_rcon_hint")} confirmLabel={t("converge_run")} onConfirm={async () => { await start(); }} />
      {host.reauth.dialog}
    </Card>
  );
}

/** useOverviewError reads the overview's error for display. */
export function useOverviewError(overview: Overview) {
  return overview.error != null ? humanizeError(overview.error) : null;
}
