import { useState } from "react";
import { Plus } from "lucide-react";
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
import { hostFor, type RuntimeConfig } from "@/lib/config";
import { DISPLAY_NAME_MAX, serverNameIssue, type ServerNameIssue } from "@/lib/naming";
import type { AutostartPolicy, CreateServerRequest } from "@/lib/types";
import { InlineError } from "@/components/MessageLine";

// CreateServerDialog IS the §15 收口 ("structured form, not free YAML"): every
// dangerous knob (hostNetwork, hostPath, arbitrary image, raw resource quantities)
// is simply absent from the UI. The image is chosen from the server-side whitelist
// (GET /images); memory/storage are constrained text the API re-validates. Nothing
// here can express a privileged pod — the form's value space IS the allowlist.

const MEMORY_OPTIONS = ["2Gi", "4Gi", "6Gi", "8Gi"];
const STORAGE_OPTIONS = ["5Gi", "10Gi", "20Gi", "50Gi"];

function policyOptions(t: (key: string) => string): { value: AutostartPolicy; label: string }[] {
  return [
    { value: "ownerOnly", label: t("create_server_policy_owner") },
    { value: "public", label: t("create_server_policy_public") },
    { value: "allowlist", label: t("create_server_policy_allowlist") },
  ];
}

interface Props {
  cfg: RuntimeConfig;
  onCreated: () => void;
}

export function CreateServerDialog({ cfg, onCreated }: Props) {
  const { t } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  // The whitelist is read each time the dialog opens, so an image added, enabled
  // or retired since the page loaded shows up or drops out without a reload.
  const images = useAsync(() => (open ? api.listImages() : Promise.resolve(null)), [open]);

  const [form, setForm] = useState<CreateServerRequest>({
    name: "",
    subdomain: "",
    displayName: "",
    image: "",
    memory: "4Gi",
    storage: "10Gi",
    autostartPolicy: "ownerOnly",
  });
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // A refusal answers the form as it was sent; any edit, or opening the dialog
  // again, starts past it.
  function set<K extends keyof CreateServerRequest>(k: K, v: CreateServerRequest[K]) {
    setForm((f) => ({ ...f, [k]: v }));
    setError(null);
  }

  function openChange(next: boolean) {
    setOpen(next);
    if (next) setError(null);
  }

  const enabledImages = (images.data ?? []).filter((i) => i.enabled);
  // An image chosen before it was disabled or removed no longer counts as chosen.
  const image = enabledImages.some((i) => i.image_ref === form.image) ? form.image : "";
  const nameIssue = serverNameIssue(form.name);
  const subdomainIssue = serverNameIssue(form.subdomain);
  const canSubmit =
    nameIssue === null &&
    subdomainIssue === null &&
    !!image &&
    !!form.memory &&
    !!form.storage &&
    !submitting;

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const payload: CreateServerRequest = {
        ...form,
        displayName: form.displayName?.trim() || undefined,
      };
      await api.createServer(payload);
      setOpen(false);
      onCreated();
      setForm((f) => ({ ...f, name: "", subdomain: "", displayName: "" }));
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setSubmitting(false);
    }
  }

  const policies = policyOptions(t);

  // A field's hint turns into the reason it would be refused once something is typed.
  const issueText = (issue: ServerNameIssue, value: string) =>
    issue === "reserved" ? t("create_server_name_reserved", { name: value }) : t("create_server_name_invalid");
  const hintClass = (value: string, issue: ServerNameIssue | null) =>
    value && issue ? "text-xs text-destructive" : "text-xs text-muted-foreground";

  return (
    <Dialog open={open} onOpenChange={openChange}>
      <DialogTrigger asChild>
        <Button size="sm" className="gap-1.5">
          <Plus className="h-4 w-4" />
          {t("create_server_btn")}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("create_server_title")}</DialogTitle>
          <DialogDescription>
            {t("create_server_desc")}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="cs-name">{t("create_server_name")}</Label>
            <Input
              id="cs-name"
              placeholder={t("create_server_name_placeholder")}
              value={form.name}
              onChange={(e) => set("name", e.target.value.toLowerCase())}
              aria-invalid={!!form.name && nameIssue !== null}
              aria-describedby="cs-name-hint"
            />
            <p id="cs-name-hint" className={hintClass(form.name, nameIssue)}>
              {form.name && nameIssue
                ? issueText(nameIssue, form.name)
                : t("create_server_name_hint")}
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="cs-sub">{t("create_server_subdomain")}</Label>
            <Input
              id="cs-sub"
              placeholder={t("create_server_subdomain_placeholder")}
              value={form.subdomain}
              onChange={(e) => set("subdomain", e.target.value.toLowerCase())}
              aria-invalid={!!form.subdomain && subdomainIssue !== null}
              aria-describedby="cs-sub-hint"
            />
            <p id="cs-sub-hint" className={hintClass(form.subdomain, subdomainIssue)}>
              {form.subdomain && subdomainIssue
                ? issueText(subdomainIssue, form.subdomain)
                : t("create_server_subdomain_hint", { host: hostFor(form.subdomain || "name", cfg) })}
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="cs-display">{t("create_server_display_name")}</Label>
            <Input
              id="cs-display"
              placeholder={t("create_server_display_name_placeholder")}
              maxLength={DISPLAY_NAME_MAX}
              value={form.displayName ?? ""}
              onChange={(e) => set("displayName", e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="create-server-image">{t("create_server_image")}</Label>
            <Select value={image} onValueChange={(v) => set("image", v)}>
              <SelectTrigger id="create-server-image">
                <SelectValue
                  placeholder={
                    images.loading
                      ? t("create_server_image_loading")
                      : images.error
                        ? t("create_server_image_unavailable")
                        : enabledImages.length
                          ? t("create_server_image_choose")
                          : t("create_server_image_none")
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {enabledImages.map((img) => (
                  <SelectItem key={img.image_ref} value={img.image_ref}>
                    {img.image_ref}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {!!images.error && (
              <div role="alert" className="flex items-center justify-between gap-3 text-xs text-destructive">
                <span>{t("create_server_image_failed", { reason: humanizeError(images.error) })}</span>
                <Button variant="outline" size="sm" className="shrink-0" onClick={images.reload}>
                  {t("common:try_again")}
                </Button>
              </div>
            )}
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="create-server-memory">{t("create_server_memory")}</Label>
              <Select value={form.memory} onValueChange={(v) => set("memory", v)}>
                <SelectTrigger id="create-server-memory">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MEMORY_OPTIONS.map((m) => (
                    <SelectItem key={m} value={m}>
                      {m}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="create-server-storage">{t("create_server_storage")}</Label>
              <Select value={form.storage} onValueChange={(v) => set("storage", v)}>
                <SelectTrigger id="create-server-storage">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {STORAGE_OPTIONS.map((s) => (
                    <SelectItem key={s} value={s}>
                      {s}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="create-server-policy">{t("create_server_policy")}</Label>
            <Select
              value={form.autostartPolicy}
              onValueChange={(v) => set("autostartPolicy", v as AutostartPolicy)}
            >
              <SelectTrigger id="create-server-policy">
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

          <InlineError message={error} />
        </div>

        <ConfirmFooter
          onCancel={() => setOpen(false)}
          onConfirm={submit}
          disabled={!canSubmit}
          loading={submitting}
          cancelLabel={t("create_server_cancel")}
          confirmLabel={t("create_server_submit")}
          confirmVariant="default"
        />
      </DialogContent>
    </Dialog>
  );
}
