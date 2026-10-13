import { useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, CloudUpload, Copy, Eye, KeyRound, ListTree, Loader2, Play, Power, Save, ShieldCheck, Undo2 } from "lucide-react";
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
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { formatAbsolute, formatRelative } from "@/lib/format";
import type { AsyncState } from "@/lib/hooks";
import { useUnsavedGuard } from "@/lib/hooks";
import type { HostOverview, OffsiteStatus } from "@/lib/types";

const blank = { endpoint: "", bucket: "", region: "", prefix: "", dbKeep: "", accessKey: "", secretKey: "", key: "" };

/** OffsiteCard runs the off-site copy: the hourly encrypted copy of world archives,
 *  database bundles, user images and uploads to an S3-compatible bucket. The Owner points
 *  it at a bucket, copies now, checks that the bucket's objects are sealed with this
 *  host's key, reads that key to keep it elsewhere, and takes the bucket over when this
 *  host replaces the one that wrote it. */
export function OffsiteCard({ overview }: { overview: AsyncState<HostOverview> }) {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const host = useHostTask(["offsite-sync", "offsite-list", "offsite-check-key", "offsite-take-over", "offsite-configure", "offsite-disable"]);
  const reauth = useReauth();
  const offsite = overview.data?.offsite;
  const status = offsite?.status as OffsiteStatus | undefined;
  const [editing, setEditing] = useState(false);
  const [form, setForm] = useState(blank);
  const [key, setKey] = useState<string | null>(null);
  const [keyError, setKeyError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<"take-over" | "disable" | null>(null);
  useEffect(() => {
    if (offsite && editing) setForm((f) => ({ ...f, endpoint: offsite.endpoint ?? "", bucket: offsite.bucket ?? "", region: offsite.region ?? "", prefix: offsite.prefix ?? "", dbKeep: offsite.dbKeep ? String(offsite.dbKeep) : "" }));
  }, [offsite, editing]);
  useUnsavedGuard(editing && (form.accessKey !== "" || form.secretKey !== "" || form.key !== ""));
  const needsKeys = !offsite?.configured;
  const valid = /^https?:\/\/\S+$/.test(form.endpoint) && /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(form.bucket)
    && (!form.dbKeep || (/^\d+$/.test(form.dbKeep) && Number(form.dbKeep) >= 1 && Number(form.dbKeep) <= 1000))
    && (needsKeys ? !!form.accessKey && !!form.secretKey : (!form.accessKey) === (!form.secretKey));
  async function save() {
    const params: Record<string, string> = { endpoint: form.endpoint.trim(), bucket: form.bucket.trim() };
    if (form.region.trim()) params.region = form.region.trim();
    if (form.prefix.trim()) params.prefix = form.prefix.trim();
    if (form.dbKeep) params.dbKeep = form.dbKeep;
    const secrets: Record<string, string> = {};
    if (form.accessKey) secrets.accessKey = form.accessKey.trim();
    if (form.secretKey) secrets.secretKey = form.secretKey.trim();
    if (form.key) secrets.key = form.key.trim();
    if (await host.start({ action: "offsite-configure", params, secrets, confirmMaintenance: false })) {
      setEditing(false);
      setForm(blank);
      overview.reload();
    }
  }
  async function reveal() {
    setKeyError(null);
    try {
      const answer = await reauth.guard(() => api.revealOffsiteKey());
      setKey(answer.key);
    } catch (e) {
      if (!isReauthCancelled(e)) setKeyError(humanizeError(e));
    }
  }
  const field = (name: keyof typeof blank, label: string, extra: { type?: string; placeholder?: string; hint?: string } = {}) => (
    <div className="space-y-2">
      <Label htmlFor={`offsite-${name}`}>{label}</Label>
      <Input id={`offsite-${name}`} type={extra.type ?? "text"} value={form[name]} placeholder={extra.placeholder} autoComplete="off" spellCheck={false}
        disabled={host.busy} onChange={(e) => setForm((f) => ({ ...f, [name]: e.target.value }))} />
      {extra.hint && <p className="text-xs leading-relaxed text-muted-foreground">{extra.hint}</p>}
    </div>
  );
  const healthy = !!status?.last_success && !status.key_mismatch && !status.standby && !status.displaced && (!status.last_attempt || status.last_attempt <= status.last_success);
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><CloudUpload className="h-4 w-4 text-primary" aria-hidden="true" />{t("offsite_title")}</CardTitle>
        {offsite && <Badge variant={!offsite.configured ? "muted" : healthy ? "default" : "destructive"}>{t(!offsite.configured ? "offsite_state_off" : healthy ? "offsite_state_ok" : "offsite_state_attention")}</Badge>}
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("offsite_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        {offsite?.configured && !editing && (
          <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
            <div className="min-w-0"><dt className="text-[11px] font-medium text-muted-foreground">{t("offsite_bucket")}</dt><dd className="truncate font-mono text-xs">{offsite.bucket}{offsite.prefix ? `/${offsite.prefix}` : ""}</dd></div>
            <div className="min-w-0"><dt className="text-[11px] font-medium text-muted-foreground">{t("offsite_endpoint")}</dt><dd className="truncate font-mono text-xs">{offsite.endpoint}</dd></div>
            <div><dt className="text-[11px] font-medium text-muted-foreground">{t("offsite_last_success")}</dt><dd>{status?.last_success ? <span title={formatAbsolute(status.last_success, locale)}>{formatRelative(status.last_success, Date.now(), locale)}</span> : t("offsite_never")}</dd></div>
            <div><dt className="text-[11px] font-medium text-muted-foreground">{t("offsite_last_attempt")}</dt><dd>{status?.last_attempt ? <span title={formatAbsolute(status.last_attempt, locale)}>{formatRelative(status.last_attempt, Date.now(), locale)}</span> : "—"}</dd></div>
          </dl>
        )}
        {offsite?.configured && !editing && status?.last_error && status.last_attempt && (!status.last_success || status.last_attempt > status.last_success) && <MessageLine kind="error" message={t("offsite_last_error", { error: status.last_error })} />}
        {status?.key_mismatch && <MessageLine kind="error" message={t("offsite_key_mismatch")} />}
        {(status?.standby || status?.displaced) && (
          <div className="space-y-2 rounded-lg border border-amber-500/30 bg-amber-500/10 p-4 text-sm text-amber-900 dark:text-amber-200">
            <p className="flex items-start gap-2"><AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />{t(status.displaced ? "offsite_displaced" : "offsite_standby", { host: status.writer?.host ?? "?" })}</p>
            <Button size="sm" variant="outline" disabled={host.busy} onClick={() => setConfirm("take-over")}>{t("offsite_take_over")}</Button>
          </div>
        )}
        {offsite?.error && <MessageLine kind="error" message={offsite.error} />}
        {offsite && !offsite.configured && !editing && <p className="rounded-md border border-dashed border-border p-4 text-sm text-muted-foreground">{t("offsite_not_configured")}</p>}

        {editing ? (
          <section aria-label={t("offsite_configure")} className="space-y-4 rounded-lg border border-border p-4">
            <div className="grid gap-4 md:grid-cols-2">
              {field("endpoint", t("offsite_endpoint"), { placeholder: "https://s3.example.com" })}
              {field("bucket", t("offsite_bucket"), { placeholder: "felis-backup" })}
              {field("region", t("offsite_region"), { placeholder: "auto" })}
              {field("prefix", t("offsite_prefix"), { placeholder: "host-a/", hint: t("offsite_prefix_hint") })}
              {field("accessKey", t("offsite_access_key"), { hint: needsKeys ? undefined : t("offsite_keys_keep") })}
              {field("secretKey", t("offsite_secret_key"), { type: "password" })}
              {field("dbKeep", t("offsite_db_keep"), { type: "number", placeholder: "30" })}
              {needsKeys && field("key", t("offsite_seal_key"), { type: "password", hint: t("offsite_seal_key_hint") })}
            </div>
            <div className="flex flex-wrap gap-3">
              <Button disabled={!valid || host.busy} onClick={() => void save()}>{host.starting ? <Loader2 className="animate-spin" /> : <Save />}{t("offsite_save")}</Button>
              <Button variant="outline" disabled={host.busy} onClick={() => { setEditing(false); setForm(blank); }}>{t("platform_discard")}</Button>
            </div>
          </section>
        ) : (
          <div className="flex flex-wrap gap-2">
            {offsite?.configured && <>
              <Button size="sm" disabled={host.busy} onClick={() => void host.start({ action: "offsite-sync", confirmMaintenance: false })}><Play />{t("offsite_sync_now")}</Button>
              <Button size="sm" variant="outline" disabled={host.busy} onClick={() => void host.start({ action: "offsite-check-key", confirmMaintenance: false })}><ShieldCheck />{t("offsite_check_key")}</Button>
              <Button size="sm" variant="outline" disabled={host.busy} onClick={() => void host.start({ action: "offsite-list", confirmMaintenance: false })}><ListTree />{t("offsite_list")}</Button>
            </>}
            <Button size="sm" variant="outline" disabled={!host.available || host.busy} onClick={() => setEditing(true)}><CloudUpload />{t(offsite?.configured ? "offsite_change" : "offsite_configure")}</Button>
            {offsite?.configured && <Button size="sm" variant="outline" disabled={host.busy} onClick={() => setConfirm("disable")}><Power />{t("offsite_disable")}</Button>}
          </div>
        )}

        {offsite?.keyKept && (
          <section aria-label={t("offsite_key_title")} className="space-y-2 rounded-lg border border-border p-4">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <h3 className="flex items-center gap-2 text-sm font-semibold"><KeyRound className="h-4 w-4 text-primary" aria-hidden="true" />{t("offsite_key_title")}</h3>
              {key ? <Button size="sm" variant="outline" onClick={() => setKey(null)}><Undo2 />{t("offsite_key_hide")}</Button>
                : <Button size="sm" variant="outline" onClick={() => void reveal()}><Eye />{t("offsite_key_show")}</Button>}
            </div>
            <p className="text-xs leading-relaxed text-muted-foreground">{t("offsite_key_hint")}</p>
            {key && (
              <div className="flex items-center gap-2 rounded-md border border-border bg-muted/40 py-1 pl-3 pr-1">
                <code className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap py-1 font-mono text-xs">{key}</code>
                <Button size="icon" variant="ghost" className="h-7 w-7" aria-label={t("offsite_key_copy")} onClick={() => void navigator.clipboard?.writeText(key)}><Copy className="h-3.5 w-3.5" /></Button>
              </div>
            )}
            {keyError && <MessageLine kind="error" message={keyError} />}
          </section>
        )}

        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
        {host.task?.state === "succeeded" && host.task.request.action === "offsite-configure" && !offsite?.keyKept && <p className="flex items-center gap-2 text-sm text-muted-foreground"><CheckCircle2 className="h-4 w-4 text-primary" />{t("offsite_saved_reload")}</p>}
      </CardContent>
      <ConfirmDialog
        open={confirm !== null}
        onOpenChange={(open) => { if (!open) setConfirm(null); }}
        title={t(confirm === "disable" ? "offsite_disable_title" : "offsite_take_over_title")}
        description={t(confirm === "disable" ? "offsite_disable_confirm" : "offsite_take_over_confirm")}
        confirmLabel={t(confirm === "disable" ? "offsite_disable" : "offsite_take_over")}
        onConfirm={async () => {
          if (await host.start({ action: confirm === "disable" ? "offsite-disable" : "offsite-take-over", confirmMaintenance: true })) overview.reload();
        }}
      />
      {host.reauth.dialog}
      {reauth.dialog}
    </Card>
  );
}

/** RestoreHostCard rebuilds this host from the off-site copy: the database, the user
 *  images, the uploads, the servers and their worlds. It is meant for a freshly installed
 *  host that replaces a lost one; over an install that holds servers it asks to overwrite. */
export function RestoreHostCard({ overview }: { overview: AsyncState<HostOverview> }) {
  const { t } = useTranslation("admin");
  const host = useHostTask(["restore-host"]);
  const [bundle, setBundle] = useState("");
  const [takeOver, setTakeOver] = useState(true);
  const [force, setForce] = useState(false);
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const word = t("restore_host_word");
  const validBundle = !bundle || /^felis-db-[0-9]{8}T[0-9]{6}Z-[a-z][a-z-]*\.tar$/.test(bundle);
  const configured = !!overview.data?.offsite.configured;
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2"><Undo2 className="h-4 w-4 text-primary" aria-hidden="true" />{t("restore_host_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("restore_host_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        {!configured && overview.data && <p className="text-sm text-muted-foreground">{t("restore_host_needs_offsite")}</p>}
        <div className="grid gap-4 md:grid-cols-2">
          <div className="space-y-2 md:col-span-2">
            <Label htmlFor="restore-bundle">{t("restore_host_bundle")}</Label>
            <Input id="restore-bundle" value={bundle} placeholder="felis-db-20261001T033000Z-offsite.tar" className="font-mono text-xs" disabled={host.busy} onChange={(e) => setBundle(e.target.value.trim())} />
            <p className="text-xs leading-relaxed text-muted-foreground">{t("restore_host_bundle_hint")}</p>
          </div>
        </div>
        <div className="flex items-start justify-between gap-4 rounded-lg border border-border p-4">
          <div className="space-y-1"><Label htmlFor="restore-take-over">{t("restore_host_take_over")}</Label><p className="text-xs leading-relaxed text-muted-foreground">{t("restore_host_take_over_hint")}</p></div>
          <Switch id="restore-take-over" checked={takeOver} disabled={host.busy} onCheckedChange={setTakeOver} />
        </div>
        <div className="flex items-start justify-between gap-4 rounded-lg border border-destructive/30 p-4">
          <div className="space-y-1"><Label htmlFor="restore-force">{t("restore_host_force")}</Label><p className="text-xs leading-relaxed text-muted-foreground">{t("restore_host_force_hint")}</p></div>
          <Switch id="restore-force" checked={force} disabled={host.busy} onCheckedChange={setForce} />
        </div>
        <Button variant="destructive" disabled={!configured || !validBundle || host.busy} onClick={() => { setTyped(""); setOpen(true); }}>
          {host.starting ? <Loader2 className="animate-spin" /> : <Undo2 />}{t("restore_host_start")}
        </Button>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title={t("restore_host_confirm_title")}
        description={t("restore_host_confirm")}
        confirmLabel={t("restore_host_start")}
        confirmDisabled={typed.trim() !== word}
        onConfirm={async () => {
          const params: Record<string, string> = { takeOver: String(takeOver), force: String(force) };
          if (bundle) params.bundle = bundle;
          await host.start({ action: "restore-host", params, confirmMaintenance: true });
        }}
      >
        <div className="space-y-2">
          <Label htmlFor="restore-host-confirm">{t("restore_host_type", { word })}</Label>
          <Input id="restore-host-confirm" value={typed} autoComplete="off" onChange={(e) => setTyped(e.target.value)} />
        </div>
      </ConfirmDialog>
      {host.reauth.dialog}
    </Card>
  );
}
