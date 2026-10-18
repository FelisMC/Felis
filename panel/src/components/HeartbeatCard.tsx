import { useEffect, useState } from "react";
import { Activity, Loader2, Save, Trash2 } from "lucide-react";
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
import { useAsync, useRefreshSignal, useUnsavedGuard } from "@/lib/hooks";
import type { HeartbeatSettings } from "@/lib/types";

/** looksLikeURL mirrors the server's check: an http(s) URL with a host and no spaces. */
function looksLikeURL(s: string): boolean {
  try {
    const u = new URL(s);
    return (u.protocol === "http:" || u.protocol === "https:") && u.host !== "" && !/\s/.test(s);
  } catch {
    return false;
  }
}

// Expose only heartbeat scheme and host because its path contains the check secret.
export function HeartbeatCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getHeartbeat, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<HeartbeatSettings | null>(null);
  const [url, setURL] = useState("");
  const [busy, setBusy] = useState<"save" | "clear" | null>(null);
  const [confirmClear, setConfirmClear] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  useEffect(() => {
    if (query.data) setSaved(query.data);
  }, [query.data]);
  const typed = url.trim();
  useUnsavedGuard(typed !== "");
  useRefreshSignal(query.reload);
  const valid = looksLikeURL(typed);

  async function run(kind: "save" | "clear") {
    if (!saved || busy) return;
    setBusy(kind);
    setMessage(null);
    try {
      const next = await reauth.guard(() => (kind === "save" ? api.setHeartbeat(typed, saved.revision) : api.clearHeartbeat()));
      setSaved(next);
      setURL("");
      setConfirmClear(false);
      setMessage({ kind: "success", text: t(kind === "save" ? "heartbeat_saved" : "heartbeat_cleared") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setBusy(null);
    }
  }

  const disabled = !saved || busy !== null;
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><Activity className="h-4 w-4" /></span>
        <CardTitle>{t("heartbeat_title")}</CardTitle>
        {saved && <Badge variant={saved.set ? "default" : "muted"}>{t(saved.set ? "heartbeat_set" : "heartbeat_unset")}</Badge>}
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("heartbeat_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("heartbeat_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        {saved?.set && <p className="text-sm">{t("heartbeat_current", { url: saved.display })}</p>}
        <div className="space-y-2">
          <Label htmlFor="heartbeat-url">{t(saved?.set ? "heartbeat_replace" : "heartbeat_url")}</Label>
          <Input id="heartbeat-url" type="url" inputMode="url" value={url} disabled={disabled} placeholder="https://hc-ping.com/…" autoComplete="off" spellCheck={false} aria-invalid={typed !== "" && !valid}
            onChange={(e) => { setURL(e.target.value); setMessage(null); }} />
          <p className={typed !== "" && !valid ? "text-xs leading-relaxed text-destructive" : "text-xs leading-relaxed text-muted-foreground"}>{t("heartbeat_url_hint")}</p>
        </div>
        {message && <MessageLine kind={message.kind} message={message.text} />}
        {confirmClear && (
          <div className="space-y-2 rounded-md border p-3">
            <p className="text-sm text-muted-foreground">{t("heartbeat_clear_confirm")}</p>
            <InlineConfirm open confirming={busy === "clear"} onConfirm={() => void run("clear")} onCancel={() => setConfirmClear(false)} confirmLabel={t("heartbeat_clear")} cancelLabel={t("common:cancel")} className="flex justify-end gap-2" />
          </div>
        )}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t("heartbeat_footer")}</p>
        <div className="flex flex-wrap gap-2">
          {saved?.set && !confirmClear && <Button variant="outline" disabled={disabled} onClick={() => setConfirmClear(true)}><Trash2 />{t("heartbeat_clear")}</Button>}
          <Button disabled={disabled || !valid} onClick={() => void run("save")}>{busy === "save" ? <Loader2 className="animate-spin" /> : <Save />}{t("heartbeat_save")}</Button>
        </div>
      </CardFooter>
      {reauth.dialog}
    </Card>
  );
}
