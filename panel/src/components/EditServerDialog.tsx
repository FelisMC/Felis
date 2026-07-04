import { useState, useEffect } from "react";
import { Settings, ChevronRight } from "lucide-react";
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
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import type { AutostartPolicy } from "@/lib/types";

const MEMORY_OPTIONS = ["2Gi", "4Gi", "6Gi", "8Gi"];

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
}

interface Props {
  serverName: string;
  currentDisplayName?: string;
  currentPolicy?: AutostartPolicy;
  currentImage?: string;
  currentMemory?: string;
  currentStorage?: string;
  currentCpu?: string;
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
  onUpdated,
}: Props) {
  const { t } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  const images = useAsync(() => api.listImages(), []);

  const [form, setForm] = useState<EditServerForm>({
    displayName: currentDisplayName,
    autostartPolicy: currentPolicy,
    image: currentImage,
    memory: currentMemory,
    cpu: currentCpu,
  });

  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // Sync form state when dialog opens or current values change from server status
  useEffect(() => {
    if (open) {
      setForm({
        displayName: currentDisplayName,
        autostartPolicy: currentPolicy,
        image: currentImage,
        memory: currentMemory,
        cpu: currentCpu,
      });
      setError(null);
    }
  }, [open, currentDisplayName, currentPolicy, currentImage, currentMemory, currentCpu]);

  function set<K extends keyof EditServerForm>(k: K, v: EditServerForm[K]) {
    setForm((f) => ({ ...f, [k]: v }));
  }

  const enabledImages = (images.data ?? []).filter((i) => i.enabled);

  // Checks if form state has mutated from initial values
  const hasChanges =
    form.displayName !== currentDisplayName ||
    form.autostartPolicy !== currentPolicy ||
    form.image !== currentImage ||
    form.memory !== currentMemory ||
    form.cpu !== currentCpu;

  const canSubmit = hasChanges && !submitting;

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const payload: Parameters<typeof api.patchServer>[1] = {};

      if (form.displayName !== currentDisplayName) {
        payload.displayName = form.displayName.trim() || undefined;
      }
      if (form.autostartPolicy !== currentPolicy) {
        payload.autostartPolicy = form.autostartPolicy;
      }
      if (form.image !== currentImage) {
        payload.image = form.image;
      }
      if (form.memory !== currentMemory) {
        payload.memory = form.memory;
      }
      if (form.cpu !== currentCpu) {
        payload.resources = {
          cpu: form.cpu.trim(),
        };
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
            <Label>{t("create_server_image")}</Label>
            <Select value={form.image} onValueChange={(v) => set("image", v)}>
              <SelectTrigger>
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
                  <SelectItem value={form.image}>{form.image}</SelectItem>
                )}
                {enabledImages.map((img) => (
                  <SelectItem key={img.image_ref} value={img.image_ref}>
                    {img.image_ref}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label>{t("create_server_memory")}</Label>
              <Select value={form.memory} onValueChange={(v) => set("memory", v)}>
                <SelectTrigger>
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
              <Label>{t("create_server_storage")}</Label>
              <Input value={currentStorage} disabled className="opacity-60 cursor-not-allowed select-none bg-muted" />
            </div>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="es-cpu">CPU 限制</Label>
              <Input
                id="es-cpu"
                placeholder='例如 1, 2, 500m'
                value={form.cpu}
                onChange={(e) => set("cpu", e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label>{t("create_server_policy")}</Label>
              <Select
                value={form.autostartPolicy}
                onValueChange={(v) => set("autostartPolicy", v as AutostartPolicy)}
              >
                <SelectTrigger>
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

          {error && <p className="text-sm text-destructive">{error}</p>}
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
