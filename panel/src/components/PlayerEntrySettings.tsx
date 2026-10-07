import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ArrowRight, DoorOpen, Globe, Loader2, LogIn, RefreshCw, Route, Save, Server, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useUnsavedGuard } from "@/lib/hooks";
import type { EntryPolicySettings } from "@/lib/types";

const modes = [{ value: "lobby", icon: DoorOpen }, { value: "direct", icon: Server }, { value: "domain", icon: Globe }] as const;

const initial: EntryPolicySettings = { mode: "domain", defaultServer: "", requireAccountLink: true, offlineAction: "wake", waitingSpace: "lobby", fallbackServer: "", revision: "" };

export function PlayerEntrySettings() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getEntryPolicy, []);
  const fleet = useAsync(api.fleet, []);
  const reauth = useReauth();
  const [draft, setDraft] = useState(initial);
  const [saved, setSaved] = useState<EntryPolicySettings | null>(null);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const dirty = saved !== null && JSON.stringify(draft) !== JSON.stringify(saved);
  useUnsavedGuard(dirty);
  useEffect(() => {
    if (query.data) { setSaved(query.data); setDraft(query.data); }
  }, [query.data]);
  const busy = query.loading || saving || !saved;
  const servers = (fleet.data ?? []).filter((server) => !server.system && server.name !== "login" && server.name !== "lobby");
  const valid = (draft.mode !== "direct" || !!draft.defaultServer)
    && (draft.offlineAction !== "fallback" || (!!draft.fallbackServer && draft.fallbackServer !== draft.defaultServer));
  function change(update: Partial<EntryPolicySettings>) { setDraft((value) => ({ ...value, ...update })); setMessage(null); }
  function serverSelect(field: "defaultServer" | "fallbackServer", optional = false) {
    const value = draft[field];
    return <Select value={value || "__unset__"} disabled={busy || fleet.loading || !!fleet.error} onValueChange={(name) => change({ [field]: name === "__unset__" ? "" : name })}>
      <SelectTrigger id={`entry-${field}`}><SelectValue placeholder={t("entry_select_server")} /></SelectTrigger>
      <SelectContent>
        {optional && <SelectItem value="__unset__">{t("entry_no_default")}</SelectItem>}
        {!optional && !value && <SelectItem value="__unset__" disabled>{t("entry_select_server")}</SelectItem>}
        {value && !servers.some((server) => server.name === value) && <SelectItem value={value}>{value}</SelectItem>}
        {servers.map((server) => <SelectItem key={server.name} value={server.name}>{server.displayName || server.name} · {server.name}</SelectItem>)}
      </SelectContent>
    </Select>;
  }
  async function save() {
    if (busy || !dirty || !valid) return;
    setSaving(true); setMessage(null);
    try {
      const policy = await reauth.guard(() => api.setEntryPolicy(draft));
      setSaved(policy); setDraft(policy);
      setMessage({ kind: "success", text: t("entry_saved") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally { setSaving(false); }
  }
  return <Card>
    <CardHeader className="flex-row flex-wrap items-center justify-between gap-3"><CardTitle className="flex items-center gap-2"><Route className="h-4 w-4 text-primary" aria-hidden="true" />{t("entry_title")}</CardTitle><Button variant="outline" size="sm" disabled={dirty || saving || query.loading} onClick={() => { query.reload(); fleet.reload(); }}><RefreshCw />{t("platform_reload")}</Button></CardHeader>
    <CardContent className="space-y-6" aria-busy={query.loading}>
      <p className="text-sm text-muted-foreground">{t("entry_description")}</p>
      {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("platform_loading")}</p>}
      <div role="radiogroup" aria-label={t("entry_mode")} className="grid gap-3 sm:grid-cols-3" onKeyDown={(event) => {
        if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End"].includes(event.key) || busy) return;
        event.preventDefault();
        const choices = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="radio"]'));
        const current = choices.indexOf(event.target as HTMLButtonElement);
        const index = event.key === "Home" ? 0 : event.key === "End" ? choices.length - 1 : (current + (event.key === "ArrowLeft" || event.key === "ArrowUp" ? -1 : 1) + choices.length) % choices.length;
        choices[index].focus(); choices[index].click();
      }}>
        {modes.map(({ value: mode, icon: Icon }) => <Button key={mode} type="button" role="radio" tabIndex={draft.mode === mode ? 0 : -1} aria-checked={draft.mode === mode} variant="outline" disabled={busy} className={`h-auto justify-start whitespace-normal px-4 py-3 text-left active:scale-100 ${draft.mode === mode ? "border-primary/40 bg-primary/5 ring-1 ring-primary/20" : ""}`} onClick={() => { if (draft.mode === mode) return; change({ mode, ...(mode === "lobby" ? { defaultServer: "" } : {}), ...(mode === "direct" ? { requireAccountLink: false, offlineAction: "disconnect", waitingSpace: "login", fallbackServer: "" } : {}) }); }}>
          <span className="space-y-1"><span className="flex items-center gap-2"><Icon className={draft.mode === mode ? "text-primary" : "text-muted-foreground"} aria-hidden="true" />{t(`entry_${mode}`)}</span><span className="block text-xs font-normal leading-relaxed text-muted-foreground">{t(`entry_${mode}_hint`)}</span></span>
        </Button>)}
      </div>
      <div className="flex flex-wrap items-center gap-2 rounded-lg bg-muted/40 px-4 py-3 text-sm" aria-label={t("entry_preview")}>
        <span className="flex items-center gap-2"><ShieldCheck className="h-4 w-4 text-primary" aria-hidden="true" />{t("entry_identity")}</span>{draft.requireAccountLink && <><ArrowRight className="h-4 w-4 text-muted-foreground" /><span className="flex items-center gap-2"><LogIn className="h-4 w-4 text-muted-foreground" aria-hidden="true" />{t("entry_web")}</span></>}<ArrowRight className="h-4 w-4 text-muted-foreground" /><span className="font-medium">{draft.mode === "lobby" ? t("entry_lobby") : draft.mode === "domain" ? t("entry_domain_target") : draft.defaultServer || t("entry_select_server")}</span>
      </div>
      <div className="grid gap-6 md:grid-cols-2">
        {draft.mode !== "lobby" && <div className="space-y-2"><Label htmlFor="entry-defaultServer">{t(draft.mode === "direct" ? "entry_main" : "entry_default")}</Label>{serverSelect("defaultServer", draft.mode === "domain")}<p className="text-xs leading-relaxed text-muted-foreground">{t(draft.mode === "direct" ? "entry_main_hint" : "entry_default_hint")}</p></div>}
        <div className="space-y-2"><Label htmlFor="entry-offline">{t("entry_offline")}</Label><Select value={draft.offlineAction} disabled={busy} onValueChange={(value) => change({ offlineAction: value as EntryPolicySettings["offlineAction"], ...(value !== "fallback" ? { fallbackServer: "" } : {}) })}><SelectTrigger id="entry-offline"><SelectValue /></SelectTrigger><SelectContent>{(["wake", "fallback", "disconnect"] as const).map((action) => <SelectItem key={action} value={action}>{t(`entry_${action}`)}</SelectItem>)}</SelectContent></Select></div>
        {draft.offlineAction === "fallback" && <div className="space-y-2"><Label htmlFor="entry-fallbackServer">{t("entry_fallback_server")}</Label>{serverSelect("fallbackServer")}</div>}
        {draft.offlineAction === "wake" && <div className="space-y-2"><Label htmlFor="entry-waiting">{t("entry_waiting")}</Label><Select value={draft.waitingSpace} disabled={busy || draft.requireAccountLink} onValueChange={(value) => change({ waitingSpace: value as EntryPolicySettings["waitingSpace"] })}><SelectTrigger id="entry-waiting"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="login">{t("entry_wait_login")}</SelectItem><SelectItem value="lobby">{t("entry_lobby")}</SelectItem></SelectContent></Select><p className="text-xs leading-relaxed text-muted-foreground">{t(draft.requireAccountLink ? "entry_web_wait" : "entry_wake_hint")}</p></div>}
      </div>
      <div className="flex items-start justify-between gap-4 border-t border-border pt-5"><div className="space-y-1"><Label htmlFor="entry-link">{t("entry_link")}</Label><p className="text-xs leading-relaxed text-muted-foreground">{t("entry_link_hint")}</p></div><Switch id="entry-link" checked={draft.requireAccountLink} disabled={busy} onCheckedChange={(checked) => change({ requireAccountLink: checked, ...(checked ? { waitingSpace: "lobby" } : {}) })} /></div>
      <p className="text-xs leading-relaxed text-muted-foreground">{t("entry_components")} <Link className="font-medium text-primary hover:underline" to="/admin/lobby">{t("entry_manage_spaces")}</Link></p>
      {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
      {fleet.error != null && <MessageLine kind="error" message={humanizeError(fleet.error)} />}
      {message && <MessageLine kind={message.kind} message={message.text} />}
    </CardContent>
    <CardFooter><p className="text-xs text-muted-foreground">{t("entry_scope")}</p><div className="flex gap-2">{dirty && <Button variant="outline" disabled={saving} onClick={() => { if (saved) setDraft(saved); setMessage(null); }}>{t("platform_discard")}</Button>}<Button disabled={busy || !dirty || !valid} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t("entry_save")}</Button></div></CardFooter>
    {reauth.dialog}
  </Card>;
}
