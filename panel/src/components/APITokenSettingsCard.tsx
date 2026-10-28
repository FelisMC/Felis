import { useEffect, useState } from "react";
import { KeyRound, Loader2, Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useRefreshSignal, useUnsavedGuard } from "@/lib/hooks";
import type { APITokenSettings } from "@/lib/types";

export function APITokenSettingsCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getAPITokenSettings, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<APITokenSettings | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  useEffect(() => {
    if (query.data) {
      setSaved(query.data);
      setEnabled(query.data.enabled);
    }
  }, [query.data]);
  const dirty = saved !== null && enabled !== saved.enabled;
  useUnsavedGuard(dirty);
  useRefreshSignal(query.reload, dirty);
  const disabled = !saved || saving;

  async function save() {
    if (!saved || !dirty || saving) return;
    setSaving(true);
    setMessage(null);
    try {
      const next = await reauth.guard(() => api.setAPITokenSettings({ enabled, revision: saved.revision }));
      setSaved(next);
      setEnabled(next.enabled);
      setMessage({ kind: "success", text: t("api_tokens_saved") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><KeyRound className="h-4 w-4" aria-hidden="true" /></span>
        <CardTitle>{t("api_tokens_title")}</CardTitle>
        {saved && <Badge variant={saved.enabled ? "default" : "muted"}>{t(saved.enabled ? "api_tokens_on" : "api_tokens_off")}</Badge>}
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("api_tokens_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("api_tokens_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        <label htmlFor="api-tokens-enabled" className="flex min-h-12 cursor-pointer items-center justify-between gap-4 text-sm">
          <span className="space-y-1"><span className="block">{t("api_tokens_enabled")}</span><span className="block text-xs leading-relaxed text-muted-foreground">{t("api_tokens_enabled_hint")}</span></span>
          <Switch id="api-tokens-enabled" checked={enabled} disabled={disabled} onCheckedChange={(value) => { setEnabled(value); setMessage(null); }} />
        </label>
        {message && <MessageLine kind={message.kind} message={message.text} />}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t(dirty ? "failover_unsaved" : "failover_live")}</p>
        <div className="flex flex-wrap gap-2">
          {dirty && <Button variant="outline" disabled={saving} onClick={() => { if (saved) setEnabled(saved.enabled); setMessage(null); }}>{t("platform_discard")}</Button>}
          <Button disabled={disabled || !dirty} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t("api_tokens_save")}</Button>
        </div>
      </CardFooter>
      {reauth.dialog}
    </Card>
  );
}
