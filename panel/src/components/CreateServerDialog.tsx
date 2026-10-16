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
import { SegmentedControl } from "@/components/SegmentedControl";
import {
  EMPTY_SOFTWARE,
  EulaCheckbox,
  SoftwarePicker,
  softwareReady,
  softwareRequest,
  type SoftwareChoice,
} from "@/components/SoftwarePicker";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { hostFor, type RuntimeConfig } from "@/lib/config";
import { DISPLAY_NAME_MAX, serverNameIssue, type ServerNameIssue } from "@/lib/naming";
import type { AutostartPolicy, ServerQuota } from "@/lib/types";
import { InlineError } from "@/components/MessageLine";

// CreateServerDialog IS the §15 收口 ("structured form, not free YAML"): every
// dangerous knob (hostNetwork, hostPath, arbitrary image, raw resource quantities)
// is simply absent from the UI. A server runs either server software the
// platform installs on the runtime image, or (staff only) an image from the
// server-side whitelist (GET /images); memory/storage are constrained choices
// the API re-validates. Nothing here can express a privileged pod.

const MEMORY_OPTIONS = ["2Gi", "4Gi", "6Gi", "8Gi"];
const STORAGE_OPTIONS = ["5Gi", "10Gi", "20Gi", "50Gi"];

function policyOptions(t: (key: string) => string): { value: AutostartPolicy; label: string }[] {
  return [
    { value: "ownerOnly", label: t("create_server_policy_owner") },
    { value: "public", label: t("create_server_policy_public") },
    { value: "allowlist", label: t("create_server_policy_allowlist") },
  ];
}

/** gibToMB converts the "<n>Gi" options to the quota's MB. */
function gibToMB(q: string): number {
  return Number(q.replace(/Gi$/, "")) * 1024;
}

interface Props {
  cfg: RuntimeConfig;
  onCreated: () => void;
  /** self creates a server of the caller's own within their quota (POST
   *  /me/servers); staff otherwise (POST /servers). */
  self?: { quota: ServerQuota };
}

interface Form {
  name: string;
  subdomain: string;
  displayName: string;
  nodeName: string;
  image: string;
  memory: string;
  storage: string;
  autostartPolicy: AutostartPolicy;
}

export function CreateServerDialog({ cfg, onCreated, self }: Props) {
  const { t } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  const staff = !self;
  const [source, setSource] = useState<"software" | "image">("software");
  // The whitelist is read each time the dialog opens, so an image added, enabled
  // or retired since the page loaded shows up or drops out without a reload.
  const images = useAsync(() => (open && staff ? api.listImages() : Promise.resolve(null)), [open, staff]);

  const nodes = useAsync(() => (open && staff && cfg.distributed ? api.nodes() : Promise.resolve(null)), [open, staff, cfg.distributed]);
  const workers = (nodes.data ?? []).filter((n) => n.role === "worker" && n.ready && n.approved);

  const [form, setForm] = useState<Form>({
    name: "",
    subdomain: "",
    displayName: "",
    nodeName: "",
    image: "",
    memory: "4Gi",
    storage: "10Gi",
    autostartPolicy: "ownerOnly",
  });
  const [software, setSoftware] = useState<SoftwareChoice>(EMPTY_SOFTWARE);
  const [eula, setEula] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // A refusal answers the form as it was sent; any edit, or opening the dialog
  // again, starts past it.
  function set<K extends keyof Form>(k: K, v: Form[K]) {
    setForm((f) => ({ ...f, [k]: v }));
    setError(null);
  }
  function changeSoftware(next: SoftwareChoice) {
    setSoftware(next);
    setError(null);
  }

  function openChange(next: boolean) {
    setOpen(next);
    if (next) setError(null);
  }

  const useSoftware = !staff || source === "software";
  const enabledImages = (images.data ?? []).filter((i) => i.enabled);
  // An image chosen before it was disabled or removed no longer counts as chosen.
  const image = enabledImages.some((i) => i.image_ref === form.image) ? form.image : "";
  const nameIssue = serverNameIssue(form.name);
  const subdomainIssue = serverNameIssue(form.subdomain);

  // What the caller's quota leaves for memory and storage; an option past it is
  // offered disabled, with the reason beside it.
  const limits = self?.quota.limits ?? null;
  const usage = self?.quota.usage;
  const memoryLeft = limits?.max_memory_mb != null && usage ? limits.max_memory_mb - usage.memory_mb : Infinity;
  const storageLeft = limits?.max_storage_gb != null && usage ? limits.max_storage_gb * 1024 - usage.storage_mb : Infinity;
  const memoryFits = (m: string) => gibToMB(m) <= memoryLeft;
  const storageFits = (s: string) => gibToMB(s) <= storageLeft;

  const canSubmit =
    nameIssue === null &&
    subdomainIssue === null &&
    (useSoftware ? softwareReady(software) && eula : !!image) &&
    (!staff || !cfg.distributed || workers.some((n) => n.name === form.nodeName)) &&
    !!form.memory &&
    !!form.storage &&
    (staff || (memoryFits(form.memory) && storageFits(form.storage))) &&
    !submitting;

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const displayName = form.displayName.trim() || undefined;
      if (self) {
        await api.createMyServer({
          name: form.name,
          subdomain: form.subdomain,
          displayName,
          software: softwareRequest(software),
          memory: form.memory,
          storage: form.storage,
          autostartPolicy: form.autostartPolicy,
          acceptEula: eula,
        });
      } else {
        await api.createServer({
          name: form.name,
          subdomain: form.subdomain,
          displayName,
          ...(cfg.distributed ? { nodeName: form.nodeName } : {}),
          memory: form.memory,
          storage: form.storage,
          autostartPolicy: form.autostartPolicy,
          ...(useSoftware ? { software: softwareRequest(software), acceptEula: eula } : { image }),
        });
      }
      setOpen(false);
      onCreated();
      setForm((f) => ({ ...f, name: "", subdomain: "", displayName: "" }));
      setSoftware(EMPTY_SOFTWARE);
      setEula(false);
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
  const quotaFull = !!self && !self.quota.canCreate;

  return (
    <Dialog open={open} onOpenChange={openChange}>
      <DialogTrigger asChild>
        <Button size="sm" className="gap-1.5" disabled={quotaFull} title={quotaFull ? t("self_create_quota_full") : undefined}>
          <Plus className="h-4 w-4" />
          {t("create_server_btn")}
        </Button>
      </DialogTrigger>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t(self ? "self_create_title" : "create_server_title")}</DialogTitle>
          <DialogDescription>{t(self ? "self_create_desc" : "create_server_desc")}</DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          {self && limits && usage && (
            <p className="rounded-md border border-border bg-muted/40 p-2.5 text-xs text-muted-foreground">
              {t("self_create_quota", {
                servers: limits.max_servers != null ? `${usage.servers} / ${limits.max_servers}` : usage.servers,
                memory: limits.max_memory_mb != null ? `${usage.memory_mb} / ${limits.max_memory_mb} MB` : `${usage.memory_mb} MB`,
              })}
            </p>
          )}

          {staff && (
            <SegmentedControl
              label={t("create_server_source")}
              value={source}
              onChange={(k) => { setSource(k); setError(null); }}
              segments={[
                { key: "software", label: t("software_source_software") },
                { key: "image", label: t("software_source_image") },
              ]}
            />
          )}

          {useSoftware ? (
            <SoftwarePicker idPrefix="cs-sw" value={software} onChange={changeSoftware} />
          ) : (
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
          )}

          <div className="grid gap-4 sm:grid-cols-2">
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
                {form.name && nameIssue ? issueText(nameIssue, form.name) : t("create_server_name_hint")}
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
          </div>

          <div className="grid gap-2">
            <Label htmlFor="cs-display">{t("create_server_display_name")}</Label>
            <Input
              id="cs-display"
              placeholder={t("create_server_display_name_placeholder")}
              maxLength={DISPLAY_NAME_MAX}
              value={form.displayName}
              onChange={(e) => set("displayName", e.target.value)}
            />
          </div>

          {staff && cfg.distributed && (
            <div className="grid gap-2">
              <Label htmlFor="cs-node">{t("node")}</Label>
              <Select value={form.nodeName} onValueChange={(v) => set("nodeName", v)}>
                <SelectTrigger id="cs-node"><SelectValue placeholder={t("node_choose")} /></SelectTrigger>
                <SelectContent>{workers.map((n) => <SelectItem key={n.name} value={n.name}>{n.name}</SelectItem>)}</SelectContent>
              </Select>
              {!!nodes.error && <InlineError message={humanizeError(nodes.error)} />}
              {!nodes.loading && !nodes.error && workers.length === 0 && <p className="text-xs text-muted-foreground">{t("node_none")}</p>}
            </div>
          )}

          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="create-server-memory">{t("create_server_memory")}</Label>
              <Select value={form.memory} onValueChange={(v) => set("memory", v)}>
                <SelectTrigger id="create-server-memory">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MEMORY_OPTIONS.map((m) => (
                    <SelectItem key={m} value={m} disabled={!staff && !memoryFits(m)}>
                      {!staff && !memoryFits(m) ? t("self_create_over_quota", { value: m }) : m}
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
                    <SelectItem key={s} value={s} disabled={!staff && !storageFits(s)}>
                      {!staff && !storageFits(s) ? t("self_create_over_quota", { value: s }) : s}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          {!staff && (!memoryFits(form.memory) || !storageFits(form.storage)) && (
            <p className="text-xs text-destructive">{t("self_create_over_quota_hint")}</p>
          )}

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

          {useSoftware && <EulaCheckbox checked={eula} onChange={(v) => { setEula(v); setError(null); }} />}

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
