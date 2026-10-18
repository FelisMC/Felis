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
import { uploadFile } from "@/components/files/uploadFile";
import { GiBInput, gibQuantity, parseGiB } from "@/components/GiBInput";
import { api, humanizeError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import { hostFor, type RuntimeConfig } from "@/lib/config";
import { DISPLAY_NAME_MAX, serverNameIssue, type ServerNameIssue } from "@/lib/naming";
import type { AutostartPolicy, ServerQuota } from "@/lib/types";
import { InlineError } from "@/components/MessageLine";

// Expose structured fields only; the backend validates resource limits and image eligibility.


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
    memory: "4",
    storage: "10",
    autostartPolicy: "ownerOnly",
  });
  const [software, setSoftware] = useState<SoftwareChoice>(EMPTY_SOFTWARE);
  const [eula, setEula] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // Retry a failed custom-JAR upload against the created server instead of creating another.
  const [jarFile, setJarFile] = useState<File | null>(null);
  const [created, setCreated] = useState<string | null>(null);
  const [uploaded, setUploaded] = useState<number | null>(null);

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

  // reset empties the form for the next server.
  function reset() {
    setForm((f) => ({ ...f, name: "", subdomain: "", displayName: "" }));
    setSoftware(EMPTY_SOFTWARE);
    setEula(false);
    setJarFile(null);
    setCreated(null);
  }

  function openChange(next: boolean) {
    // An upload in flight finishes before the dialog goes.
    if (!next && submitting) return;
    setOpen(next);
    if (next) setError(null);
    // Closed with the jar still to upload: the server stays, the form starts over.
    else if (created) reset();
  }

  const useSoftware = !staff || source === "software";
  const enabledImages = (images.data ?? []).filter((i) => i.enabled);
  // An image chosen before it was disabled or removed stops counting as chosen.
  const image = enabledImages.some((i) => i.image_ref === form.image) ? form.image : "";
  const nameIssue = serverNameIssue(form.name);
  const subdomainIssue = serverNameIssue(form.subdomain);

  // What the caller's quota leaves for memory and storage; an option past it is
  // offered disabled, with the reason beside it.
  const limits = self?.quota.limits ?? null;
  const usage = self?.quota.usage;
  const memoryLeft = limits?.max_memory_mb != null && usage ? limits.max_memory_mb - usage.memory_mb : Infinity;
  const storageLeft = limits?.max_storage_gb != null && usage ? limits.max_storage_gb * 1024 - usage.storage_mb : Infinity;
  // Memory and storage are typed in GiB: memory from 1, in steps as fine as the
  // user likes; storage in whole GiB, as a volume is sized.
  const memoryGiB = parseGiB(form.memory);
  const storageGiB = parseGiB(form.storage);
  const memoryIssue =
    memoryGiB === null || memoryGiB < 1 ? "invalid" : memoryGiB * 1024 > memoryLeft ? "quota" : null;
  const storageIssue =
    storageGiB === null || storageGiB < 1 || !Number.isInteger(storageGiB) ? "invalid" : storageGiB * 1024 > storageLeft ? "quota" : null;
  const left = (mb: number) => String(Math.max(0, Math.floor((mb / 1024) * 10) / 10));

  const uploadJar = useSoftware && software.type === "custom" && !!jarFile;
  const canSubmit = created ? !submitting : nameIssue === null &&
    subdomainIssue === null &&
    (useSoftware ? softwareReady(software) && eula : !!image) &&
    (!staff || !cfg.distributed || workers.some((n) => n.name === form.nodeName)) &&
    memoryIssue === null &&
    storageIssue === null &&
    !submitting;

  async function submit() {
    setSubmitting(true);
    setError(null);
    let name = created;
    try {
      if (!name) {
        await createServer();
        name = form.name;
        onCreated();
      }
      if (uploadJar && jarFile) {
        setCreated(name);
        setUploaded(0);
        await uploadFile(name, software.jar.trim(), jarFile, { onProgress: setUploaded });
      }
      setOpen(false);
      reset();
    } catch (e) {
      setError(name ? t("create_server_upload_failed", { error: humanizeError(e) }) : humanizeError(e));
    } finally {
      setSubmitting(false);
      setUploaded(null);
    }
  }

  // createServer sends the form to the API: the caller's own server, or staff's.
  async function createServer() {
    const displayName = form.displayName.trim() || undefined;
    if (self) {
      await api.createMyServer({
        name: form.name,
        subdomain: form.subdomain,
        displayName,
        software: softwareRequest(software),
        memory: gibQuantity(memoryGiB!),
        storage: gibQuantity(storageGiB!),
        autostartPolicy: form.autostartPolicy,
        acceptEula: eula,
      });
    } else {
      await api.createServer({
        name: form.name,
        subdomain: form.subdomain,
        displayName,
        ...(cfg.distributed ? { nodeName: form.nodeName } : {}),
        memory: gibQuantity(memoryGiB!),
        storage: gibQuantity(storageGiB!),
        autostartPolicy: form.autostartPolicy,
        ...(useSoftware ? { software: softwareRequest(software), acceptEula: eula } : { image }),
      });
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

        {/* Once the server exists only its jar upload is left, so the form is held. */}
        <fieldset disabled={!!created} className="grid min-w-0 gap-4">
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
            <SoftwarePicker
              idPrefix="cs-sw"
              value={software}
              onChange={changeSoftware}
              disabled={!!created}
              jarFile={jarFile}
              onJarFile={(file) => { setJarFile(file); setError(null); }}
            />
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

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid content-start gap-2">
              <Label htmlFor="create-server-memory">{t("create_server_memory")}</Label>
              <GiBInput
                id="create-server-memory"
                value={form.memory}
                onChange={(v) => set("memory", v)}
                invalid={!!form.memory && memoryIssue !== null}
                describedBy="create-server-memory-hint"
              />
              <p id="create-server-memory-hint" className={memoryIssue ? "text-xs text-destructive" : "text-xs text-muted-foreground"}>
                {memoryIssue === "invalid"
                  ? t("create_server_memory_invalid")
                  : memoryIssue === "quota"
                    ? t("create_server_over_quota", { left: left(memoryLeft) })
                    : memoryLeft !== Infinity
                      ? t("create_server_memory_hint_left", { left: left(memoryLeft) })
                      : t("create_server_memory_hint")}
              </p>
            </div>
            <div className="grid content-start gap-2">
              <Label htmlFor="create-server-storage">{t("create_server_storage")}</Label>
              <GiBInput
                id="create-server-storage"
                value={form.storage}
                onChange={(v) => set("storage", v)}
                invalid={!!form.storage && storageIssue !== null}
                describedBy="create-server-storage-hint"
              />
              <p id="create-server-storage-hint" className={storageIssue ? "text-xs text-destructive" : "text-xs text-muted-foreground"}>
                {storageIssue === "invalid"
                  ? t("create_server_storage_invalid")
                  : storageIssue === "quota"
                    ? t("create_server_over_quota", { left: left(storageLeft) })
                    : storageLeft !== Infinity
                      ? t("create_server_storage_hint_left", { left: left(storageLeft) })
                      : t("create_server_storage_hint")}
              </p>
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

          {useSoftware && <EulaCheckbox checked={eula} onChange={(v) => { setEula(v); setError(null); }} />}

          {uploaded !== null && jarFile && (
            <p role="status" className="text-xs text-muted-foreground">
              {t("create_server_uploading", { sent: formatBytes(uploaded), total: formatBytes(jarFile.size) })}
            </p>
          )}
          <InlineError message={error} />
        </fieldset>

        <ConfirmFooter
          onCancel={() => openChange(false)}
          onConfirm={submit}
          disabled={!canSubmit}
          loading={submitting}
          cancelLabel={t(created ? "create_server_close" : "create_server_cancel")}
          confirmLabel={t(created ? "create_server_retry_upload" : "create_server_submit")}
          confirmVariant="default"
        />
      </DialogContent>
    </Dialog>
  );
}
