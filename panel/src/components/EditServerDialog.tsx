import { Checkbox } from "@/components/ui/checkbox";
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
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError } from "@/lib/api";
import { formatBytes, splitImageRef } from "@/lib/format";
import { DISPLAY_NAME_MAX } from "@/lib/naming";
import { useAsync } from "@/lib/hooks";
import type { AutostartPolicy, ServerSoftware } from "@/lib/types";
import { InlineError } from "@/components/MessageLine";
import { uploadFile } from "@/components/files/uploadFile";
import { SegmentedControl } from "@/components/SegmentedControl";
import { GiBInput, gibQuantity, parseGiB, quantityGiB } from "@/components/GiBInput";
import {
  EMPTY_SOFTWARE,
  EulaCheckbox,
  SoftwarePicker,
  softwareReady,
  softwareRequest,
  type SoftwareChoice,
} from "@/components/SoftwarePicker";

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
  systemService?: boolean;
  currentDisplayName?: string;
  currentPolicy?: AutostartPolicy;
  currentImage?: string;
  currentMemory?: string;
  currentStorage?: string;
  currentCpu?: string;
  currentIdleStopSeconds?: number;
  /** The operator cannot read the player count, so idle stop is paused. */
  playerCountUnknown?: boolean;
  /** What the server runs from the runtime image; absent for a server image. */
  currentSoftware?: ServerSoftware;
  /** owner edits a server the caller owns (PATCH /me/servers/{name}): no image,
   *  memory and CPU within their quota. admin is the staff patch. */
  mode?: "admin" | "owner";
  onUpdated: () => void;
}

/** choiceOf turns a server's software into the picker's value. */
function choiceOf(s?: ServerSoftware): SoftwareChoice {
  if (!s) return EMPTY_SOFTWARE;
  return { type: s.type, minecraftVersion: s.minecraftVersion ?? "", build: s.build ?? "", java: s.java, jar: s.jar ?? "" };
}

/** softwareSummary names a server's software the way the panel lists it. */
export function softwareSummary(t: (key: string, opts?: Record<string, unknown>) => string, s: ServerSoftware): string {
  const name = t(`software_type_${s.type}`);
  if (s.type === "custom") return `${name} · ${s.jar ?? ""}`;
  return [name, s.minecraftVersion, s.build].filter(Boolean).join(" ");
}

export function EditServerDialog({
  serverName,
  systemService = false,
  currentDisplayName = "",
  currentPolicy = "ownerOnly",
  currentImage = "",
  currentMemory = "",
  currentStorage = "",
  currentCpu = "",
  currentIdleStopSeconds = 0,
  playerCountUnknown = false,
  currentSoftware,
  mode = "admin",
  onUpdated,
}: Props) {
  const currentIdleStop = String(currentIdleStopSeconds);
  const { t } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  const admin = mode === "admin";
  const images = useAsync(() => (admin ? api.listImages() : Promise.resolve([])), [admin]);
  // What runs the server: server software on the runtime image, or a whitelisted
  // server image (staff only). Changing software is opened on purpose, so a
  // plain settings edit never sends one.
  const [source, setSource] = useState<"software" | "image">(currentSoftware ? "software" : "image");
  const [changingSoftware, setChangingSoftware] = useState(false);
  const [software, setSoftware] = useState<SoftwareChoice>(choiceOf(currentSoftware));
  const [softwareConfirmed, setSoftwareConfirmed] = useState(false);
  const [eula, setEula] = useState(false);
  // A custom jar chosen here is uploaded into the server's files on save,
  // replacing one of the same name.
  const [jarFile, setJarFile] = useState<File | null>(null);
  const [uploaded, setUploaded] = useState<number | null>(null);

  // Memory is edited in GiB, as typed text.
  const currentMemoryGiB = quantityGiB(currentMemory);
  const [form, setForm] = useState<EditServerForm>({
    displayName: currentDisplayName,
    autostartPolicy: currentPolicy,
    image: currentImage,
    memory: currentMemoryGiB,
    cpu: currentCpu,
    idleStop: currentIdleStop,
  });

  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // An image change moves the world to another build for good, so saving one waits
  // for this acknowledgement (the API refuses it with image_change_unconfirmed).
  const [imageConfirmed, setImageConfirmed] = useState(false);

  // The console polls the status and hands a new software object each time, so
  // the form follows what the server runs by value: a poll alone never clears
  // a choice being made.
  const softwareKey = currentSoftware ? JSON.stringify(choiceOf(currentSoftware)) : "";

  // Sync form state when dialog opens or current values change from server status
  useEffect(() => {
    if (open) {
      setForm({
        displayName: currentDisplayName,
        autostartPolicy: currentPolicy,
        image: currentImage,
        memory: currentMemoryGiB,
        cpu: currentCpu,
        idleStop: currentIdleStop,
      });
      setError(null);
      setImageConfirmed(false);
      setSource(softwareKey ? "software" : "image");
      setChangingSoftware(false);
      setSoftware(softwareKey ? (JSON.parse(softwareKey) as SoftwareChoice) : EMPTY_SOFTWARE);
      setSoftwareConfirmed(false);
      setEula(false);
      setJarFile(null);
    }
  }, [open, currentDisplayName, currentPolicy, currentImage, currentMemoryGiB, currentCpu, currentIdleStop, softwareKey]);

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

  // Software changes may irreversibly upgrade worlds and require acknowledgement; leaving an image also requires EULA acceptance.
  const pickingSoftware = source === "software" && (changingSoftware || !currentSoftware);
  const jarUpload = pickingSoftware && software.type === "custom" ? jarFile : null;
  const cur = choiceOf(currentSoftware);
  const softwareChanged =
    pickingSoftware &&
    softwareReady(software) &&
    (software.type !== cur.type || software.minecraftVersion !== cur.minecraftVersion || software.build !== cur.build ||
      software.java !== cur.java || software.jar.trim() !== cur.jar);
  const softwareMoves =
    softwareChanged && (!currentSoftware || software.type !== cur.type || software.minecraftVersion !== cur.minecraftVersion);
  const imageChanged = admin && source === "image" && form.image !== currentImage;

  const memoryGiB = parseGiB(form.memory);
  const memoryOk = memoryGiB !== null && memoryGiB >= 1;
  const memoryChanged = memoryGiB === null ? form.memory.trim() !== currentMemoryGiB : memoryGiB !== parseGiB(currentMemoryGiB);

  const hasChanges =
    displayName !== currentDisplayName ||
    form.autostartPolicy !== currentPolicy ||
    imageChanged ||
    softwareChanged ||
    !!jarUpload ||
    memoryChanged ||
    cpu !== currentCpu ||
    form.idleStop !== currentIdleStop;

  const pinnedBuild = splitImageRef(currentImage).short;
  const canSubmit =
    hasChanges &&
    cpuOk &&
    (!memoryChanged || memoryOk) &&
    !submitting &&
    (!imageChanged || imageConfirmed) &&
    (!softwareMoves || softwareConfirmed) &&
    (!softwareChanged || !!currentSoftware || eula);

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const payload: Parameters<typeof api.patchServer>[1] = {};

      if (displayName !== currentDisplayName) {
        payload.displayName = displayName;
      }
      if (!systemService && form.autostartPolicy !== currentPolicy) {
        payload.autostartPolicy = form.autostartPolicy;
      }
      if (imageChanged) {
        payload.image = form.image;
        payload.confirmImageChange = imageConfirmed;
      }
      if (softwareChanged) {
        payload.software = softwareRequest(software);
        if (softwareMoves) payload.confirmSoftwareChange = true;
        if (!currentSoftware) payload.acceptEula = eula;
      }
      // Memory and CPU each go alone; the API keeps whatever is not sent.
      if (memoryChanged && memoryGiB !== null) {
        payload.memory = gibQuantity(memoryGiB);
      }
      if (cpu !== currentCpu) {
        payload.resources = { cpu };
      }
      if (!systemService && form.idleStop !== currentIdleStop) {
        payload.idleStopSeconds = Number(form.idleStop);
      }

      // The jar goes first: the software then names a file that is there.
      if (jarUpload) {
        setUploaded(0);
        await uploadFile(serverName, software.jar.trim(), jarUpload, { overwrite: true, onProgress: setUploaded });
      }
      if (Object.keys(payload).length > 0) {
        if (admin) {
          await api.patchServer(serverName, payload);
        } else {
          const { resources, ...rest } = payload;
          await api.patchMyServer(serverName, { ...rest, ...(resources ? { cpu: resources.cpu } : {}) });
        }
      }
      setOpen(false);
      onUpdated();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setSubmitting(false);
      setUploaded(null);
    }
  }

  const policies = policyOptions(t);

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {/* Unified Sidebar Link/Card style trigger */}
        <button
          type="button"
          className="group flex w-full text-left items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent focus:outline-none"
        >
          <Settings className="h-5 w-5 shrink-0 text-primary" />
          <div className="min-w-0 flex-1 text-left">
            <p className="text-sm font-medium">{t("edit_server_title")}</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              {t(systemService ? "edit_system_desc" : admin ? "edit_server_desc" : "edit_owner_desc")}
            </p>
          </div>
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
        </button>
      </DialogTrigger>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("edit_server_title")}</DialogTitle>
          <DialogDescription>
            {t(systemService ? "edit_system_desc" : "edit_server_desc_long")}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="es-display">{t("create_server_display_name")}</Label>
            <Input
              id="es-display"
              placeholder={t("create_server_display_name_placeholder")}
              maxLength={DISPLAY_NAME_MAX}
              value={form.displayName}
              onChange={(e) => set("displayName", e.target.value)}
            />
          </div>

          {admin && !systemService && (
            <SegmentedControl
              label={t("create_server_source")}
              value={source}
              onChange={(k) => { setSource(k); setChangingSoftware(false); setSoftware(choiceOf(currentSoftware)); }}
              segments={[
                { key: "software", label: t("software_source_software") },
                { key: "image", label: t("software_source_image") },
              ]}
            />
          )}

          {source === "software" || !admin ? (
            <div className="grid gap-2">
              <Label>{t("edit_server_software")}</Label>
              {!pickingSoftware ? (
                <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border p-2.5 text-sm">
                  <span>
                    {currentSoftware
                      ? t("edit_server_software_current", { summary: softwareSummary(t, currentSoftware), java: currentSoftware.java })
                      : t("edit_server_software_image", { image: splitImageRef(currentImage).tag || currentImage })}
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => { setSource("software"); setChangingSoftware(true); }}
                  >
                    {t(currentSoftware ? "edit_server_software_change" : "edit_server_software_switch")}
                  </Button>
                </div>
              ) : (
                <div className="grid gap-3 rounded-md border border-border p-3">
                  <SoftwarePicker idPrefix="es-sw" value={software} onChange={setSoftware}
                    jarFile={jarFile}
                    onJarFile={(file) => { setJarFile(file); setError(null); }}
                    jarServer={serverName}
                  />
                  {softwareMoves && (
                    <div className="grid gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs">
                      <p className="flex items-start gap-1.5 text-amber-700 dark:text-amber-300">
                        <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
                        {t("edit_server_software_warning")}
                      </p>
                      <label className="flex cursor-pointer items-center gap-2 font-medium text-foreground">
                        <Checkbox className="mt-0.5" checked={softwareConfirmed} onChange={(e) => setSoftwareConfirmed(e.target.checked)} />
                        {t("edit_server_software_confirm")}
                      </label>
                    </div>
                  )}
                  {!currentSoftware && <EulaCheckbox checked={eula} onChange={setEula} />}
                  {(currentSoftware || !admin) && (
                    <Button
                      variant="ghost"
                      size="sm"
                      className="justify-self-start"
                      onClick={() => { setChangingSoftware(false); setSoftware(choiceOf(currentSoftware)); if (!currentSoftware) setSource("image"); }}
                    >
                      {t("edit_server_software_cancel")}
                    </Button>
                  )}
                </div>
              )}
            </div>
          ) : (
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
                  <Checkbox

                    className="mt-0.5"
                    checked={imageConfirmed}
                    onChange={(e) => setImageConfirmed(e.target.checked)}
                  />
                  {t("edit_server_image_confirm")}
                </label>
              </div>
            )}
          </div>
          )}

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid content-start gap-2">
              <Label htmlFor="edit-server-memory">{t("create_server_memory")}</Label>
              <GiBInput
                id="edit-server-memory"
                value={form.memory}
                onChange={(v) => set("memory", v)}
                invalid={memoryChanged && !memoryOk}
                describedBy="edit-server-memory-hint"
              />
              <p id="edit-server-memory-hint" className={memoryChanged && !memoryOk ? "text-xs text-destructive" : "text-xs text-muted-foreground"}>
                {memoryChanged && !memoryOk ? t("create_server_memory_invalid") : t("create_server_memory_hint")}
              </p>
            </div>
            <div className="grid content-start gap-2">
              {/* A volume keeps the size it was created with. */}
              <Label htmlFor="edit-server-storage">{t("create_server_storage")}</Label>
              <GiBInput id="edit-server-storage" value={quantityGiB(currentStorage) || currentStorage} onChange={() => {}} disabled describedBy="edit-server-storage-hint" />
              <p id="edit-server-storage-hint" className="text-xs text-muted-foreground">{t("edit_server_storage_fixed")}</p>
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
                disabled={systemService}
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
            <Select disabled={systemService} value={form.idleStop} onValueChange={(v) => set("idleStop", v)}>
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

          {uploaded !== null && jarUpload && (
            <p role="status" className="text-xs text-muted-foreground">
              {t("create_server_uploading", { sent: formatBytes(uploaded), total: formatBytes(jarUpload.size) })}
            </p>
          )}
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
