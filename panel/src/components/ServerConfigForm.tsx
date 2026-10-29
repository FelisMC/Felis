import { Checkbox } from "@/components/ui/checkbox";
import { createContext, useState, useEffect, useRef, useCallback, useContext, useId, type ReactNode } from "react";
import { ChevronRight, AlertTriangle, Loader2, Plus, Trash2, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { CopyAddress } from "@/components/CopyAddress";
import { Card } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { api, humanizeError } from "@/lib/api";
import { formatBytes, splitImageRef } from "@/lib/format";
import { DISPLAY_NAME_MAX } from "@/lib/naming";
import { MAX_SERVER_TAGS, MAX_SERVER_TAG_LENGTH, mergeTags, tagIssue } from "@/lib/tags";
import { useAsync, useUnsavedGuard } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { AssignServerDialog } from "@/components/AssignServerDialog";
import type { AutostartPolicy, Phase, ServerExtraPort, ServerNotify, ServerSoftware, ServerStartupSettings } from "@/lib/types";
import { InlineError } from "@/components/MessageLine";
import { cn } from "@/lib/utils";
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

/** Idle auto-stop presets in seconds once it is on; the server default is 600. */
const IDLE_OPTIONS = ["300", "600", "900", "1800", "3600", "7200"];
const DEFAULT_IDLE_STOP = "600";

/** idleLabel renders an idle-stop duration the way a person says it. */
export function idleLabel(t: (key: string, opts?: Record<string, unknown>) => string, seconds: number): string {
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

export function policyOptions(t: (key: string) => string): { value: AutostartPolicy; label: string }[] {
  return [
    { value: "ownerOnly", label: t("create_server_policy_owner") },
    { value: "public", label: t("create_server_policy_public") },
    { value: "allowlist", label: t("create_server_policy_allowlist") },
  ];
}

interface EditServerForm {
  displayName: string;
  tags: string[];
  autostartPolicy: AutostartPolicy;
  image: string;
  memory: string;
  storage: string;
  cpu: string;
  /** Idle auto-stop seconds as a Select value; "0" is Never. */
  idleStop: string;
  resume: boolean;
  crashRestart: boolean;
  maxCrash: string;
  notify: ServerNotify;
}

const MAX_CRASH_RESTARTS = 20;

function crashLimit(text: string): number | null {
  const n = Number(text.trim());
  return /^\d+$/.test(text.trim()) && n >= 1 && n <= MAX_CRASH_RESTARTS ? n : null;
}

const DEFAULT_NOTIFY: ServerNotify = { started: false, stopped: false, problems: true };
const NO_TAGS: string[] = [];
const NOTIFY_KEYS = ["problems", "started", "stopped"] as const;
const STARTUP_KEYS = ["java_flags", "args", "env", "stop_commands"] as const;
const STARTUP_LISTS = [{ key: "java_flags", max: 128 }, { key: "args", max: 128 }, { key: "stop_commands", max: 16 }] as const;
const MAX_EXTRA_PORTS = 5;
type ExtraPortDraft = { protocol: ServerExtraPort["protocol"]; port: string };

function StartupArgumentList({ label, hint, values, max, onChange }: {
  label: string; hint: string; values: string[]; max: number; onChange: (values: string[]) => void;
}) {
  const { t } = useTranslation("servers");
  const id = useId();
  return <fieldset className="min-w-0 space-y-2 rounded-md border border-border p-3">
    <legend className="px-1 text-sm font-medium">{label}</legend>
    <p id={`${id}-hint`} className="text-xs leading-relaxed text-muted-foreground">{hint}</p>
    {values.map((value, index) => <div key={index} className="flex min-w-0 items-start gap-2">
      <div className="min-w-0 flex-1 space-y-1">
        <Label htmlFor={`${id}-${index}`} className="text-xs">{t("edit_startup_item", { label, number: index + 1 })}</Label>
        <Textarea id={`${id}-${index}`} rows={2} className="min-h-16 font-mono text-base sm:text-sm" value={value}
          aria-describedby={`${id}-hint`} spellCheck={false}
          onChange={(event) => onChange(values.map((item, at) => at === index ? event.target.value : item))} />
      </div>
      <Button type="button" variant="ghost" size="icon" className="mt-5 shrink-0"
        aria-label={t("edit_startup_remove_item", { label, number: index + 1 })} onClick={() => onChange(values.filter((_, at) => at !== index))}>
        <Trash2 className="h-4 w-4" />
      </Button>
    </div>)}
    <Button type="button" variant="outline" size="sm" disabled={values.length >= max} onClick={() => onChange([...values, ""])}>
      <Plus className="h-4 w-4" />{t("edit_startup_add_item", { label })}
    </Button>
    <p className="text-xs text-muted-foreground">{t("edit_startup_count", { count: values.length, max })}</p>
  </fieldset>;
}

// InDialog stacks every row, since a dialog has no room for the label and control side by side.
const InDialog = createContext(false);

/** SettingsSection groups related parameters under a title; inside a dialog it drops the card. */
export function SettingsSection({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  const inDialog = useContext(InDialog);
  const titleId = useId();
  const header = (
    <div className={cn("space-y-1", !inDialog && "border-b border-border px-5 py-4")}>
      <h2 id={titleId} className="text-sm font-semibold">{title}</h2>
      {description && <p className="text-xs leading-relaxed text-muted-foreground">{description}</p>}
    </div>
  );
  return inDialog ? (
    <section aria-labelledby={titleId} className="grid gap-3 border-t border-border pt-4 first:border-t-0 first:pt-0">
      {header}
      <div className="grid gap-4">{children}</div>
    </section>
  ) : (
    <Card role="region" aria-labelledby={titleId}>
      {header}
      <div className="divide-y divide-border">{children}</div>
    </Card>
  );
}

/** SettingRow puts a parameter's name and explanation beside its control; block puts the control underneath. */
export function SettingRow({ label, hint, htmlFor, block = false, children }: {
  label: ReactNode; hint?: ReactNode; htmlFor?: string; block?: boolean; children: ReactNode;
}) {
  const inDialog = useContext(InDialog);
  const side = !inDialog && !block;
  return (
    <div className={cn("grid gap-2", !inDialog && "px-5 py-4", side && "lg:grid-cols-[minmax(0,1fr)_minmax(0,24rem)] lg:items-center lg:gap-8")}>
      <div className="min-w-0 space-y-1">
        {htmlFor ? <Label htmlFor={htmlFor} className="text-sm font-medium">{label}</Label> : <p className="text-sm font-medium">{label}</p>}
        {hint && <div className="text-xs leading-relaxed text-muted-foreground">{hint}</div>}
      </div>
      <div className="min-w-0">{children}</div>
    </div>
  );
}

export interface ServerConfigProps {
  serverName: string;
  systemService?: boolean;
  currentDisplayName?: string;
  currentTags?: string[];
  currentPhase?: Phase;
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
  currentResume?: boolean;
  currentCrashRestart?: boolean;
  currentMaxCrashRestarts?: number;
  currentNotify?: ServerNotify;
  currentExtraPorts?: ServerExtraPort[];
  currentExtraPortsPending?: boolean;
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

/** ServerConfigForm edits a server's parameters in place: on the settings page with a save bar, or inside a dialog with its footer. */
export function ServerConfigForm({
  serverName,
  systemService = false,
  currentDisplayName = "",
  currentTags = NO_TAGS,
  currentPhase,
  currentPolicy = "ownerOnly",
  currentImage = "",
  currentMemory = "",
  currentStorage = "",
  currentCpu = "",
  currentIdleStopSeconds = 0,
  playerCountUnknown = false,
  currentSoftware,
  currentResume = true,
  currentCrashRestart = true,
  currentMaxCrashRestarts = 3,
  currentNotify = DEFAULT_NOTIFY,
  currentExtraPorts,
  currentExtraPortsPending = false,
  mode = "admin",
  onUpdated,
  inDialog = false,
  onDone,
  network,
}: ServerConfigProps & {
  inDialog?: boolean;
  /** onDone closes the dialog after a save or a cancel. */
  onDone?: () => void;
  /** network adds rows the page manages itself to the network section. */
  network?: ReactNode;
}) {
  const currentIdleStop = String(currentIdleStopSeconds);
  const currentMaxCrash = String(currentMaxCrashRestarts);
  // The status poll hands a new object each time, so the form follows the values.
  const { started: notifyStarted, stopped: notifyStopped, problems: notifyProblems } = currentNotify;
  const { t } = useTranslation("servers");
  const admin = mode === "admin";
  const images = useAsync(() => (admin ? api.listImages() : Promise.resolve([])), [admin]);
  const { isOwner } = useTier();
  const showOwner = isOwner && !systemService;
  const [assignOpen, setAssignOpen] = useState(false);
  // The fleet is the only read that names a server's owner.
  const fleet = useAsync(() => (showOwner ? api.fleet() : Promise.resolve(null)), [showOwner]);
  const ownerRow = fleet.data?.find((s) => s.name === serverName);
  const ownerName = ownerRow?.owner ?? "";
  let ownerLabel = ownerName;
  if (!ownerLabel) {
    if (fleet.loading && !fleet.data) ownerLabel = "—";
    else ownerLabel = t(ownerRow && !ownerRow.ownerUnknown ? "ops:fleet_unclaimed" : "ops:fleet_owner_unknown");
  }
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

  // Memory and storage are edited in GiB, as typed text.
  const currentMemoryGiB = quantityGiB(currentMemory);
  const currentStorageGiB = quantityGiB(currentStorage);
  // The status poll hands a new array each time, so the form follows the tags by value.
  const tagsKey = JSON.stringify(currentTags);
  const [tagDraft, setTagDraft] = useState("");
  const [form, setForm] = useState<EditServerForm>({
    displayName: currentDisplayName,
    tags: currentTags,
    autostartPolicy: currentPolicy,
    image: currentImage,
    memory: currentMemoryGiB,
    storage: currentStorageGiB,
    cpu: currentCpu,
    idleStop: currentIdleStop,
    resume: currentResume,
    crashRestart: currentCrashRestart,
    maxCrash: currentMaxCrash,
    notify: { started: notifyStarted, stopped: notifyStopped, problems: notifyProblems },
  });

  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // An image change moves the world to another build for good, so saving one waits
  // for this acknowledgement (the API refuses it with image_change_unconfirmed).
  const [imageConfirmed, setImageConfirmed] = useState(false);
  const portsId = useId();
  const portsKey = JSON.stringify((currentExtraPorts ?? []).map(({ protocol, port }) => ({ protocol, port })));
  const [ports, setPorts] = useState<{ name: string; original: string; draft: ExtraPortDraft[] } | null>(null);
  const startupId = useId();
  const [startupOpen, setStartupOpen] = useState(false);
  const [startup, setStartup] = useState<{ name: string; original: ServerStartupSettings; draft: ServerStartupSettings } | null>(null);
  const [startupLoading, setStartupLoading] = useState(false);
  const [startupError, setStartupError] = useState<string | null>(null);
  const startupGeneration = useRef(0);
  const startupBusy = useRef(false);
  const saveGeneration = useRef(0);
  const saving = useRef(false);
  useEffect(() => {
    startupGeneration.current += 1;
    saveGeneration.current += 1;
    startupBusy.current = false;
    saving.current = false;
    setStartupOpen(false);
    setStartup(null);
    setStartupError(null);
    setStartupLoading(false);
    setSubmitting(false);
    setUploaded(null);
    setPorts(null);
    return () => { startupGeneration.current += 1; saveGeneration.current += 1; };
  }, [serverName]);

  useEffect(() => {
    setPorts((previous) => previous?.name === serverName ? previous : {
      name: serverName, original: portsKey,
      draft: (JSON.parse(portsKey) as ServerExtraPort[]).map(({ protocol, port }) => ({ protocol, port: String(port) })),
    });
  }, [serverName, portsKey]);
  const currentPorts = ports?.name === serverName ? ports : null;
  const extraPorts = (currentPorts?.draft ?? []).map(({ protocol, port }) => ({ protocol, port: Number(port) }));
  const extraPortsChanged = currentPorts !== null && JSON.stringify(extraPorts) !== currentPorts.original;
  const extraPortsOk = extraPorts.length <= MAX_EXTRA_PORTS &&
    extraPorts.every(({ protocol, port }) => Number.isInteger(port) && port >= 1 && port <= 65535 && !(protocol === "TCP" && [25565, 25575].includes(port))) &&
    new Set(extraPorts.map(({ protocol, port }) => `${protocol}:${port}`)).size === extraPorts.length;

  function setPortDraft(draft: ExtraPortDraft[]) {
    setPorts((previous) => previous?.name === serverName ? { ...previous, draft } : previous);
  }

  const currentStartup = startup?.name === serverName ? startup : null;
  const startupChanges: Partial<ServerStartupSettings> = currentStartup ? Object.fromEntries(STARTUP_KEYS
    .filter((key) => JSON.stringify(currentStartup.draft[key]) !== JSON.stringify(currentStartup.original[key]))
    .map((key) => [key, currentStartup.draft[key]])) : {};
  const launchChanged = ["java_flags", "args", "env"].some((key) => key in startupChanges);

  async function loadStartup() {
    if (startupBusy.current) return;
    startupBusy.current = true;
    const ticket = ++startupGeneration.current;
    setStartupLoading(true);
    setStartupError(null);
    try {
      const value = await api.getStartupSettings(serverName);
      if (ticket === startupGeneration.current) setStartup({ name: serverName, original: value, draft: value });
    } catch (reason) {
      if (ticket === startupGeneration.current) setStartupError(humanizeError(reason));
    } finally {
      if (ticket === startupGeneration.current) { startupBusy.current = false; setStartupLoading(false); }
    }
  }

  function setStartupField<K extends keyof ServerStartupSettings>(key: K, value: ServerStartupSettings[K]) {
    setStartup((previous) => previous?.name === serverName ? { ...previous, draft: { ...previous.draft, [key]: value } } : previous);
  }

  // The console polls the status and hands a new software object each time, so
  // the form follows what the server runs by value: a poll alone never clears
  // a choice being made.
  const softwareKey = currentSoftware ? JSON.stringify(choiceOf(currentSoftware)) : "";

  const resetForm = useCallback(() => {
    setForm({
      displayName: currentDisplayName,
      tags: JSON.parse(tagsKey) as string[],
      autostartPolicy: currentPolicy,
      image: currentImage,
      memory: currentMemoryGiB,
      storage: currentStorageGiB,
      cpu: currentCpu,
      idleStop: currentIdleStop,
      resume: currentResume,
      crashRestart: currentCrashRestart,
      maxCrash: currentMaxCrash,
      notify: { started: notifyStarted, stopped: notifyStopped, problems: notifyProblems },
    });
    setTagDraft("");
    setError(null);
    setImageConfirmed(false);
    setSource(softwareKey ? "software" : "image");
    setChangingSoftware(false);
    setSoftware(softwareKey ? (JSON.parse(softwareKey) as SoftwareChoice) : EMPTY_SOFTWARE);
    setSoftwareConfirmed(false);
    setEula(false);
    setJarFile(null);
  }, [currentDisplayName, tagsKey, currentPolicy, currentImage, currentMemoryGiB, currentStorageGiB, currentCpu, currentIdleStop, softwareKey,
    currentResume, currentCrashRestart, currentMaxCrash, notifyStarted, notifyStopped, notifyProblems]);
  // The form follows the server whenever what it runs with changes, as after a save.
  useEffect(() => resetForm(), [resetForm]);

  function discard() {
    resetForm();
    setPorts(null);
    setStartup((previous) => previous && { ...previous, draft: previous.original });
  }

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

  // A tag typed but not yet added is saved with the others, so Save never drops it.
  const pendingTag = tagDraft.trim();
  const pendingIssue = pendingTag === "" ? null : tagIssue(pendingTag);
  const nextTags = pendingTag !== "" && pendingIssue === null ? mergeTags(form.tags, [pendingTag]) : form.tags;
  const tagError = pendingIssue ?? (nextTags.length > MAX_SERVER_TAGS ? "full" : null);
  const tagsChanged = tagError === null && JSON.stringify(nextTags) !== tagsKey;

  function addPendingTag() {
    if (pendingTag === "" || tagError !== null) return;
    set("tags", nextTags);
    setTagDraft("");
  }

  // A comma ends a tag: the finished ones are added, the rest stays in the box.
  function changeTagDraft(value: string) {
    if (!value.includes(",")) {
      setTagDraft(value);
      return;
    }
    const parts = value.split(",");
    const finished = parts.slice(0, -1).map((part) => part.trim()).filter((part) => part !== "");
    const merged = mergeTags(form.tags, finished);
    if (finished.some((part) => tagIssue(part) !== null) || merged.length > MAX_SERVER_TAGS) {
      setTagDraft(value.replaceAll(",", " "));
      return;
    }
    set("tags", merged);
    setTagDraft(parts[parts.length - 1]);
  }

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
  const storageGiB = parseGiB(form.storage);
  const storageOk = storageGiB !== null && storageGiB >= 1 && Number.isInteger(storageGiB);
  const storageChanged = storageGiB === null ? form.storage.trim() !== currentStorageGiB : storageGiB !== parseGiB(currentStorageGiB);

  const maxCrash = crashLimit(form.maxCrash);
  const maxCrashChanged = form.crashRestart && form.maxCrash.trim() !== currentMaxCrash;
  const notifyChanges = NOTIFY_KEYS.filter((k) => form.notify[k] !== currentNotify[k]);
  const recoveryChanged =
    !systemService &&
    (form.resume !== currentResume || form.crashRestart !== currentCrashRestart || maxCrashChanged || notifyChanges.length > 0);

  const hasChanges =
    extraPortsChanged ||
    Object.keys(startupChanges).length > 0 ||
    recoveryChanged ||
    tagsChanged ||
    displayName !== currentDisplayName ||
    form.autostartPolicy !== currentPolicy ||
    imageChanged ||
    softwareChanged ||
    !!jarUpload ||
    memoryChanged ||
    storageChanged ||
    cpu !== currentCpu ||
    form.idleStop !== currentIdleStop;
  useUnsavedGuard(!inDialog && hasChanges);

  const pinnedBuild = splitImageRef(currentImage).short;
  const canSubmit =
    hasChanges &&
    cpuOk &&
    tagError === null &&
    (!memoryChanged || memoryOk) &&
    (!storageChanged || storageOk) &&
    (!maxCrashChanged || maxCrash !== null) &&
    (!extraPortsChanged || extraPortsOk) &&
    !submitting &&
    (!imageChanged || imageConfirmed) &&
    (!softwareMoves || softwareConfirmed) &&
    (!softwareChanged || !!currentSoftware || eula);

  async function submit() {
    if (!canSubmit || saving.current) return;
    saving.current = true;
    const ticket = ++saveGeneration.current;
    setSubmitting(true);
    setError(null);
    try {
      const payload: Parameters<typeof api.patchServer>[1] = { ...startupChanges };
      if (extraPortsChanged) payload.extraPorts = extraPorts;

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
      // Memory, storage and CPU each go alone; the API keeps whatever is not sent.
      if (memoryChanged && memoryGiB !== null) {
        payload.memory = gibQuantity(memoryGiB);
      }
      if (storageChanged && storageGiB !== null) {
        payload.storage = gibQuantity(storageGiB);
      }
      if (cpu !== currentCpu) {
        payload.resources = { cpu };
      }
      if (!systemService && form.idleStop !== currentIdleStop) {
        payload.idleStopSeconds = Number(form.idleStop);
      }
      if (!systemService) {
        if (form.resume !== currentResume) payload.resumeAfterHostRestart = form.resume;
        if (form.crashRestart !== currentCrashRestart) payload.restartOnCrash = form.crashRestart;
        if (maxCrashChanged && maxCrash !== null) payload.maxCrashRestarts = maxCrash;
        if (notifyChanges.length > 0) {
          const notify: Partial<ServerNotify> = {};
          for (const k of notifyChanges) notify[k] = form.notify[k];
          payload.notify = notify;
        }
      }

      if (tagsChanged) {
        await api.setServerTags(serverName, nextTags);
        if (ticket !== saveGeneration.current) return;
      }
      // The jar goes first: the software then names a file that is there.
      if (jarUpload) {
        setUploaded(0);
        await uploadFile(serverName, software.jar.trim(), jarUpload, { overwrite: true, onProgress: (sent) => {
          if (ticket === saveGeneration.current) setUploaded(sent);
        } });
        if (ticket !== saveGeneration.current) return;
      }
      if (Object.keys(payload).length > 0) {
        if (admin) {
          await api.patchServer(serverName, payload);
        } else {
          const { resources, ...rest } = payload;
          await api.patchMyServer(serverName, { ...rest, ...(resources ? { cpu: resources.cpu } : {}) });
        }
      }
      if (ticket !== saveGeneration.current) return;
      // What was saved becomes the base, so the page shows no change while the status catches up.
      setPorts((previous) => previous && { ...previous, original: JSON.stringify(extraPorts) });
      setStartup((previous) => previous && { ...previous, original: previous.draft });
      onDone?.();
      onUpdated();
    } catch (e) {
      if (ticket === saveGeneration.current) setError(humanizeError(e));
    } finally {
      if (ticket === saveGeneration.current) { saving.current = false; setSubmitting(false); setUploaded(null); }
    }
  }

  const policies = policyOptions(t);
  const idleOn = form.idleStop !== "0";
  const allocated = (protocol: string, port: string) => currentExtraPorts?.find((entry) => entry.protocol === protocol && String(entry.port) === port);

  return (
    <InDialog.Provider value={inDialog}>
      <div className={cn("grid", inDialog ? "gap-5" : "gap-6")}>
        <SettingsSection title={t("settings_section_general")}>
          <SettingRow label={t("settings_display_name")} hint={t("settings_display_name_hint", { name: serverName })} htmlFor="es-display">
            <Input
              id="es-display"
              placeholder={serverName}
              maxLength={DISPLAY_NAME_MAX}
              value={form.displayName}
              onChange={(e) => set("displayName", e.target.value)}
            />
          </SettingRow>

          {!systemService && (
            <SettingRow
              label={t("edit_server_tags")}
              htmlFor="es-tags"
              hint={<span id="es-tags-hint" className={tagError ? "text-destructive" : undefined}>
                {tagError
                  ? t(`edit_server_tags_${tagError}`, { length: MAX_SERVER_TAG_LENGTH, max: MAX_SERVER_TAGS })
                  : t("edit_server_tags_hint", { length: MAX_SERVER_TAG_LENGTH, max: MAX_SERVER_TAGS })}
              </span>}
            >
              <div className="grid gap-2">
                {form.tags.length > 0 && (
                  <ul aria-label={t("tags_label")} className="flex flex-wrap gap-1.5">
                    {form.tags.map((tag) => (
                      <li
                        key={tag}
                        className="inline-flex max-w-full items-center gap-1 rounded-full border border-border bg-muted/50 py-0.5 pl-2.5 pr-1 text-xs"
                      >
                        <span className="truncate">{tag}</span>
                        <button
                          type="button"
                          aria-label={t("edit_server_tag_remove", { tag })}
                          onClick={() => set("tags", form.tags.filter((other) => other !== tag))}
                          className="shrink-0 rounded-full p-0.5 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        >
                          <X className="h-3 w-3" />
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
                <div className="flex gap-2">
                  <Input
                    id="es-tags"
                    placeholder={t("edit_server_tags_placeholder")}
                    value={tagDraft}
                    autoComplete="off"
                    onChange={(e) => changeTagDraft(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key !== "Enter") return;
                      e.preventDefault();
                      if (!e.nativeEvent.isComposing) addPendingTag();
                    }}
                    aria-invalid={tagError !== null}
                    aria-describedby="es-tags-hint"
                  />
                  <Button type="button" variant="outline" disabled={pendingTag === "" || tagError !== null} onClick={addPendingTag}>
                    {t("edit_server_tags_add")}
                  </Button>
                </div>
              </div>
            </SettingRow>
          )}

          {showOwner && (
            <SettingRow label={t("edit_server_owner")} hint={t("settings_owner_hint")}>
              <div className="flex items-center justify-between gap-2 lg:justify-end">
                <span className="truncate text-sm font-medium">{ownerLabel}</span>
                <Button type="button" variant="outline" size="sm" onClick={() => setAssignOpen(true)}>
                  {t("edit_server_owner_change")}
                </Button>
              </div>
              <AssignServerDialog
                open={assignOpen}
                onOpenChange={setAssignOpen}
                server={{ name: serverName, label: currentDisplayName || serverName, owner: ownerName }}
                onAssigned={() => {
                  fleet.reload();
                  onUpdated();
                }}
              />
            </SettingRow>
          )}
        </SettingsSection>

        <SettingsSection title={t("settings_section_software")}>
          {admin && !systemService && (
            <SettingRow label={t("create_server_source")} hint={t("settings_source_hint")}>
              <SegmentedControl
                label={t("create_server_source")}
                value={source}
                onChange={(k) => { setSource(k); setChangingSoftware(false); setSoftware(choiceOf(currentSoftware)); }}
                segments={[
                  { key: "software", label: t("software_source_software") },
                  { key: "image", label: t("software_source_image") },
                ]}
              />
            </SettingRow>
          )}

          {source === "software" || !admin ? (
            <SettingRow label={t("edit_server_software")} hint={t("settings_software_hint")} block={pickingSoftware}>
              {!pickingSoftware ? (
                <div className="flex items-center justify-between gap-3 lg:justify-end">
                  <span className="min-w-0 text-sm">
                    {currentSoftware
                      ? t("edit_server_software_current", { summary: softwareSummary(t, currentSoftware), java: currentSoftware.java })
                      : t("edit_server_software_image", { image: splitImageRef(currentImage).tag || currentImage })}
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    className="shrink-0"
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
            </SettingRow>
          ) : (
            <SettingRow label={t("create_server_image")} htmlFor="edit-server-image"
              hint={!imageChanged && pinnedBuild ? t("edit_server_image_pinned_hint", { id: pinnedBuild }) : undefined}>
              <div className="grid gap-2">
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
                {imageChanged && (
                  <div className="grid gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs">
                    <p className="flex items-start gap-1.5 text-amber-700 dark:text-amber-300">
                      <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
                      {t("edit_server_image_warning")}
                    </p>
                    <label className="flex cursor-pointer items-center gap-2 font-medium text-foreground">
                      <Checkbox className="mt-0.5" checked={imageConfirmed} onChange={(e) => setImageConfirmed(e.target.checked)} />
                      {t("edit_server_image_confirm")}
                    </label>
                  </div>
                )}
              </div>
            </SettingRow>
          )}
        </SettingsSection>

        <SettingsSection title={t("settings_section_resources")}>
          <SettingRow
            label={t("create_server_memory")}
            htmlFor="edit-server-memory"
            hint={<span id="edit-server-memory-hint" className={memoryChanged && !memoryOk ? "text-destructive" : undefined}>
              {memoryChanged && !memoryOk ? t("create_server_memory_invalid") : t("create_server_memory_hint")}
            </span>}
          >
            <GiBInput
              id="edit-server-memory"
              value={form.memory}
              onChange={(v) => set("memory", v)}
              invalid={memoryChanged && !memoryOk}
              describedBy="edit-server-memory-hint"
            />
          </SettingRow>
          <SettingRow
            label={t("create_server_storage")}
            htmlFor="edit-server-storage"
            hint={<span id="edit-server-storage-hint" className={storageChanged && !storageOk ? "text-destructive" : undefined}>
              {storageChanged && !storageOk ? t("create_server_storage_invalid") : t("edit_server_storage_hint")}
            </span>}
          >
            <GiBInput
              id="edit-server-storage"
              value={form.storage}
              onChange={(v) => set("storage", v)}
              invalid={storageChanged && !storageOk}
              describedBy="edit-server-storage-hint"
            />
          </SettingRow>
          <SettingRow
            label={t("edit_server_cpu")}
            htmlFor="es-cpu"
            hint={cpuOk ? t("settings_cpu_hint") : <span id="es-cpu-error" className="text-destructive">{t("edit_server_cpu_invalid")}</span>}
          >
            <Input
              id="es-cpu"
              placeholder={t("settings_unlimited")}
              value={form.cpu}
              onChange={(e) => set("cpu", e.target.value)}
              aria-invalid={!cpuOk}
              aria-describedby={cpuOk ? undefined : "es-cpu-error"}
            />
          </SettingRow>
        </SettingsSection>

        {!systemService && (
          <SettingsSection title={t("settings_section_running")}>
            <SettingRow label={t("create_server_policy")} hint={t("settings_policy_hint")} htmlFor="edit-server-policy">
              <Select value={form.autostartPolicy} onValueChange={(v) => set("autostartPolicy", v as AutostartPolicy)}>
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
            </SettingRow>
            <SettingRow
              label={t("edit_server_idle")}
              htmlFor="es-idle"
              hint={<>
                {t("edit_server_idle_hint")}
                {playerCountUnknown && idleOn && (
                  <span className="mt-1 flex items-start gap-1.5 text-amber-600 dark:text-amber-400">
                    <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
                    {t("edit_server_idle_unknown")}
                  </span>
                )}
              </>}
            >
              <div className="flex items-center gap-3 lg:justify-end">
                {idleOn && (
                  <Select value={form.idleStop} onValueChange={(v) => set("idleStop", v)}>
                    <SelectTrigger id="edit-server-idle" aria-label={t("settings_idle_after")} className="w-44">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {/* A duration set outside the presets (kubectl, converge) stays selectable. */}
                      {!IDLE_OPTIONS.includes(form.idleStop) && (
                        <SelectItem value={form.idleStop}>{t("settings_idle_after_value", { duration: idleLabel(t, Number(form.idleStop)) })}</SelectItem>
                      )}
                      {IDLE_OPTIONS.map((s) => (
                        <SelectItem key={s} value={s}>
                          {t("settings_idle_after_value", { duration: idleLabel(t, Number(s)) })}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
                <Switch
                  id="es-idle"
                  checked={idleOn}
                  onCheckedChange={(on) => set("idleStop", on ? (currentIdleStop !== "0" ? currentIdleStop : DEFAULT_IDLE_STOP) : "0")}
                />
              </div>
            </SettingRow>
            <SettingRow label={t("edit_server_resume")} hint={t("edit_server_resume_hint")} htmlFor="es-resume">
              <div className="flex lg:justify-end">
                <Switch id="es-resume" checked={form.resume} onCheckedChange={(v) => set("resume", v)} />
              </div>
            </SettingRow>
            <SettingRow
              label={t("edit_server_crash_restart")}
              htmlFor="es-crash-restart"
              hint={form.crashRestart ? t("edit_server_crash_restart_hint") : t("edit_server_crash_off_hint")}
            >
              <div className="flex lg:justify-end">
                <Switch id="es-crash-restart" checked={form.crashRestart} onCheckedChange={(v) => set("crashRestart", v)} />
              </div>
            </SettingRow>
            {form.crashRestart && (
              <SettingRow
                label={t("edit_server_crash_max")}
                htmlFor="es-crash-max"
                hint={<span id="es-crash-max-hint" className={maxCrashChanged && maxCrash === null ? "text-destructive" : undefined}>
                  {maxCrashChanged && maxCrash === null
                    ? t("edit_server_crash_max_invalid", { max: MAX_CRASH_RESTARTS })
                    : t("edit_server_crash_max_hint")}
                </span>}
              >
                <div className="flex items-center gap-2 text-sm lg:justify-end">
                  <Input
                    id="es-crash-max"
                    inputMode="numeric"
                    className="w-20 text-center"
                    value={form.maxCrash}
                    onChange={(e) => set("maxCrash", e.target.value)}
                    aria-invalid={maxCrashChanged && maxCrash === null}
                    aria-describedby="es-crash-max-hint"
                  />
                  <span className="text-muted-foreground">{t("edit_server_crash_max_unit")}</span>
                </div>
              </SettingRow>
            )}
          </SettingsSection>
        )}

        {!systemService && (
          <SettingsSection title={t("edit_server_notify")} description={t("edit_server_notify_hint")}>
            {NOTIFY_KEYS.map((k) => (
              <SettingRow key={k} label={t(`edit_server_notify_${k}`)} htmlFor={`es-notify-${k}`}
                hint={k === "problems" ? t("edit_server_notify_problems_hint") : undefined}>
                <div className="flex lg:justify-end">
                  <Switch id={`es-notify-${k}`} checked={form.notify[k]} onCheckedChange={(v) => set("notify", { ...form.notify, [k]: v })} />
                </div>
              </SettingRow>
            ))}
          </SettingsSection>
        )}

        <SettingsSection title={t("settings_network_title")}>
          {network}
          <SettingRow
            label={t("extra_ports_title")}
            block
            hint={<>
              <span id={`${portsId}-hint`}>{t("extra_ports_edit_hint")}</span>
              {currentExtraPortsPending && <span role="status" className="mt-1 block">{t("extra_ports_pending")}</span>}
            </>}
          >
            <fieldset disabled={submitting} className="min-w-0 space-y-3">
              {currentPorts && currentPorts.draft.map((entry, index) => {
                const live = allocated(entry.protocol, entry.port);
                return (
                  <div key={index} className="grid min-w-0 gap-2 sm:grid-cols-[8rem_10rem_minmax(0,1fr)_auto] sm:items-end">
                    <div className="min-w-0 space-y-1">
                      <Label htmlFor={`${portsId}-protocol-${index}`} className="block text-xs">{t("extra_ports_protocol", { number: index + 1 })}</Label>
                      <Select value={entry.protocol} disabled={submitting} onValueChange={(value) => setPortDraft(currentPorts.draft.map((item, at) => at === index ? { ...item, protocol: value as ServerExtraPort["protocol"] } : item))}>
                        <SelectTrigger id={`${portsId}-protocol-${index}`} aria-describedby={`${portsId}-hint`}><SelectValue /></SelectTrigger>
                        <SelectContent><SelectItem value="TCP">TCP</SelectItem><SelectItem value="UDP">UDP</SelectItem></SelectContent>
                      </Select>
                    </div>
                    <div className="min-w-0 space-y-1">
                      <Label htmlFor={`${portsId}-port-${index}`} className="block text-xs">{t("extra_ports_internal", { number: index + 1 })}</Label>
                      <Input id={`${portsId}-port-${index}`} type="number" inputMode="numeric" min={1} max={65535} step={1} value={entry.port}
                        aria-invalid={extraPortsChanged && !extraPortsOk} aria-describedby={`${portsId}-hint${extraPortsChanged && !extraPortsOk ? ` ${portsId}-error` : ""}`}
                        onChange={(event) => setPortDraft(currentPorts.draft.map((item, at) => at === index ? { ...item, port: event.target.value } : item))} />
                    </div>
                    <div className="min-w-0 pb-2 text-xs text-muted-foreground">
                      {live?.address ? <CopyAddress address={live.address} /> : live?.nodePort !== undefined ? t("extra_ports_external", { port: live.nodePort }) : null}
                    </div>
                    <Button type="button" variant="ghost" size="icon" aria-label={t("extra_ports_remove", { number: index + 1 })}
                      onClick={() => setPortDraft(currentPorts.draft.filter((_, at) => at !== index))}><Trash2 className="h-4 w-4" /></Button>
                  </div>
                );
              })}
              <div className="flex flex-wrap items-center gap-3">
                <Button type="button" variant="outline" size="sm" disabled={!currentPorts || currentPorts.draft.length >= MAX_EXTRA_PORTS}
                  onClick={() => currentPorts && setPortDraft([...currentPorts.draft, { protocol: "TCP", port: "" }])}><Plus className="h-4 w-4" />{t("extra_ports_add")}</Button>
                <span className="text-xs text-muted-foreground">{t("extra_ports_count", { count: currentPorts?.draft.length ?? 0, max: MAX_EXTRA_PORTS })}</span>
              </div>
              {extraPortsChanged && !extraPortsOk && <p id={`${portsId}-error`} className="text-xs text-destructive">{t("extra_ports_invalid")}</p>}
            </fieldset>
          </SettingRow>
        </SettingsSection>

        <SettingsSection title={t("edit_startup_title")} description={t("edit_startup_hint")}>
          <div className={cn(!inDialog && "px-5 py-4")}>
            <Button type="button" variant="outline" size="sm" aria-expanded={startupOpen} aria-controls={startupId}
              onClick={() => {
                setStartupOpen(!startupOpen);
                if (!startupOpen && !currentStartup) void loadStartup();
              }}>
              <ChevronRight className={`h-4 w-4 shrink-0 transition-transform ${startupOpen ? "rotate-90" : ""}`} />
              {t(startupOpen ? "settings_startup_hide" : "settings_startup_show")}
            </Button>
            {startupOpen && <div id={startupId} className="mt-4 min-w-0 space-y-3">
              {startupLoading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("edit_startup_loading")}</p>}
              {startupError && <div className="space-y-2">
                <InlineError message={startupError} />
                <Button type="button" variant="outline" size="sm" disabled={startupLoading} onClick={() => void loadStartup()}>{t("edit_startup_retry")}</Button>
              </div>}
              {currentStartup && <fieldset disabled={submitting} className="grid min-w-0 gap-3 lg:grid-cols-2">
                {STARTUP_LISTS.map(({ key, max }) => <StartupArgumentList key={key} label={t(`edit_startup_${key}`)} hint={t(`edit_startup_${key}_hint`)}
                  values={currentStartup.draft[key]} max={max} onChange={(value) => setStartupField(key, value)} />)}
                <fieldset className="min-w-0 space-y-3 rounded-md border border-border p-3">
                  <legend className="px-1 text-sm font-medium">{t("edit_startup_env")}</legend>
                  <p id={`${startupId}-env-hint`} className="text-xs leading-relaxed text-muted-foreground">{t("edit_startup_env_hint")}</p>
                  {currentStartup.draft.env.map((entry, index) => <div key={index} className="min-w-0 space-y-2 rounded-md border border-border p-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="text-xs font-medium">{t("edit_startup_env_item", { number: index + 1 })}</span>
                      <Button type="button" variant="ghost" size="icon" aria-label={t("edit_startup_env_remove", { number: index + 1 })}
                        onClick={() => setStartupField("env", currentStartup.draft.env.filter((_, at) => at !== index))}>
                        <Trash2 className="h-4 w-4" />
                      </Button>
                    </div>
                    <div className="grid min-w-0 gap-2 sm:grid-cols-2">
                      <div className="min-w-0 space-y-1">
                        <Label htmlFor={`${startupId}-env-name-${index}`}>{t("edit_startup_env_name", { number: index + 1 })}</Label>
                        <Input id={`${startupId}-env-name-${index}`} value={entry.name} autoComplete="off" spellCheck={false} className="font-mono text-base sm:text-sm"
                          aria-describedby={`${startupId}-env-hint`} onChange={(event) => setStartupField("env", currentStartup.draft.env.map((item, at) => at === index ? { ...item, name: event.target.value } : item))} />
                      </div>
                      <div className="min-w-0 space-y-1">
                        <Label htmlFor={`${startupId}-env-value-${index}`}>{t("edit_startup_env_value", { number: index + 1 })}</Label>
                        <Textarea id={`${startupId}-env-value-${index}`} value={entry.value} rows={2} className="min-h-16 font-mono text-base sm:text-sm" spellCheck={false}
                          aria-describedby={`${startupId}-env-hint`} onChange={(event) => setStartupField("env", currentStartup.draft.env.map((item, at) => at === index ? { ...item, value: event.target.value } : item))} />
                      </div>
                    </div>
                  </div>)}
                  <Button type="button" variant="outline" size="sm" disabled={currentStartup.draft.env.length >= 64}
                    onClick={() => setStartupField("env", [...currentStartup.draft.env, { name: "", value: "" }])}><Plus className="h-4 w-4" />{t("edit_startup_env_add")}</Button>
                  <p className="text-xs text-muted-foreground">{t("edit_startup_count", { count: currentStartup.draft.env.length, max: 64 })}</p>
                </fieldset>
              </fieldset>}
            </div>}
          </div>
        </SettingsSection>

        {(launchChanged && (currentPhase === "Starting" || currentPhase === "Running")) || (uploaded !== null && jarUpload) ? (
          <div className="space-y-1">
            {launchChanged && (currentPhase === "Starting" || currentPhase === "Running") && <p role="status" className="text-xs text-amber-700 dark:text-amber-300">{t("edit_startup_restart_hint")}</p>}
            {uploaded !== null && jarUpload && (
              <p role="status" className="text-xs text-muted-foreground">
                {t("create_server_uploading", { sent: formatBytes(uploaded), total: formatBytes(jarUpload.size) })}
              </p>
            )}
          </div>
        ) : null}

        {inDialog ? (
          <>
            <InlineError message={error} />
            <ConfirmFooter
              onCancel={() => onDone?.()}
              onConfirm={submit}
              disabled={!canSubmit}
              loading={submitting}
              cancelLabel={t("create_server_cancel")}
              confirmLabel={t("edit_server_submit")}
              confirmVariant="default"
            />
          </>
        ) : (hasChanges || error) && (
          <div role="region" aria-label={t("settings_save_bar")} className="sticky bottom-4 z-10 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-background/95 px-4 py-3 shadow-lg backdrop-blur supports-[backdrop-filter]:bg-background/80">
            <div className="min-w-0 text-sm">
              {error ? <InlineError message={error} /> : <span className="text-muted-foreground">{t("settings_unsaved")}</span>}
            </div>
            <div className="flex gap-2">
              <Button type="button" variant="ghost" disabled={submitting} onClick={discard}>{t("settings_discard")}</Button>
              <Button type="button" disabled={!canSubmit} onClick={() => void submit()}>
                {submitting && <Loader2 className="h-4 w-4 animate-spin" />}{t("edit_server_submit")}
              </Button>
            </div>
          </div>
        )}
      </div>
    </InDialog.Provider>
  );
}
