import { useEffect, useState } from "react";
import { Boxes, DoorOpen, Loader2, Save, SlidersHorizontal, Users, Shield } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useFeatures } from "@/lib/features";
import { useAsync, useRefreshSignal, useUnsavedGuard } from "@/lib/hooks";
import type { PlatformFeaturesSettings, PlatformMode } from "@/lib/types";
import { cn } from "@/lib/utils";

const MODES: { value: PlatformMode; icon: typeof Shield }[] = [
  { value: "team", icon: Shield },
  { value: "community", icon: Users },
];

const INITIAL: PlatformFeaturesSettings = {
  mode: "team",
  useLobby: false,
  lobbyAvailable: false,
  customImages: false,
  allowSubmissions: false,
  requireAccountLink: false,
  revision: "",
};

function draftEquals(a: PlatformFeaturesSettings, b: PlatformFeaturesSettings): boolean {
  return (
    a.mode === b.mode &&
    a.useLobby === b.useLobby &&
    a.customImages === b.customImages &&
    a.allowSubmissions === b.allowSubmissions
  );
}

export function PlatformModeCard({ onApplied }: { onApplied?: () => void }) {
  const { t } = useTranslation("admin");
  const { apply } = useFeatures();
  const query = useAsync(api.getFeatures, []);
  const reauth = useReauth();
  const [saved, setSaved] = useState<PlatformFeaturesSettings | null>(null);
  const [draft, setDraft] = useState<PlatformFeaturesSettings>(INITIAL);
  const [saving, setSaving] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [syncLink, setSyncLink] = useState(true);
  const [syncSubmissions, setSyncSubmissions] = useState(true);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  const dirty = saved !== null && !draftEquals(draft, saved);
  const modeChanged = saved !== null && draft.mode !== saved.mode;
  const busy = query.loading || saving || !saved;

  useUnsavedGuard(dirty);
  useRefreshSignal(query.reload, dirty);

  useEffect(() => {
    if (!query.data) return;
    setSaved(query.data);
    setDraft(query.data);
  }, [query.data]);

  function change(update: Partial<PlatformFeaturesSettings>) {
    setDraft((prev) => ({ ...prev, ...update }));
    setMessage(null);
  }

  async function commit(opts?: { syncRequireAccountLink?: boolean; allowSubmissions?: boolean }) {
    if (!saved || !dirty || busy) return;
    setSaving(true);
    setMessage(null);
    try {
      const next = await reauth.guard(() =>
        api.setFeatures({
          mode: draft.mode,
          useLobby: saved.lobbyAvailable ? draft.useLobby : false,
          customImages: draft.customImages,
          allowSubmissions: opts?.allowSubmissions ?? draft.allowSubmissions,
          syncRequireAccountLink: opts?.syncRequireAccountLink,
          revision: saved.revision,
        }),
      );
      setSaved(next);
      setDraft(next);
      setConfirmOpen(false);
      apply(next);
      onApplied?.();
      setMessage({ kind: "success", text: t("features_saved") });
    } catch (err) {
      if (!isReauthCancelled(err)) setMessage({ kind: "error", text: humanizeError(err) });
    } finally {
      setSaving(false);
    }
  }

  function handleSaveClick() {
    if (!dirty || busy) return;
    if (modeChanged) {
      setSyncLink(true);
      setSyncSubmissions(true);
      setConfirmOpen(true);
      return;
    }
    void commit();
  }

  function handleConfirmModeSwitch() {
    const nextAllowSubmissions =
      draft.customImages && syncSubmissions ? draft.mode === "community" : draft.allowSubmissions;
    void commit({
      syncRequireAccountLink: syncLink,
      allowSubmissions: nextAllowSubmissions,
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <SlidersHorizontal className="h-4 w-4 text-primary" aria-hidden="true" />
          {t("features_title")}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4" aria-busy={query.loading}>
        {query.loading && (
          <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            {t("features_loading")}
          </p>
        )}

        <div className="grid gap-3 sm:grid-cols-2" role="group" aria-label={t("features_mode_label")}>
          {MODES.map(({ value, icon: Icon }) => {
            const active = draft.mode === value;
            return (
              <button
                key={value}
                type="button"
                aria-pressed={active}
                disabled={busy}
                onClick={() => change({ mode: value })}
                className={cn(
                  "flex flex-col items-start gap-1 rounded-lg border px-3.5 py-2.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50",
                  active
                    ? "border-primary/45 bg-primary/5 ring-1 ring-primary/20"
                    : "border-border hover:border-muted-foreground/30 hover:bg-accent/50",
                )}
              >
                <span className="flex items-center gap-2 text-sm font-medium text-foreground">
                  <Icon className={cn("h-4 w-4", active ? "text-primary" : "text-muted-foreground")} aria-hidden="true" />
                  {t(`features_mode_${value}`)}
                </span>
                <span className="text-xs leading-relaxed text-muted-foreground">
                  {t(`features_mode_${value}_hint`)}
                </span>
              </button>
            );
          })}
        </div>

        <div className="grid gap-3 sm:grid-cols-2">
          <div className="flex items-start justify-between gap-3 rounded-lg border border-border px-3.5 py-2.5">
            <div className="min-w-0 space-y-0.5">
              <Label htmlFor="features-use-lobby" className="flex items-center gap-1.5 text-sm">
                <DoorOpen className="h-3.5 w-3.5 text-muted-foreground" aria-hidden="true" />
                {t("features_lobby_label")}
              </Label>
              <p className="text-xs leading-relaxed text-muted-foreground">
                {t(saved && !saved.lobbyAvailable ? "features_lobby_unavailable" : "features_lobby_hint")}
              </p>
            </div>
            <Switch
              id="features-use-lobby"
              checked={saved?.lobbyAvailable ? draft.useLobby : false}
              disabled={busy || !saved?.lobbyAvailable}
              onCheckedChange={(checked) => change({ useLobby: checked })}
            />
          </div>

          <div className="flex items-start justify-between gap-3 rounded-lg border border-border px-3.5 py-2.5">
            <div className="min-w-0 space-y-0.5">
              <Label htmlFor="features-custom-images" className="flex items-center gap-1.5 text-sm">
                <Boxes className="h-3.5 w-3.5 text-muted-foreground" aria-hidden="true" />
                {t("features_custom_images_label")}
              </Label>
              <p className="text-xs leading-relaxed text-muted-foreground">
                {t("features_custom_images_hint")}
              </p>
            </div>
            <Switch
              id="features-custom-images"
              checked={draft.customImages}
              disabled={busy}
              onCheckedChange={(checked) => change({ customImages: checked })}
            />
          </div>
        </div>

        {query.error != null && <MessageLine kind="error" message={humanizeError(query.error)} />}
        {message && <MessageLine kind={message.kind} message={message.text} />}
      </CardContent>
      <CardFooter>
        <p className="text-xs text-muted-foreground">
          {t(dirty ? "platform_unsaved" : "features_scope")}
        </p>
        <div className="flex gap-2">
          {dirty && (
            <Button
              variant="outline"
              disabled={saving}
              onClick={() => {
                if (saved) setDraft(saved);
                setMessage(null);
              }}
            >
              {t("platform_discard")}
            </Button>
          )}
          <Button disabled={busy || !dirty} onClick={handleSaveClick}>
            {saving ? <Loader2 className="animate-spin" /> : <Save />}
            {t("features_save")}
          </Button>
        </div>
      </CardFooter>

      <Dialog open={confirmOpen} onOpenChange={(o) => !saving && setConfirmOpen(o)}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t(`features_switch_title_${draft.mode}`)}</DialogTitle>
            <DialogDescription>{t(`features_switch_desc_${draft.mode}`)}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3 py-1 text-sm">
            <label className="flex cursor-pointer items-start gap-2.5">
              <Checkbox
                className="mt-0.5"
                checked={syncLink}
                disabled={saving}
                onChange={(e) => setSyncLink(e.target.checked)}
              />
              <span>
                <span className="block font-medium text-foreground">
                  {t(`features_switch_sync_link_${draft.mode}`)}
                </span>
                <span className="block text-xs text-muted-foreground">
                  {t(`features_switch_sync_link_${draft.mode}_hint`)}
                </span>
              </span>
            </label>
            {draft.customImages && (
              <label className="flex cursor-pointer items-start gap-2.5">
                <Checkbox
                  className="mt-0.5"
                  checked={syncSubmissions}
                  disabled={saving}
                  onChange={(e) => setSyncSubmissions(e.target.checked)}
                />
                <span>
                  <span className="block font-medium text-foreground">
                    {t(`features_switch_sync_submissions_${draft.mode}`)}
                  </span>
                  <span className="block text-xs text-muted-foreground">
                    {t(`features_switch_sync_submissions_${draft.mode}_hint`)}
                  </span>
                </span>
              </label>
            )}
          </div>
          {message?.kind === "error" && <MessageLine kind="error" message={message.text} compact />}
          <ConfirmFooter
            onCancel={() => setConfirmOpen(false)}
            onConfirm={handleConfirmModeSwitch}
            disabled={saving}
            loading={saving}
            cancelLabel={t("common:cancel")}
            confirmLabel={t("features_switch_confirm")}
            confirmVariant="default"
          />
        </DialogContent>
      </Dialog>
      {reauth.dialog}
    </Card>
  );
}
