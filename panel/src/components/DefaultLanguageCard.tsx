import { useEffect, useState } from "react";
import { Languages, Loader2, Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useRefreshSignal, useUnsavedGuard } from "@/lib/hooks";
import type { DefaultLanguageSettings, PanelLanguageSetting } from "@/lib/types";

// Select items cannot carry an empty value, so "follow the browser" uses a placeholder.
const FOLLOW_BROWSER = "__browser__";

export function DefaultLanguageCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getDefaultLanguage, []);
  const [saved, setSaved] = useState<DefaultLanguageSettings | null>(null);
  const [draft, setDraft] = useState<PanelLanguageSetting>("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  useEffect(() => {
    if (query.data) {
      setSaved(query.data);
      setDraft(query.data.language);
    }
  }, [query.data]);
  const dirty = saved !== null && draft !== saved.language;
  useUnsavedGuard(dirty);
  useRefreshSignal(query.reload, dirty);
  const disabled = !saved || saving;

  async function save() {
    if (!saved || !dirty || saving) return;
    setSaving(true);
    setMessage(null);
    try {
      const next = await api.setDefaultLanguage({ language: draft, revision: saved.revision });
      setSaved(next);
      setDraft(next.language);
      setMessage({ kind: "success", text: t("default_language_saved") });
    } catch (error) {
      setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><Languages className="h-4 w-4" aria-hidden="true" /></span>
        <CardTitle>{t("default_language_title")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("default_language_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("default_language_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        <div className="space-y-2 md:max-w-sm">
          <Label htmlFor="default-language">{t("default_language_label")}</Label>
          <Select value={draft || FOLLOW_BROWSER} disabled={disabled} onValueChange={(value) => { setDraft(value === FOLLOW_BROWSER ? "" : (value as PanelLanguageSetting)); setMessage(null); }}>
            <SelectTrigger id="default-language"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value={FOLLOW_BROWSER}>{t("default_language_browser")}</SelectItem>
              <SelectItem value="zh-CN">简体中文</SelectItem>
              <SelectItem value="en-US">English</SelectItem>
            </SelectContent>
          </Select>
          <p className="text-xs leading-relaxed text-muted-foreground">{t("default_language_hint")}</p>
        </div>
        {message && <MessageLine kind={message.kind} message={message.text} />}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t(dirty ? "platform_unsaved" : "default_language_live")}</p>
        <div className="flex flex-wrap gap-2">
          {dirty && <Button variant="outline" disabled={saving} onClick={() => { if (saved) setDraft(saved.language); setMessage(null); }}>{t("platform_discard")}</Button>}
          <Button disabled={disabled || !dirty} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t("default_language_save")}</Button>
        </div>
      </CardFooter>
    </Card>
  );
}
