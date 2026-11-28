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
import { GiBInput, gibQuantity, isWholeGiB, parseGiB } from "@/components/GiBInput";
import { api, humanizeError } from "@/lib/api";
import { useFeatures } from "@/lib/features";
import { formatBytes } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import { hostFor, type RuntimeConfig } from "@/lib/config";
import { DISPLAY_NAME_MAX, deriveServerSlug, serverNameIssue, type ServerNameIssue } from "@/lib/naming";
import { useTier } from "@/lib/tier";
import type { AutostartPolicy, ServerQuota } from "@/lib/types";
import { cn } from "@/lib/utils";
import { InlineError } from "@/components/MessageLine";
import { UserPicker, type AssignUser } from "@/components/AssignServerDialog";

const MEMORY_DEFAULT_GIB = 4;
const MEMORY_STEP_GIB = 0.5;
const MIN_GAME_HOST_BYTES = 3584 * 2 ** 20;

type OwnerChoice = "me" | "other" | "none";

function policyOptions(t: (key: string) => string, teamMode: boolean): { value: AutostartPolicy; label: string }[] {
  return [
    { value: "ownerOnly", label: t(teamMode ? "create_server_policy_manual" : "create_server_policy_owner") },
    { value: "public", label: t("create_server_policy_public") },
    { value: "allowlist", label: t("create_server_policy_allowlist") },
  ];
}

function defaultPolicy(self: boolean, teamMode: boolean): AutostartPolicy {
  if (self) return "ownerOnly";
  return teamMode ? "public" : "allowlist";
}

interface Props {
  cfg: RuntimeConfig;
  onCreated: () => void;
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
  const { features } = useFeatures();
  const teamMode = features.mode === "team";
  const staff = !self;
  const canUseImages = staff && features.customImages;
  const showOwner = staff && !teamMode;

  const [open, setOpen] = useState(false);
  const [source, setSource] = useState<"software" | "image">("software");
  const images = useAsync(() => (open && canUseImages ? api.listImages() : Promise.resolve(null)), [open, canUseImages]);
  const nodes = useAsync(() => (open && staff && cfg.distributed ? api.nodes() : Promise.resolve(null)), [open, staff, cfg.distributed]);
  const workers = (nodes.data ?? []).filter((n) => n.role === "worker" && n.ready && n.approved);
  const fleetUsage = useAsync(() => (open && staff ? api.fleetUsage({ freeMemory: true }) : Promise.resolve(null)), [open, staff]);
  const { identity, isOwner } = useTier();

  const [form, setForm] = useState<Form>({
    name: "",
    subdomain: "",
    displayName: "",
    nodeName: "",
    image: "",
    memory: String(MEMORY_DEFAULT_GIB),
    storage: "10",
    autostartPolicy: defaultPolicy(!staff, teamMode),
  });
  const [slugTouched, setSlugTouched] = useState(false);
  const [subTouched, setSubTouched] = useState(false);
  const [showSlugFields, setShowSlugFields] = useState(false);
  const [policyTouched, setPolicyTouched] = useState(false);
  const [memoryTouched, setMemoryTouched] = useState(false);
  const [owner, setOwner] = useState<OwnerChoice>("me");
  const [ownerUser, setOwnerUser] = useState<AssignUser | null>(null);
  const [software, setSoftware] = useState<SoftwareChoice>(EMPTY_SOFTWARE);
  const [eula, setEula] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [jarFile, setJarFile] = useState<File | null>(null);
  const [created, setCreated] = useState<string | null>(null);
  const [uploaded, setUploaded] = useState<number | null>(null);

  function set<K extends keyof Form>(k: K, v: Form[K]) {
    setForm((f) => ({ ...f, [k]: v }));
    setError(null);
  }

  function changeTitle(value: string) {
    const derived = deriveServerSlug(value);
    if (value.trim() !== "" && !slugTouched && (derived === "" || serverNameIssue(derived) === "reserved")) {
      setShowSlugFields(true);
    }
    setForm((f) => ({
      ...f,
      displayName: value,
      name: slugTouched ? f.name : derived,
      subdomain: subTouched ? f.subdomain : derived,
    }));
    setError(null);
  }

  function changeSoftware(next: SoftwareChoice) {
    setSoftware(next);
    setError(null);
  }

  function reset() {
    setForm((f) => ({
      ...f,
      name: "",
      subdomain: "",
      displayName: "",
      autostartPolicy: defaultPolicy(!staff, teamMode),
    }));
    setSlugTouched(false);
    setSubTouched(false);
    setShowSlugFields(false);
    setPolicyTouched(false);
    setSoftware(EMPTY_SOFTWARE);
    setEula(false);
    setJarFile(null);
    setCreated(null);
  }

  function openChange(next: boolean) {
    if (!next && submitting) return;
    setOpen(next);
    if (next) {
      setError(null);
      if (!policyTouched) {
        setForm((f) => ({ ...f, autostartPolicy: defaultPolicy(!staff, teamMode) }));
      }
      if (!canUseImages && source !== "software") setSource("software");
    } else if (created) {
      reset();
    }
  }

  const useSoftware = !canUseImages || source === "software";
  const enabledImages = (images.data ?? []).filter((i) => i.enabled);
  const image = enabledImages.some((i) => i.image_ref === form.image) ? form.image : "";
  const nameIssue = serverNameIssue(form.name);
  const subdomainIssue = serverNameIssue(form.subdomain);
  const slugEditorOpen = showSlugFields;

  const limits = self?.quota.limits ?? null;
  const quotaUsage = self?.quota.usage;
  const memoryLeft = limits?.max_memory_mb != null && quotaUsage ? limits.max_memory_mb - quotaUsage.memory_mb : Infinity;
  const storageLeft = limits?.max_storage_gb != null && quotaUsage ? limits.max_storage_gb * 1024 - quotaUsage.storage_mb : Infinity;
  const free = (fleetUsage.data?.freeMemory ?? []).filter((n) =>
    !cfg.distributed || (form.nodeName ? n.node === form.nodeName : workers.some((w) => w.name === n.node)));
  const freeBytes = free.length === 0 ? null : Math.max(...free.map((n) => n.bytes));
  const freeMB = freeBytes === null ? Infinity : freeBytes / 2 ** 20;
  const roomGiB = Math.min(freeMB, memoryLeft) / 1024;
  const defaultMemory = Math.max(1, Math.min(MEMORY_DEFAULT_GIB, Math.floor(roomGiB / MEMORY_STEP_GIB) * MEMORY_STEP_GIB));
  const memory = memoryTouched ? form.memory : String(defaultMemory);

  const memoryGiB = parseGiB(memory);
  const storageGiB = parseGiB(form.storage);
  const memoryIssue =
    memoryGiB === null || memoryGiB < 1 ? "invalid" : memoryGiB * 1024 > memoryLeft ? "quota" : null;
  const storageIssue =
    storageGiB === null || storageGiB < 1 || !isWholeGiB(storageGiB) ? "invalid" : storageGiB * 1024 > storageLeft ? "quota" : null;
  const left = (mb: number) => String(Math.max(0, Math.floor((mb / 1024) * 10) / 10));
  const overFree = memoryIssue === null && memoryGiB !== null && memoryGiB * 1024 > freeMB;
  const hosts = fleetUsage.data?.freeMemory ?? [];
  const hostTooSmall = !cfg.distributed && hosts.length > 0 && hosts.every((n) => !!n.capacity && n.capacity < MIN_GAME_HOST_BYTES);
  const ownerId = !showOwner || owner === "none" ? undefined : owner === "me" ? identity?.user_id : ownerUser?.id;

  const uploadJar = useSoftware && software.type === "custom" && !!jarFile;
  const canSubmit = created ? !submitting : nameIssue === null &&
    subdomainIssue === null &&
    (useSoftware ? softwareReady(software) && eula : !!image) &&
    (!staff || !cfg.distributed || workers.some((n) => n.name === form.nodeName)) &&
    (!showOwner || owner !== "other" || !!ownerUser) &&
    memoryIssue === null &&
    storageIssue === null &&
    !hostTooSmall &&
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

  async function createServer() {
    const trimmed = form.displayName.trim();
    const displayName = trimmed && trimmed !== form.name ? trimmed : undefined;
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
        ...(ownerId ? { ownerId } : {}),
      });
    }
  }

  const policies = policyOptions(t, teamMode);
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
          <DialogDescription>{t(self ? "self_create_desc" : canUseImages ? "create_server_desc" : "create_server_desc_software_only")}</DialogDescription>
        </DialogHeader>

        <fieldset disabled={!!created} className="grid min-w-0 gap-4">
          {hostTooSmall && (
            <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/5 p-2.5 text-xs text-destructive">
              {t("errors:host_too_small")}
            </p>
          )}
          {self && limits && quotaUsage && (
            <p className="rounded-md border border-border bg-muted/40 p-2.5 text-xs text-muted-foreground">
              {t("self_create_quota", {
                servers: limits.max_servers != null ? `${quotaUsage.servers} / ${limits.max_servers}` : quotaUsage.servers,
                memory: limits.max_memory_mb != null ? `${quotaUsage.memory_mb} / ${limits.max_memory_mb} MB` : `${quotaUsage.memory_mb} MB`,
              })}
            </p>
          )}

          <div className="grid gap-2">
            <Label htmlFor="cs-title">{t("create_server_title_name")}</Label>
            <Input
              id="cs-title"
              placeholder={t("create_server_display_name_placeholder")}
              maxLength={DISPLAY_NAME_MAX}
              value={form.displayName}
              onChange={(e) => changeTitle(e.target.value)}
              onBlur={() => {
                if (form.displayName.trim() !== "" && (nameIssue !== null || subdomainIssue !== null)) {
                  setShowSlugFields(true);
                }
              }}
            />
            <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
              <span className="truncate font-mono">
                {t("create_server_address_preview", {
                  name: form.name || "name",
                  host: hostFor(form.subdomain || "name", cfg),
                })}
              </span>
              <button
                type="button"
                aria-expanded={slugEditorOpen}
                onClick={() => setShowSlugFields((v) => !v)}
                className="shrink-0 font-medium text-primary hover:underline"
              >
                {t(slugEditorOpen ? "create_server_hide_identity" : "create_server_edit_identity")}
              </button>
            </div>
          </div>

          {slugEditorOpen && (
            <div className="grid gap-4 rounded-lg border border-border bg-muted/20 p-3 sm:grid-cols-2">
              <div className="grid gap-2">
                <Label htmlFor="cs-name">{t("create_server_name")}</Label>
                <Input
                  id="cs-name"
                  placeholder={t("create_server_name_placeholder")}
                  value={form.name}
                  onChange={(e) => {
                    const v = e.target.value.toLowerCase();
                    setSlugTouched(true);
                    setForm((f) => ({ ...f, name: v, subdomain: subTouched ? f.subdomain : v }));
                    setError(null);
                  }}
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
                  onChange={(e) => {
                    setSubTouched(true);
                    set("subdomain", e.target.value.toLowerCase());
                  }}
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
          )}

          {canUseImages && (
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

          {showOwner && (
            <div className="grid gap-2">
              <Label htmlFor="cs-owner">{t("create_server_owner")}</Label>
              <Select value={owner} onValueChange={(v) => { setOwner(v as OwnerChoice); setError(null); }}>
                <SelectTrigger id="cs-owner" aria-describedby="cs-owner-hint"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="me">{identity?.email ? t("create_server_owner_me_named", { account: identity.email }) : t("create_server_owner_me")}</SelectItem>
                  {isOwner && <SelectItem value="other">{t("create_server_owner_other")}</SelectItem>}
                  <SelectItem value="none">{t("create_server_owner_none")}</SelectItem>
                </SelectContent>
              </Select>
              {owner === "other" && <UserPicker exclude={identity?.email} picked={ownerUser} onPick={(u) => { setOwnerUser(u); setError(null); }} />}
              <p id="cs-owner-hint" className="text-xs text-muted-foreground">
                {t(owner === "none" ? "create_server_owner_none_hint" : "create_server_owner_hint")}
              </p>
            </div>
          )}

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
                value={memory}
                onChange={(v) => { setMemoryTouched(true); set("memory", v); }}
                invalid={!!memory && memoryIssue !== null}
                describedBy="create-server-memory-hint"
              />
              <p
                id="create-server-memory-hint"
                className={cn("text-xs", memoryIssue ? "text-destructive" : overFree ? "text-amber-700 dark:text-amber-300" : "text-muted-foreground")}
              >
                {memoryIssue === "invalid"
                  ? t("create_server_memory_invalid")
                  : memoryIssue === "quota"
                    ? t("create_server_over_quota", { left: left(memoryLeft) })
                    : overFree
                      ? t("create_server_memory_over_free", { free: left(freeMB) })
                      : freeBytes !== null
                        ? t("create_server_memory_hint_free", { free: left(freeMB) })
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
              onValueChange={(v) => {
                setPolicyTouched(true);
                set("autostartPolicy", v as AutostartPolicy);
              }}
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
