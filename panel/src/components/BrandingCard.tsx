import { useEffect, useRef, useState } from "react";
import { Loader2, Palette, Save, Trash2, Upload } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useBranding, useRefreshBranding } from "@/lib/branding";
import { useAsync, useRefreshSignal, useUnsavedGuard } from "@/lib/hooks";
import type { BrandingImageKind, BrandingSettings } from "@/lib/types";

const NAME_MAX = 40;
const NOTICE_MAX = 1000;
const IMAGE_TYPES = ["image/png", "image/webp"];
const IMAGE_MAX_BYTES: Record<BrandingImageKind, number> = { logo: 256 << 10, background: 2 << 20 };

const characters = (text: string) => [...text].length;
const hasControl = (text: string) => /[\u0000-\u001F\u007F-\u009F]/.test(text);
const hasControlBesidesLineBreaks = (text: string) => /[\u0000-\u0009\u000B\u000C\u000E-\u001F\u007F-\u009F]/.test(text);

export function BrandingCard() {
  const { t } = useTranslation("admin");
  const query = useAsync(api.getBrandingSettings, []);
  const reauth = useReauth();
  const live = useBranding();
  const refreshBranding = useRefreshBranding();
  const [saved, setSaved] = useState<BrandingSettings | null>(null);
  const [name, setName] = useState("");
  const [notice, setNotice] = useState("");
  const [poweredBy, setPoweredBy] = useState(true);
  const [busy, setBusy] = useState<"save" | BrandingImageKind | null>(null);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  function load(settings: BrandingSettings) {
    setSaved(settings);
    setName(settings.name);
    setNotice(settings.notice);
    setPoweredBy(settings.show_powered_by);
  }
  useEffect(() => {
    if (query.data) load(query.data);
  }, [query.data]);

  const dirty = saved !== null && (name !== saved.name || notice !== saved.notice || poweredBy !== saved.show_powered_by);
  useUnsavedGuard(dirty);
  useRefreshSignal(query.reload, dirty);
  const nameOk = characters(name) <= NAME_MAX && !hasControl(name);
  const noticeLeft = NOTICE_MAX - characters(notice);
  const noticeControl = hasControlBesidesLineBreaks(notice);
  const noticeOk = noticeLeft >= 0 && !noticeControl;
  const valid = nameOk && noticeOk;
  const disabled = !saved || busy !== null;

  async function save() {
    if (!saved || !dirty || !valid || busy) return;
    setBusy("save");
    setMessage(null);
    try {
      load(await reauth.guard(() => api.setBrandingSettings({ name, notice, show_powered_by: poweredBy, revision: saved.revision })));
      refreshBranding();
      setMessage({ kind: "success", text: t("branding_saved") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setBusy(null);
    }
  }

  async function changeImage(kind: BrandingImageKind, change: () => Promise<BrandingSettings>, done: string) {
    if (busy) return;
    setBusy(kind);
    setMessage(null);
    try {
      setSaved(await reauth.guard(change));
      refreshBranding();
      setMessage({ kind: "success", text: t(done) });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setBusy(null);
    }
  }

  function upload(kind: BrandingImageKind, file: File) {
    if (!IMAGE_TYPES.includes(file.type)) {
      setMessage({ kind: "error", text: t("branding_image_type") });
    } else if (file.size > IMAGE_MAX_BYTES[kind]) {
      setMessage({ kind: "error", text: t(`branding_${kind}_too_large`) });
    } else {
      void changeImage(kind, () => api.uploadBrandingImage(kind, file), "branding_image_uploaded");
    }
  }

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <span className="shrink-0 text-primary"><Palette className="h-4 w-4" aria-hidden="true" /></span>
        <CardTitle>{t("branding_title")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-6">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("branding_description")}</p>
        {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("branding_loading")}</p>}
        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        <div className="space-y-2 md:max-w-sm">
          <Label htmlFor="branding-name">{t("branding_name_label")}</Label>
          <Input id="branding-name" value={name} disabled={disabled} placeholder="Felis" autoComplete="off" aria-invalid={!nameOk} aria-describedby="branding-name-hint"
            onChange={(e) => { setName(e.target.value); setMessage(null); }} />
          <p id="branding-name-hint" className={nameOk ? "text-xs leading-relaxed text-muted-foreground" : "text-xs leading-relaxed text-destructive"}>{t(nameOk ? "branding_name_hint" : "branding_name_invalid")}</p>
        </div>
        <div className="space-y-2">
          <Label htmlFor="branding-notice">{t("branding_notice_label")}</Label>
          <Textarea id="branding-notice" rows={4} value={notice} disabled={disabled} aria-invalid={!noticeOk} aria-describedby="branding-notice-hint branding-notice-count"
            onChange={(e) => { setNotice(e.target.value); setMessage(null); }} />
          <p id="branding-notice-hint" className="text-xs leading-relaxed text-muted-foreground">{t("branding_notice_hint")}</p>
          <p id="branding-notice-count" className={noticeOk ? "text-xs text-muted-foreground" : "text-xs text-destructive"}>
            {noticeControl ? t("branding_notice_control") : noticeLeft >= 0 ? t("branding_notice_remaining", { n: noticeLeft }) : t("branding_notice_over", { n: -noticeLeft })}
          </p>
        </div>
        <div className="flex items-start justify-between gap-4 border-t border-border pt-5">
          <div className="space-y-1">
            <Label htmlFor="branding-powered-by">{t("branding_powered_by_label")}</Label>
            <p className="text-xs leading-relaxed text-muted-foreground">{t("branding_powered_by_hint")}</p>
          </div>
          <Switch id="branding-powered-by" checked={poweredBy} disabled={disabled} onCheckedChange={(checked) => { setPoweredBy(checked); setMessage(null); }} />
        </div>
        <div className="grid gap-6 border-t border-border pt-5 md:grid-cols-2">
          {(["logo", "background"] as const).map((kind) => (
            <BrandingImage key={kind} kind={kind} present={saved?.[kind === "logo" ? "has_logo" : "has_background"] ?? false} preview={live[kind]} busy={busy === kind} disabled={disabled}
              onPick={(file) => upload(kind, file)}
              onRemove={() => void changeImage(kind, () => api.removeBrandingImage(kind), "branding_image_removed")} />
          ))}
        </div>
        <p className="text-xs leading-relaxed text-muted-foreground">{t("branding_images_note")}</p>
        {message && <MessageLine kind={message.kind} message={message.text} />}
      </CardContent>
      <CardFooter className="flex-wrap gap-2">
        <p className="text-xs text-muted-foreground">{t(dirty ? "platform_unsaved" : "branding_live")}</p>
        <div className="flex flex-wrap gap-2">
          {dirty && <Button variant="outline" disabled={busy !== null} onClick={() => { if (saved) load(saved); setMessage(null); }}>{t("platform_discard")}</Button>}
          <Button disabled={disabled || !dirty || !valid} onClick={() => void save()}>{busy === "save" ? <Loader2 className="animate-spin" /> : <Save />}{t("branding_save")}</Button>
        </div>
      </CardFooter>
      {reauth.dialog}
    </Card>
  );
}

function BrandingImage({ kind, present, preview, busy, disabled, onPick, onRemove }: {
  kind: BrandingImageKind;
  present: boolean;
  preview: string | null;
  busy: boolean;
  disabled: boolean;
  onPick: (file: File) => void;
  onRemove: () => void;
}) {
  const { t } = useTranslation("admin");
  const input = useRef<HTMLInputElement>(null);
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <p className="text-sm font-medium">{t(`branding_${kind}_label`)}</p>
        <Badge variant={present ? "default" : "muted"}>{t(present ? "branding_image_set" : "branding_image_unset")}</Badge>
      </div>
      {present && preview && (
        <div className="rounded-md border border-border bg-muted/40 p-3">
          <img src={preview} alt={t(`branding_${kind}_preview`)} className={kind === "logo" ? "h-16 w-auto max-w-full object-contain" : "h-32 w-full rounded object-cover"} />
        </div>
      )}
      <p className="text-xs leading-relaxed text-muted-foreground">{t(`branding_${kind}_hint`)}</p>
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" disabled={disabled} onClick={() => input.current?.click()}>
          {busy ? <Loader2 className="animate-spin" /> : <Upload />}{t(`branding_${kind}_upload`)}
        </Button>
        {present && <Button variant="outline" disabled={disabled} onClick={onRemove}><Trash2 />{t(`branding_${kind}_remove`)}</Button>}
        <input ref={input} type="file" accept={IMAGE_TYPES.join(",")} hidden aria-label={t(`branding_${kind}_upload`)}
          onChange={(e) => { const file = e.target.files?.[0]; e.target.value = ""; if (file) onPick(file); }} />
      </div>
    </div>
  );
}
