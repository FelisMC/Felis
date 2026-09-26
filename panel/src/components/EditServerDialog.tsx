import { useState, useEffect } from "react";
import { Settings, ChevronRight, AlertTriangle } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError } from "@/lib/api";
import { splitImageRef } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import type { AutostartPolicy } from "@/lib/types";
import { InlineError } from "@/components/MessageLine";

const MEMORY_OPTIONS = ["2Gi", "4Gi", "6Gi", "8Gi"];

/** cpuValid accepts what the API takes as a CPU limit: a positive number of
 *  cores ("2", "1.5") or millicores ("500m"). Empty is valid too: no limit. */
function cpuValid(cpu: string): boolean {
  if (cpu === "") return true;
  const m = /^(\d+(?:\.\d+)?)(m?)$/.exec(cpu);
  return m !== null && Number(m[1]) > 0 && (m[2] === "" || !m[1].includes("."));
}

/** Idle auto-stop presets in seconds; "0" is Never. The server default is 600. */
const IDLE_OPTIONS = ["0", "300", "600", "900", "1800", "3600", "7200"];

/** idleLabel renders an idle-stop duration the way a person says it. */
function idleLabel(t: (key: string, opts?: Record<string, unknown>) => string, seconds: number): string {
  if (seconds <= 0) return t("idle_stop_never");
  if (seconds % 3600 === 0) return t("idle_stop_hours", { count: seconds / 3600 });
  if (seconds % 60 === 0) return t("idle_stop_minutes", { count: seconds / 60 });
  return t("idle_stop_seconds", { count: seconds });
}

/** ImageLabel shows an image ref as the tag it was picked by, plus the short id of
 *  the build a pinned ref is locked to. */
function ImageLabel({ imageRef, t }: { imageRef: string; t: (key: string, opts?: Record<string, unknown>) => string }) {
  const { tag, short } = splitImageRef(imageRef);
  return (
    <>
      {tag}
      {short && (
        <span className="ml-2 font-mono text-xs text-muted-foreground">
          {t("edit_server_image_build", { id: short })}
        </span>
      )}
    </>
  );
}

function policyOptions(t: (key: string) => string): { value: AutostartPolicy; label: string }[] {
  return [
    { value: "ownerOnly", label: t("create_server_policy_owner") },
    { value: "public", label: t("create_server_policy_public") },
    { value: "allowlist", label: t("create_server_policy_allowlist") },
  ];
}

interface EditServerForm {
  displayName: string;
  autostartPolicy: AutostartPolicy;
  image: string;
  memory: string;
  cpu: string;
  /** Idle auto-stop seconds as a Select value; "0" is Never. */
  idleStop: string;
}

interface Props {
  serverName: string;
  currentDisplayName?: string;
  currentPolicy?: AutostartPolicy;
  currentImage?: string;
  currentMemory?: string;
  currentStorage?: string;
  currentCpu?: string;
  currentIdleStopSeconds?: number;
  /** The operator cannot read the player count, so idle stop is paused. */
  playerCountUnknown?: boolean;
  onUpdated: () => void;
}

export function EditServerDialog({
  serverName,
  currentDisplayName = "",
  currentPolicy = "ownerOnly",
  currentImage = "",
  currentMemory = "",
  currentStorage = "",
  currentCpu = "",
  currentIdleStopSeconds = 0,
  playerCountUnknown = false,
  onUpdated,
}: Props) {
  const currentIdleStop = String(currentIdleStopSeconds);
  const { t } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  const images = useAsync(() => api.listImages(), []);

  const [form, setForm] = useState<EditServerForm>({
    displayName: currentDisplayName,
    autostartPolicy: currentPolicy,
    image: currentImage,
    memory: currentMemory,
    cpu: currentCpu,
    idleStop: currentIdleStop,
  });

  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // An image change moves the world to another build for good, so saving one waits
  // for this acknowledgement (the API refuses it with image_change_unconfirmed).
  const [imageConfirmed, setImageConfirmed] = useState(false);

  // Sync form state when dialog opens or current values change from server status
  useEffect(() => {
    if (open) {
      setForm({
        displayName: currentDisplayName,
        autostartPolicy: currentPolicy,
        image: currentImage,
        memory: currentMemory,
        cpu: currentCpu,
        idleStop: currentIdleStop,
      });
      setError(null);
      setImageConfirmed(false);
    }
  }, [open, currentDisplayName, currentPolicy, currentImage, currentMemory, currentCpu, currentIdleStop]);

  function set<K extends keyof EditServerForm>(k: K, v: EditServerForm[K]) {
    setForm((f) => ({ ...f, [k]: v }));
    if (k === "image") setImageConfirmed(false);
  }

  const enabledImages = (images.data ?? []).filter((i) => i.enabled);

  // Text fields count as what they send: surrounding space is no change, and an
  // emptied field is one (it clears the name, or lifts the CPU limit).
  const displayName = form.displayName.trim();
  const cpu = form.cpu.trim();
  const cpuOk = cpuValid(cpu);
  const hasChanges =
    displayName !== currentDisplayName ||
    form.autostartPolicy !== currentPolicy ||
    form.image !== currentImage ||
    form.memory !== currentMemory ||
    cpu !== currentCpu ||
    form.idleStop !== currentIdleStop;

  const imageChanged = form.image !== currentImage;
  const pinnedBuild = splitImageRef(currentImage).short;
  const canSubmit = hasChanges && cpuOk && !submitting && (!imageChanged || imageConfirmed);

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const payload: Parameters<typeof api.patchServer>[1] = {};

      if (displayName !== currentDisplayName) {
        payload.displayName = displayName;
      }
      if (form.autostartPolicy !== currentPolicy) {
        payload.autostartPolicy = form.autostartPolicy;
      }
      if (imageChanged) {
        payload.image = form.image;
        payload.confirmImageChange = imageConfirmed;
      }
      // Memory and CPU each go alone; the API keeps whatever is not sent.
      if (form.memory !== currentMemory) {
        payload.memory = form.memory;
      }
      if (cpu !== currentCpu) {
        payload.resources = { cpu };
      }
      if (form.idleStop !== currentIdleStop) {
        payload.idleStopSeconds = Number(form.idleStop);
      }

      await api.patchServer(serverName, payload);
      setOpen(false);
      onUpdated();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setSubmitting(false);
    }
  }

  const policies = policyOptions(t);

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {/* Unified Sidebar Link/Card style trigger */}
        <button
          type="button"
          className="group flex w-full text-left items-center gap-3 rounded-lg border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent focus:outline-none"
        >
          <Settings className="h-5 w-5 shrink-0 text-primary" />
          <div className="min-w-0 flex-1 text-left">
            <p className="text-sm font-medium">{t("edit_server_title")}</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              {t("edit_server_desc")}
            </p>
          </div>
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
        </button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("edit_server_title")}</DialogTitle>
          <DialogDescription>
            {t("edit_server_desc_long")}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="es-display">{t("create_server_display_name")}</Label>
            <Input
              id="es-display"
              placeholder={t("create_server_display_name_placeholder")}
              value={form.displayName}
              onChange={(e) => set("displayName", e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="edit-server-image">{t("create_server_image")}</Label>
            <Select value={form.image} onValueChange={(v) => set("image", v)}>
              <SelectTrigger id="edit-server-image">
                <SelectValue
                  placeholder={
                    images.loading
                      ? t("create_server_image_loading")
                      : enabledImages.length
                        ? t("create_server_image_choose")
                        : t("create_server_image_none")
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {/* Fallback to display the current image even if not in the whitelist options list */}
                {form.image && !enabledImages.some((img) => img.image_ref === form.image) && (
                  <SelectItem value={form.image}>
                    <ImageLabel imageRef={form.image} t={t} />
                  </SelectItem>
                )}
                {currentImage && form.image !== currentImage &&
                  !enabledImages.some((img) => img.image_ref === currentImage) && (
                  <SelectItem value={currentImage}>
                    <ImageLabel imageRef={currentImage} t={t} />
                  </SelectItem>
                )}
                {enabledImages.map((img) => (
                  <SelectItem key={img.image_ref} value={img.image_ref}>
                    {img.image_ref}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {!imageChanged && pinnedBuild && (
              <p className="text-xs text-muted-foreground">
                {t("edit_server_image_pinned_hint", { id: pinnedBuild })}
              </p>
            )}
            {imageChanged && (
              <div className="grid gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs">
                <p className="flex items-start gap-1.5 text-amber-700 dark:text-amber-300">
                  <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
                  {t("edit_server_image_warning")}
                </p>
                <label className="flex cursor-pointer items-center gap-2 font-medium text-foreground">
                  <input
                    type="checkbox"
                    className="h-4 w-4 shrink-0 cursor-pointer accent-primary"
                    checked={imageConfirmed}
                    onChange={(e) => setImageConfirmed(e.target.checked)}
                  />
                  {t("edit_server_image_confirm")}
                </label>
              </div>
            )}
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="edit-server-memory">{t("create_server_memory")}</Label>
              <Select value={form.memory} onValueChange={(v) => set("memory", v)}>
                <SelectTrigger id="edit-server-memory">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {/* Prepend current memory if not present in options */}
                  {form.memory && !MEMORY_OPTIONS.includes(form.memory) && (
                    <SelectItem value={form.memory}>{form.memory}</SelectItem>
                  )}
                  {MEMORY_OPTIONS.map((m) => (
                    <SelectItem key={m} value={m}>
                      {m}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              {/* Storage is immutable but displayed to maintain visual consistency */}
              <Label htmlFor="edit-server-storage">{t("create_server_storage")}</Label>
              <Input id="edit-server-storage" value={currentStorage} disabled className="opacity-60 cursor-not-allowed select-none bg-muted" />
            </div>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="es-cpu">{t("edit_server_cpu")}</Label>
              <Input
                id="es-cpu"
                placeholder={t("edit_server_cpu_placeholder")}
                value={form.cpu}
                onChange={(e) => set("cpu", e.target.value)}
                aria-invalid={!cpuOk}
                aria-describedby={cpuOk ? undefined : "es-cpu-error"}
              />
              {!cpuOk && (
                <p id="es-cpu-error" className="text-xs text-destructive">
                  {t("edit_server_cpu_invalid")}
                </p>
              )}
            </div>
            <div className="grid gap-2">
              <Label htmlFor="edit-server-policy">{t("create_server_policy")}</Label>
              <Select
                value={form.autostartPolicy}
                onValueChange={(v) => set("autostartPolicy", v as AutostartPolicy)}
              >
                <SelectTrigger id="edit-server-policy">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {policies.map((p) => (
                    <SelectItem key={p.value} value={p.value}>
                      {p.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="edit-server-idle">{t("edit_server_idle")}</Label>
            <Select value={form.idleStop} onValueChange={(v) => set("idleStop", v)}>
              <SelectTrigger id="edit-server-idle">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {/* A duration set outside the presets (kubectl, converge) stays selectable. */}
                {!IDLE_OPTIONS.includes(currentIdleStop) && (
                  <SelectItem value={currentIdleStop}>{idleLabel(t, currentIdleStopSeconds)}</SelectItem>
                )}
                {IDLE_OPTIONS.map((s) => (
                  <SelectItem key={s} value={s}>
                    {idleLabel(t, Number(s))}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{t("edit_server_idle_hint")}</p>
            {playerCountUnknown && form.idleStop !== "0" && (
              <p className="flex items-start gap-1.5 text-xs text-amber-600 dark:text-amber-400">
                <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
                {t("edit_server_idle_unknown")}
              </p>
            )}
          </div>

          <InlineError message={error} />
        </div>

        <ConfirmFooter
          onCancel={() => setOpen(false)}
          onConfirm={submit}
          disabled={!canSubmit}
          loading={submitting}
          cancelLabel={t("create_server_cancel")}
          confirmLabel={t("edit_server_submit")}
          confirmVariant="default"
        />
      </DialogContent>
    </Dialog>
  );
}
