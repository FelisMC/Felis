import { useEffect, useState } from "react";
import { Loader2, Mail, RotateCcw, Save, Send } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { InlineConfirm } from "@/components/InlineConfirm";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useRefreshSignal, useUnsavedGuard } from "@/lib/hooks";
import type { MailSettings, MailSettingsRequest } from "@/lib/types";

const PORTS = [587, 465, 25, 2525] as const;
type Port = (typeof PORTS)[number];

interface Draft {
  host: string;
  port: Port;
  from: string;
  username: string;
  password: string;
  requireTLS: boolean;
  maxPerHour: string;
}

function draftOf(s: MailSettings): Draft {
  return {
    host: s.host,
    port: (PORTS as readonly number[]).includes(s.port) ? (s.port as Port) : 587,
    from: s.from,
    username: s.username,
    password: "",
    requireTLS: s.source === "none" ? true : s.require_tls,
    maxPerHour: String(s.max_per_hour),
  };
}

// MailSettingsCard is the Owner's mail relay: the server email codes, identity checks and
// alerts go through. Saving proves the relay first (one self-test message to the sender),
// so a relay that cannot deliver is never put in force; a test mails the Owner without
// saving. The password is write-only: left empty, the stored one is kept.
export function MailSettingsCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getMailSettings, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<MailSettings | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [busy, setBusy] = useState<"save" | "test" | "clear" | null>(null);
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
  useRefreshSignal(query.reload, dirty);

  const cap = Number(draft?.maxPerHour);
  const valid = !!draft && draft.host.trim() !== "" && /^[^\s@]+@[^\s@]+$/.test(draft.from.trim()) && /^\d+$/.test(draft.maxPerHour) && cap <= 100000;

  function request(): MailSettingsRequest {
    const d = draft!;
    return {
      host: d.host.trim(), port: d.port, from: d.from.trim(), username: d.username.trim(),
      ...(d.password !== "" ? { password: d.password } : {}),
      require_tls: d.requireTLS, max_per_hour: cap, revision: saved?.revision ?? "",
    };
  }
  function set<K extends keyof Draft>(key: K, value: Draft[K]) {
    setDraft((d) => d && { ...d, [key]: value });
    setMessage(null);
  }
  async function run(kind: "save" | "test" | "clear") {
    if (busy || !draft) return;
    setBusy(kind);
    setMessage(null);
    try {
      if (kind === "test") {
        const result = await reauth.guard(() => api.testMailSettings(request()));
        setMessage({ kind: "success", text: t("mail_test_sent", { to: result.sent_to }) });
      } else {
        const next = await reauth.guard(() => (kind === "save" ? api.setMailSettings(request()) : api.clearMailSettings()));
        setSaved(next);
        setDraft(draftOf(next));
        setConfirmClear(false);
        setMessage({ kind: "success", text: t(kind === "save" ? "mail_saved" : "mail_cleared") });
      }
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setBusy(null);
    }
  }

  const disabled = !draft || busy !== null;
  const sourceKey = saved ? `mail_source_${saved.source}` : "mail_source_loading";
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><Mail className="h-4 w-4" /></span>
        <CardTitle>{t("mail_title")}</CardTitle>
        <Badge variant={saved?.source === "none" ? "warning" : saved?.source === "panel" ? "default" : "muted"}>{t(sourceKey)}</Badge>
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("mail_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("mail_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        <div className="grid gap-5 md:grid-cols-2">
          <div className="space-y-2">
            <Label htmlFor="mail-host">{t("mail_host")}</Label>
            <Input id="mail-host" value={draft?.host ?? ""} disabled={disabled} onChange={(e) => set("host", e.target.value)} placeholder="smtp.example.com" autoComplete="off" spellCheck={false} />
          </div>
          <div className="space-y-2">
            <Label htmlFor="mail-port">{t("mail_port")}</Label>
            <Select value={String(draft?.port ?? 587)} disabled={disabled} onValueChange={(value) => set("port", Number(value) as Port)}>
              <SelectTrigger id="mail-port"><SelectValue /></SelectTrigger>
              <SelectContent>{PORTS.map((port) => <SelectItem key={port} value={String(port)}>{t(`mail_port_${port}`)}</SelectItem>)}</SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="mail-from">{t("mail_from")}</Label>
            <Input id="mail-from" type="email" value={draft?.from ?? ""} disabled={disabled} onChange={(e) => set("from", e.target.value)} placeholder="noreply@example.com" autoComplete="off" spellCheck={false} />
            <p className="text-xs leading-relaxed text-muted-foreground">{t("mail_from_hint")}</p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="mail-user">{t("mail_username")}</Label>
            <Input id="mail-user" value={draft?.username ?? ""} disabled={disabled} onChange={(e) => set("username", e.target.value)} autoComplete="off" spellCheck={false} />
          </div>
          <div className="space-y-2">
            <Label htmlFor="mail-password">{t("mail_password")}</Label>
            <Input id="mail-password" type="password" value={draft?.password ?? ""} disabled={disabled} onChange={(e) => set("password", e.target.value)} placeholder={saved?.password_set ? t("mail_password_kept") : ""} autoComplete="new-password" />
          </div>
          <div className="space-y-2">
            <Label htmlFor="mail-cap">{t("mail_cap")}</Label>
            <Input id="mail-cap" type="number" min={0} max={100000} step={1} value={draft?.maxPerHour ?? ""} disabled={disabled} onChange={(e) => set("maxPerHour", e.target.value)} />
            <p className="text-xs leading-relaxed text-muted-foreground">{t("mail_cap_hint")}</p>
          </div>
        </div>
        <label htmlFor="mail-tls" className="flex min-h-12 cursor-pointer items-center justify-between gap-4 border-t border-border/60 pt-4 text-sm">
          <span className="space-y-1"><span className="block">{t("mail_tls")}</span><span className="block text-xs text-muted-foreground">{t("mail_tls_hint")}</span></span>
          <Switch id="mail-tls" checked={draft?.requireTLS ?? true} disabled={disabled} onCheckedChange={(value) => set("requireTLS", value)} />
        </label>
        {message && <MessageLine kind={message.kind} message={message.text} />}
        {confirmClear && (
          <div className="space-y-2 rounded-md border p-3">
            <p className="text-sm text-muted-foreground">{t("mail_clear_confirm")}</p>
            <InlineConfirm open confirming={busy === "clear"} onConfirm={() => void run("clear")} onCancel={() => setConfirmClear(false)} confirmLabel={t("mail_clear")} cancelLabel={t("common:cancel")} className="flex justify-end gap-2" />
          </div>
        )}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t(dirty ? "mail_unsaved" : "mail_save_hint")}</p>
        <div className="flex flex-wrap gap-2">
          {saved?.source === "panel" && !confirmClear && (
            <Button variant="outline" disabled={disabled} onClick={() => setConfirmClear(true)}><RotateCcw />{t("mail_clear")}</Button>
          )}
          <Button variant="outline" disabled={disabled || !valid} onClick={() => void run("test")}>{busy === "test" ? <Loader2 className="animate-spin" /> : <Send />}{t("mail_test")}</Button>
          <Button disabled={disabled || !valid || !dirty} onClick={() => void run("save")}>{busy === "save" ? <Loader2 className="animate-spin" /> : <Save />}{t(busy === "save" ? "mail_saving" : "mail_save")}</Button>
        </div>
      </CardFooter>
      {reauth.dialog}
    </Card>
  );
}
