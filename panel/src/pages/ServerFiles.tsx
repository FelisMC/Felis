import { lazy, Suspense, useCallback, useEffect, useRef, useState } from "react";
import { useParams, useSearchParams } from "react-router-dom";
import {
  AlertTriangle,
  ArrowUp,
  ChevronRight,
  Download,
  FileArchive,
  FilePlus,
  FileText,
  Folder,
  FolderOpen,
  FolderPlus,
  Loader2,
  Search,
  Pencil,
  RefreshCw,
  Save,
  Square,
  Trash2,
  Upload,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { BackLink } from "@/components/BackLink";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { MessageLine } from "@/components/MessageLine";
import { useToast } from "@/components/Toaster";
import { InlineConfirm } from "@/components/InlineConfirm";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { NameDialog } from "@/components/files/NameDialog";
import { FileOps } from "@/components/files/FileOps";
import { UploadQueue } from "@/components/files/UploadQueue";
import { FileMenu } from "@/components/files/FileMenu";
import { ServerPropertiesEditor } from "@/components/files/ServerPropertiesEditor";
import { YamlConfigEditor } from "@/components/files/YamlConfigEditor";
import { YAML_FIELDS } from "@/components/files/yamlFields";
import { PaperGlobalConfigDialog } from "@/components/files/PaperGlobalConfigDialog";
import { droppedFiles } from "@/components/files/folderUpload";
import { imageMime } from "@/components/files/imagePreview";
import { Input } from "@/components/ui/input";
import { touchesWorld } from "@/components/files/worldPaths";
import { useFileWorldConfirmation } from "@/components/files/useFileWorldConfirmation";
import { useFileOps } from "@/components/files/useFileOps";
import { useUploads } from "@/components/files/useUploads";
import { opErrorText } from "@/components/files/opText";
import { holderText, useWorldJobs } from "@/components/files/useWorldJobs";
import {
  SECRET_CONFIG_PATH,
  isManaged,
  joinPath,
  nameProblem,
  parentOf,
  sortEntries,
  type NameProblem,
} from "@/components/files/names";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { PhaseBadge, shownPhase, startFailure } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
import { DiskUsage } from "@/components/ResourceUsage";
import { Loading, ErrorState, NotYours, EmptyState } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { STATUS_POLL_FAST_MS, useAsync, usePolling, useUnsavedGuard } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage, ownershipPending } from "@/lib/ownership";
import { awaitExport, saveDownload } from "@/lib/download";
import { formatBytes, formatRelative } from "@/lib/format";
import type { FileOp, ServerFileEntry } from "@/lib/types";
import { cn } from "@/lib/utils";

const MAX_WRITE_BYTES = 5 * 1024 * 1024;
const MAX_READ_BYTES = MAX_WRITE_BYTES;
const TextFileEditor = lazy(() => import("@/components/files/TextFileEditor").then((m) => ({ default: m.TextFileEditor })));
const BINARY_EXTENSION = /\.(jar|zip|gz|tar|7z|rar|dat|mca|mcr|schem|schematic|nbt|db|sqlite|png|jpe?g|gif|webp|ico|ogg|mp[34]|pdf|class|exe|dll|so|bin)$/i;

/** encoding/json renders []byte as base64; chunked to stay under the String.fromCharCode argument limit. */
function bytesToBase64(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(bin);
}

function base64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** Returns null for invalid UTF-8 or NUL bytes: such files open read-only because a textarea round-trip would corrupt them. */
function decodeText(bytes: Uint8Array): string | null {
  try {
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return text.includes("\u0000") ? null : text;
  } catch {
    return null;
  }
}

function isArchive(name: string): boolean {
  return /\.(zip|tar|tar\.gz|tgz)$/i.test(name);
}

type FileTask = { kind: "transfer"; op: "copy" | "move"; paths: string[]; to: string }
  | { kind: "zip"; paths: string[]; to: string }
  | { kind: "url"; url: string; path: string };

type Naming =
  | { kind: "file" | "folder"; dir: string }
  | { kind: "rename"; dir: string; entry: ServerFileEntry };

const OP_FINISHED_KEY = {
  copy: "op_finished_copy",
  move: "op_finished_move",
  zip: "op_finished_zip",
  unzip: "op_finished_unzip",
  upload: "op_finished_upload",
} as const;

/** World changes on a running server need an explicit acknowledgement. */
export function ServerFiles() {
  const { name = "" } = useParams();
  const [params] = useSearchParams();
  const { t, i18n } = useTranslation("files");
  const toast = useToast();
  const locale = i18n.language;
  const { isAdmin, loading: tierLoading } = useTier();

  const statusQ = useAsync(() => api.status(name), [name]);
  const mineQ = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name],
  );
  const pending = ownershipPending(tierLoading, isAdmin, mineQ.data, mineQ.error);
  const owned = canManage(isAdmin, mineQ.data, name);
  const stopped = statusQ.data?.phase === "Stopped";
  const live = statusQ.data?.phase === "Running" && statusQ.data.desiredState === "Running";
  const accessible = stopped || live;
  const { confirmed: worldConfirmed, setConfirmed: setWorldConfirmed, directories: worldDirs,
    needsConfirmation: needsWorldConfirmation, requireConfirmation: requireWorldConfirmation,
    reload: reloadWorlds } = useFileWorldConfirmation(name, owned, live);

  function worldConfirmation(id: string) {
    return <label htmlFor={id} className="flex items-start gap-2 text-sm">
      <Checkbox id={id} checked={worldConfirmed} onChange={(e) => setWorldConfirmed(e.target.checked)} />
      <span>{t("live_world_confirm")}</span>
    </label>;
  }

  const [dir, setDir] = useState(params.get("path") ?? "");
  const [entries, setEntries] = useState<ServerFileEntry[] | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [listErr, setListErr] = useState<unknown>(null);
  const [listLoading, setListLoading] = useState(false);
  const [listTarget, setListTarget] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const [checked, setChecked] = useState<ReadonlySet<string>>(new Set());
  const [clipboard, setClipboard] = useState<{ server: string; op: "copy" | "move"; paths: string[] } | null>(null);
  const [query, setQuery] = useState("");
  const [searching, setSearching] = useState(false);
  const [searchActive, setSearchActive] = useState(false);
  const [menuPath, setMenuPath] = useState<string | null>(null);
  const longPress = useRef<ReturnType<typeof setTimeout> | null>(null);
  const longPressed = useRef(false);
  const folders = useRef(new Map<string, { at: number; listing: Awaited<ReturnType<typeof api.listServerFiles>> }>());
  // null while unknown; 0 is a full volume.
  const [free, setFree] = useState<number | null>(null);
  const [msg, setMsg] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  // Only the newest load lands: rows stay clickable while a folder loads, so a slow answer must not replace a later one.
  const loadSeq = useRef(0);
  const load = useCallback(
    async (p: string, cached = false) => {
      const ticket = ++loadSeq.current;
      setSearching(false);
      if (!cached) folders.current.clear();
      const previous = folders.current.get(`${name}:${p}`);
      setListLoading(true);
      setListTarget(p);
      setListErr(null);
      try {
        const fresh = cached && previous && Date.now() - previous.at < 30_000;
        const r = fresh
          ? previous.listing
          : await api.listServerFiles(name, p);
        if (ticket !== loadSeq.current) return;
        if (folders.current.size >= 20) folders.current.delete(folders.current.keys().next().value!);
        folders.current.set(`${name}:${p}`, { at: fresh ? previous.at : Date.now(), listing: r });
        setEntries(sortEntries(r.entries ?? []));
        setTruncated(r.truncated === true);
        setFree(r.free_bytes ?? null);
        setDir(p);
        setSearchActive(false);
        setChecked(new Set());
        setMenuPath(null);
      } catch (e) {
        if (ticket !== loadSeq.current) return;
        setEntries(null);
        setListErr(e);
      } finally {
        if (ticket === loadSeq.current) setListLoading(false);
      }
    },
    [name],
  );

  // Exclude dir from dependencies because load changes it; including it would duplicate directory fetches.
  useEffect(() => {
    if (owned && accessible) void load(dir);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [owned, accessible, load]);
  useEffect(() => {
    if (!accessible) folders.current.clear();
  }, [accessible]);

  usePolling(statusQ.reload, STATUS_POLL_FAST_MS);

  // Send the read SHA-256 on save to reject concurrent edits with file_changed.
  const [open, setOpen] = useState<{
    path: string;
    text: string;
    original: string;
    editable: boolean;
    sha256: string;
    conflict: boolean;
    error: string | null;
    preview?: "binary" | "large";
    size?: number;
  } | null>(null);
  const [opening, setOpening] = useState<string | null>(null);
  const [image, setImage] = useState<{ path: string; url: string } | null>(null);
  const previewAbort = useRef<AbortController | null>(null);
  useEffect(() => () => { if (image) URL.revokeObjectURL(image.url); }, [image]);
  useEffect(() => () => previewAbort.current?.abort(), []);
  const openingSeq = useRef(0);
  useEffect(() => () => { ++openingSeq.current; ++loadSeq.current; }, []);
  const [saving, setSaving] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [configValid, setConfigValid] = useState(true);
  const invalidConfig = !!open && (open.path === "server.properties" || !!YAML_FIELDS[open.path]) && !configValid;
  const [paperGlobalOpen, setPaperGlobalOpen] = useState(false);
  // Closing an editor with unsaved text (Esc, the overlay, ✕, Cancel) asks
  // first; leaving the page asks through the browser.
  const [confirmDiscard, setConfirmDiscard] = useState(false);
  const dirty = open !== null && open.editable && open.text !== open.original;
  useUnsavedGuard(dirty);

  function requestClose() {
    if (saving || reloading) return;
    if (dirty) {
      setConfirmDiscard(true);
      return;
    }
    ++openingSeq.current;
    previewAbort.current?.abort();
    setOpening(null);
    setOpen(null);
  }

  function discardEdits() {
    setConfirmDiscard(false);
    setOpen(null);
  }

  async function readInto(p: string, ticket = openingSeq.current) {
    const r = await api.readServerFile(name, p);
    if (ticket !== openingSeq.current) return;
    const bytes = base64ToBytes(r.content ?? "");
    const text = decodeText(bytes);
    setConfirmDiscard(false);
    setOpen({
      path: p,
      text: text ?? "",
      original: text ?? "",
      editable: text !== null,
      sha256: r.sha256 ?? "",
      conflict: false,
      error: null,
      preview: text === null ? "binary" : undefined,
      size: bytes.length,
    });
  }

  async function openFile(entry: ServerFileEntry) {
    if (opening !== null) return;
    const p = joinPath(dir, entry.name);
    setSelected(p);
    setMsg(null);
    if (p === SECRET_CONFIG_PATH) {
      if (!owned || pending) return;
      if (!accessible || statusQ.error || changing) {
        setMsg({ kind: "error", text: changing ? waitTitle : t("paper_global_unavailable") });
        return;
      }
      setPaperGlobalOpen(true);
      return;
    }
    if (/\.(png|jpe?g|gif|webp)$/i.test(entry.name) && entry.size <= MAX_READ_BYTES) {
      const ticket = ++openingSeq.current;
      setOpening(p);
      try {
        const tk = await api.downloadServerFile(name, p, false);
        const result = await awaitExport(tk.ticket, () => alive.current && ticket === openingSeq.current);
        if (!result) return;
        if (result.state === "failed") throw new Error(t("download_failed"));
        const controller = new AbortController();
        previewAbort.current = controller;
        const blob = await api.previewServerFile(tk.ticket, MAX_READ_BYTES, controller.signal);
        if (ticket !== openingSeq.current) return;
        const bytes = new Uint8Array(await blob.arrayBuffer());
        const mime = imageMime(bytes);
        if (!mime) throw new Error(t("image_invalid"));
        setImage({ path: p, url: URL.createObjectURL(new Blob([bytes], { type: mime })) });
        worldJobs.refresh();
      } catch (e) {
        if (ticket === openingSeq.current) setMsg({ kind: "error", text: humanizeError(e) });
      } finally {
        if (ticket === openingSeq.current) setOpening(null);
      }
      return;
    }
    const preview = BINARY_EXTENSION.test(entry.name) ? "binary" : entry.size > MAX_READ_BYTES ? "large" : undefined;
    if (preview) {
      setOpen({ path: p, text: "", original: "", editable: false, sha256: "", conflict: false, error: null, preview, size: entry.size });
      return;
    }
    const ticket = ++openingSeq.current;
    setOpening(p);
    setMsg(null);
    try {
      await readInto(p, ticket);
    } catch (e) {
      if (ticket === openingSeq.current) setMsg({ kind: "error", text: humanizeError(e) });
    } finally {
      if (ticket === openingSeq.current) setOpening(null);
    }
  }

  // reloadOpen resolves a conflict by taking the file as it is now.
  async function reloadOpen() {
    if (!open || reloading) return;
    setReloading(true);
    try {
      await readInto(open.path);
    } catch (e) {
      setOpen({ ...open, error: humanizeError(e) });
    } finally {
      setReloading(false);
    }
  }

  // handleSave writes the editor's text. `overwrite` drops the precondition,
  // which is what "Overwrite anyway" on a conflict means.
  async function handleSave(overwrite = false) {
    if (!open?.editable || saving || reloading || (!overwrite && open.conflict) || !dirty || tooLarge || invalidConfig) return;
    setSaving(true);
    setMsg(null);
    setOpen({ ...open, error: null });
    try {
      requireWorldConfirmation([open.path]);
      await api.writeServerFile(
        name,
        open.path,
        bytesToBase64(new TextEncoder().encode(open.text)),
        overwrite ? undefined : open.sha256 || undefined,
      );
      setMsg({ kind: "success", text: t("saved", { path: open.path }) });
      setConfirmDiscard(false);
      setOpen(null);
      void load(dir);
      if (open.path === "server.properties") reloadWorlds();
    } catch (e) {
      if ((e as { code?: string }).code === "file_changed") {
        setOpen({ ...open, conflict: true, error: null });
      } else {
        setOpen({ ...open, error: humanizeError(e) });
      }
    } finally {
      setSaving(false);
    }
  }

  // A succeeded op rereads the listing; an extraction stopped at files it would replace asks about them.
  const startedHere = useRef(new Set<string>());
  const taskRequests = useRef(new Map<string, FileTask>());
  const [conflicts, setConflicts] = useState<FileOp | null>(null);
  const [conflictsOpen, setConflictsOpen] = useState(false);
  useEffect(() => {
    startedHere.current.clear();
    taskRequests.current.clear();
    setConflicts(null);
    setConflictsOpen(false);
    setClipboard(null);
    setChecked(new Set());
    setMenuPath(null);
    setImage(null);
    previewAbort.current?.abort();
    setOpen(null);
    setPaperGlobalOpen(false);
    setOpening(null);
    ++openingSeq.current;
  }, [name]);
  function showConflicts(op: FileOp) {
    setConflicts(op);
    setConflictsOpen(true);
  }
  function opEnded(op: FileOp) {
    if (op.state === "succeeded") {
      const request = taskRequests.current.get(op.id);
      if (request?.kind === "transfer" && request.op === "move") setClipboard(null);
      void load(dir);
      reloadWorlds();
      toast.success(t(OP_FINISHED_KEY[op.op]));
    } else if (op.error?.code === "file_exists" && startedHere.current.has(op.id)) {
      showConflicts(op);
    } else {
      toast.error(opErrorText(op.op, op.error ?? { code: "job_failed", message: "" }));
    }
  }
  const fileOps = useFileOps(name, owned && accessible, opEnded);
  // Backups, restores, exports and downloads also hold the world, whoever started them.
  const worldJobs = useWorldJobs(name, owned && accessible, () => void load(dir));
  const held = worldJobs.holder;
  useEffect(() => {
    if (held !== null) folders.current.clear();
  }, [held]);

  const [downloading, setDownloading] = useState<string | null>(null);
  const [unzipping, setUnzipping] = useState<string | null>(null);
  const [startingTask, setStartingTask] = useState(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  // The listing is reread once the queue has drained, since every listing is a Job.
  const landedCount = useRef(0);
  const uploads = useUploads(name, {
    onLanded: () => {
      landedCount.current += 1;
    },
    onOp: fileOps.ignore,
    beforeUpload: (path) => requireWorldConfirmation([path]),
    hold: fileOps.running || downloading !== null || unzipping !== null || startingTask || held !== null,
    free,
  });
  // Each change is a Job holding the world lock, so any other holder blocks it.
  const changing = uploads.busy || fileOps.running || downloading !== null || unzipping !== null || startingTask || held !== null;
  // Names the current lock holder; queued uploads come last.
  const waitTitle =
    fileOps.running || unzipping !== null
      ? t("wait_for_op")
      : downloading !== null
        ? t("wait_for_download")
        : held !== null
          ? t(holderText(held))
          : t("wait_for_uploads");
  useEffect(() => {
    if (uploads.busy || landedCount.current === 0) return;
    const count = landedCount.current;
    landedCount.current = 0;
    void load(dir);
    reloadWorlds();
    toast.success(t("upload_batch_done", { count }));
  }, [uploads.busy, dir, load, reloadWorlds, t, toast]);
  useUnsavedGuard(uploads.busy);
  // A file dropped outside the list would be opened by the browser, taking the queue with it; the list claims its own drops first.
  useEffect(() => {
    const refuse = (e: DragEvent) => {
      if (e.defaultPrevented || !e.dataTransfer?.types.includes("Files")) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "none";
    };
    window.addEventListener("dragover", refuse);
    window.addEventListener("drop", refuse);
    return () => {
      window.removeEventListener("dragover", refuse);
      window.removeEventListener("drop", refuse);
    };
  }, []);

  const [naming, setNaming] = useState<Naming | null>(null);
  const [deleting, setDeleting] = useState<ServerFileEntry | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);
  const [taskDialog, setTaskDialog] = useState<"zip" | "url" | null>(null);
  const [taskPath, setTaskPath] = useState("");
  const [sourceURL, setSourceURL] = useState("");
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  // dragenter and dragleave fire for every child crossed, so the overlay counts depth.
  const [dragDepth, setDragDepth] = useState(0);

  function ask(next: Naming) {
    setMsg(null);
    setNaming(next);
  }

  function problemText(p: NameProblem | null): string | null {
    return p === null ? null : t(p);
  }

  function addFiles(files: readonly File[], paths?: readonly string[]) {
    if (files.length === 0) return;
    if (needsWorldConfirmation(files.map((file, index) => joinPath(dir, paths?.[index] || file.webkitRelativePath || file.name)))) {
      setMsg({ kind: "error", text: t("live_world_confirm_required") });
      return;
    }
    setMsg(null);
    uploads.add(files, dir, entries, paths);
  }

  async function addDropped(data: DataTransfer) {
    try {
      const { files, paths } = await droppedFiles(data);
      addFiles(files, paths);
    } catch (e) {
      setMsg({ kind: "error", text: humanizeError(e) });
    }
  }

  const draggingFiles = (e: React.DragEvent) => Array.from(e.dataTransfer.types).includes("Files");

  async function createFile(n: string) {
    if (!naming) return;
    const p = joinPath(naming.dir, n);
    requireWorldConfirmation([p]);
    const r = await api.createServerFile(name, p, "");
    if (p === "server.properties") reloadWorlds();
    setConfirmDiscard(false);
    setOpen({ path: p, text: "", original: "", editable: true, sha256: r.sha256 ?? "", conflict: false, error: null });
    void load(naming.dir);
  }

  async function makeFolder(n: string) {
    if (!naming) return;
    const p = joinPath(naming.dir, n);
    requireWorldConfirmation([p]);
    await api.mkdirServerFolder(name, p);
    setMsg({ kind: "success", text: t("folder_created", { path: p }) });
    void load(naming.dir);
  }

  async function renameEntry(n: string) {
    if (naming?.kind !== "rename") return;
    requireWorldConfirmation([joinPath(naming.dir, naming.entry.name), joinPath(naming.dir, n)]);
    await api.renameServerFile(name, joinPath(naming.dir, naming.entry.name), joinPath(naming.dir, n));
    setMsg({ kind: "success", text: t("renamed", { from: naming.entry.name, to: n }) });
    void load(naming.dir);
  }

  async function deleteEntry() {
    if (!deleting) return;
    const p = joinPath(dir, deleting.name);
    requireWorldConfirmation([p]);
    await api.deleteServerFile(name, p);
    if (p === "server.properties") reloadWorlds();
    setMsg({ kind: "success", text: t("deleted", { path: p }) });
    void load(dir);
  }

  // unzip starts extracting the archive at p into its own folder. Without
  // overwrite the op stops before touching anything when a file would be
  // replaced, and names those files.
  async function unzip(p: string, overwrite: boolean) {
    requireWorldConfirmation([]);
    const { op } = await api.unzipServerFile(name, p, overwrite);
    startedHere.current.add(op.id);
    fileOps.started(op);
    if (op.state !== "running") opEnded(op);
  }

  // A refusal because the world is held means something this page has not seen
  // holds it: reading the Jobs again names it and holds the buttons.
  function heldElsewhere(e: unknown) {
    if ((e as { code?: string }).code === "maintenance_in_progress") worldJobs.refresh();
  }

  async function startUnzip(entry: ServerFileEntry) {
    const p = joinPath(dir, entry.name);
    setMsg(null);
    setUnzipping(p);
    try {
      await unzip(p, false);
    } catch (e) {
      heldElsewhere(e);
      setMsg({ kind: "error", text: humanizeError(e) });
    } finally {
      setUnzipping(null);
    }
  }

  async function overwriteConflicts() {
    if (!conflicts) return;
    const request = taskRequests.current.get(conflicts.id);
    if (request) await runTask(request, true);
    else await unzip(conflicts.path, true);
    fileOps.dismiss(conflicts.id);
  }

  async function runTask(request: FileTask, overwrite = false) {
    requireWorldConfirmation(taskTargets(request));
    setStartingTask(true);
    try {
      const { op } = request.kind === "transfer"
        ? await api.transferServerFiles(name, request.op, request.paths, request.to, overwrite)
        : request.kind === "zip"
          ? await api.zipServerFiles(name, request.paths, request.to, overwrite)
          : await api.fetchServerFile(name, request.url, request.path, overwrite);
      startedHere.current.add(op.id);
      taskRequests.current.set(op.id, request);
      fileOps.started(op);
      if (op.state !== "running") opEnded(op);
      setChecked(new Set());
    } finally {
      setStartingTask(false);
    }
  }

  function taskTargets(request: FileTask) {
    const targets = request.kind === "transfer"
      ? request.paths.map((p) => joinPath(request.to, p.split("/").pop()!))
      : [request.kind === "zip" ? request.to : request.path];
    return request.kind === "transfer" && request.op === "move" ? [...targets, ...request.paths] : targets;
  }

  function beginTask(request: FileTask) {
    setMsg(null);
    void runTask(request).catch((e) => {
      heldElsewhere(e);
      setMsg({ kind: "error", text: humanizeError(e) });
    });
  }

  async function submitTask() {
    const path = taskPath.trim();
    if (!path) throw new Error(t("name_required"));
    await runTask(taskDialog === "zip"
      ? { kind: "zip", paths: [...checked], to: path }
      : { kind: "url", url: sourceURL.trim(), path });
  }

  async function searchFiles() {
    if (!query.trim() || searching) return;
    const ticket = ++loadSeq.current;
    setSearching(true);
    setMsg(null);
    try {
      const result = await api.searchServerFiles(name, dir, query.trim());
      if (ticket !== loadSeq.current) return;
      setEntries(sortEntries(result.entries));
      setTruncated(result.truncated);
      setSearchActive(true);
      setChecked(new Set());
    } catch (e) {
      if (ticket === loadSeq.current) setMsg({ kind: "error", text: humanizeError(e) });
    } finally {
      if (ticket === loadSeq.current) setSearching(false);
    }
  }

  async function deleteChecked() {
    const paths = [...checked];
    requireWorldConfirmation(paths);
    setStartingTask(true);
    try {
      for (const path of paths) {
        requireWorldConfirmation([path]);
        await api.deleteServerFile(name, path);
        setChecked((all) => new Set([...all].filter((p) => p !== path)));
      }
      setMsg({ kind: "success", text: t("batch_deleted", { count: paths.length }) });
    } finally {
      setStartingTask(false);
      void load(dir);
      reloadWorlds();
    }
  }

  function copyPaths(op: "copy" | "move", paths: string[]) {
    setClipboard({ server: name, op, paths });
    setMsg({ kind: "success", text: t(op === "copy" ? "copied_selection" : "cut_selection", { count: paths.length }) });
  }

  function selectAll(value: boolean) {
    setChecked(new Set(value ? (entries ?? []).slice(0, 500).map((entry) => joinPath(dir, entry.name)) : []));
    if (value && (entries?.length ?? 0) > 500) setMsg({ kind: "error", text: t("selection_limit") });
  }

  function cancelLongPress() {
    if (longPress.current !== null) clearTimeout(longPress.current);
    longPress.current = null;
  }
  useEffect(() => () => { if (longPress.current !== null) clearTimeout(longPress.current); }, []);

  // Downloads redact RCON passwords and omit forwarding-secret configuration files.
  async function download(entry?: ServerFileEntry) {
    const p = entry ? joinPath(dir, entry.name) : "*";
    setMsg(null);
    setDownloading(p);
    try {
      const tk = entry ? await api.downloadServerFile(name, p, entry.is_dir) : await api.downloadServerFiles(name, [...checked]);
      const s = await awaitExport(tk.ticket, () => alive.current);
      if (s === null) return;
      if (s.state === "failed") {
        // A folder of more files than the zipping Job has memory to list gets it
        // killed, and felis-api names that OOMKilled; a single file streams through
        // in constant memory.
        const oom = s.message?.includes("OOMKilled");
        setMsg({
          kind: "error",
          text: oom
            ? t("download_out_of_memory")
            : s.message
              ? t("download_failed_because", { reason: s.message })
              : t("download_failed"),
        });
        return;
      }
      saveDownload(await api.exportDownloadURL(tk.ticket), tk.filename);
      // Its Job holds the world until the browser has all of it.
      worldJobs.refresh();
      const note =
        p === "server.properties"
          ? "download_started_props"
          : p === parentOf(SECRET_CONFIG_PATH)
            ? "download_started_config"
            : "download_started";
      setMsg({ kind: "success", text: t(note, { filename: tk.filename }) });
    } catch (e) {
      if (!alive.current) return;
      heldElsewhere(e);
      setMsg({
        kind: "error",
        text:
          (e as { code?: string }).code === "export_busy"
            ? t("download_busy")
            : t("download_failed_because", { reason: humanizeError(e) }),
      });
    } finally {
      if (alive.current) setDownloading(null);
    }
  }

  const back = <BackLink to={`/servers/${name}`} label={t("back_to_console")} />;
  if (statusQ.loading && !statusQ.data) {
    return (
      <>
        {back}
        <Loading />
      </>
    );
  }
  // Only a failed first load replaces the page: a later poll that fails keeps
  // the page (and an open editor) mounted and says so above the listing.
  if (statusQ.error && !statusQ.data) {
    return (
      <>
        {back}
        <ErrorState error={statusQ.error} onRetry={statusQ.reload} />
      </>
    );
  }
  if (!statusQ.data) return back;

  const now = Date.now();
  const segments = dir === "" ? [] : dir.split("/");
  const dirtyBytes = open ? new TextEncoder().encode(open.text).length : 0;
  const tooLarge = dirtyBytes > MAX_WRITE_BYTES;

  // A full disk is the thing to fix first, so only then does its card lead the page.
  const diskCard = (
    <Card><CardContent className="space-y-2 p-4">
      <DiskUsage disk={statusQ.error && statusQ.data.disk ? { ...statusQ.data.disk, stale: true } : statusQ.data.disk} diskFull={statusQ.data.diskFull} />
      {statusQ.data.diskFull && <p className="text-xs text-muted-foreground">{t("disk_cleanup_hint")}</p>}
    </CardContent></Card>
  );
  const header = (
    <PageHeader
      icon={FolderOpen}
      title={statusQ.data.displayName || statusQ.data.name}
      subtitle={t("title")}
      actions={<PhaseBadge phase={shownPhase(statusQ.data)} />}
      className="mb-6"
    />
  );

  return (
    <>
      {back}
      {header}
      {pending ? (
        <Loading />
      ) : mineQ.error ? (
        <ErrorState error={mineQ.error} onRetry={mineQ.reload} />
      ) : !owned ? (
        <NotYours title={t("not_yours_title")} body={t("not_yours_body")} />
      ) : (
        <div className="space-y-4">
          {statusQ.data.diskFull && diskCard}
          {live && <div role="status" className="space-y-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-4 text-sm">
            <p>{t("live_warning")}</p>
            {worldConfirmation("files-world-confirm")}
          </div>}
          {msg && <MessageLine kind={msg.kind} message={msg.text} />}
          {statusQ.error != null && (
            <MessageLine
              kind="error"
              message={t("status_refresh_failed", { reason: humanizeError(statusQ.error) })}
            />
          )}
          {!accessible ? (
            <Card>
              <CardContent className="flex flex-col items-center gap-3 py-14 text-center">
                <Square className="h-7 w-7 text-muted-foreground/70" />
                <div>
                  <p className="font-medium">{t("stopped_required_title")}</p>
                  <p className="mx-auto mt-1 max-w-md text-sm text-muted-foreground">
                    {t("stopped_required_body")}
                  </p>
                </div>
                <PowerButton
                  name={name}
                  phase={statusQ.data.phase}
                  desiredState={statusQ.data.desiredState}
                  failed={startFailure(statusQ.data) !== null}
                  playersOnline={statusQ.data.playersOnline}
                  playerCountUnknown={statusQ.data.playerCountUnknown}
                  retiring={statusQ.data.retiring}
                  diskFull={statusQ.data.diskFull}
                  stopOnly
                  onChanged={statusQ.reload}
                  className="items-center"
                />
              </CardContent>
            </Card>
          ) : listLoading && entries === null ? (
            <Loading />
          ) : listErr ? (
            <ErrorState error={listErr} onRetry={() => load(dir)} />
          ) : (
            <Card
              className="relative overflow-hidden"
              onDragEnter={(e) => {
                if (!draggingFiles(e)) return;
                e.preventDefault();
                setDragDepth((d) => d + 1);
              }}
              onDragOver={(e) => {
                if (!draggingFiles(e)) return;
                e.preventDefault();
                e.dataTransfer.dropEffect = "copy";
              }}
              onDragLeave={(e) => {
                if (!draggingFiles(e)) return;
                setDragDepth((d) => Math.max(0, d - 1));
              }}
              onDrop={(e) => {
                if (!draggingFiles(e)) return;
                e.preventDefault();
                setDragDepth(0);
                void addDropped(e.dataTransfer);
              }}
            >
              {dragDepth > 0 && (
                <div className="pointer-events-none absolute inset-0 z-10 flex flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed border-primary bg-background/85 text-center backdrop-blur-[1px]">
                  <Upload className="h-7 w-7 text-primary" />
                  <p className="px-4 text-sm font-medium">
                    {t("drop_here", { dir: dir === "" ? t("root") : dir })}
                  </p>
                </div>
              )}
              <CardContent className="p-0">
                {/* Location bar: parent button + clickable breadcrumbs + actions */}
                <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => void load(parentOf(dir), true)}
                    disabled={dir === "" || listLoading}
                  >
                    <ArrowUp className="h-4 w-4" />
                    {t("up")}
                  </Button>
                  <nav className="flex min-w-0 flex-wrap items-center gap-1 text-sm">
                    <button
                      type="button"
                      onClick={() => void load("", true)}
                      className={cn(
                        "rounded px-1.5 py-0.5 hover:bg-muted",
                        dir === "" ? "font-medium text-foreground" : "text-muted-foreground",
                      )}
                    >
                      {t("root")}
                    </button>
                    {segments.map((seg, i) => {
                      const target = segments.slice(0, i + 1).join("/");
                      const last = i === segments.length - 1;
                      return (
                        <span key={target} className="flex items-center gap-1">
                          <ChevronRight className="h-3.5 w-3.5 text-muted-foreground/60" />
                          <button
                            type="button"
                            onClick={() => void load(target, true)}
                            className={cn(
                              "rounded px-1.5 py-0.5 font-mono text-xs hover:bg-muted",
                              last ? "font-medium text-foreground" : "text-muted-foreground",
                            )}
                          >
                            {seg}
                          </button>
                        </span>
                      );
                    })}
                  </nav>
                  <div className="ml-auto flex max-w-full flex-wrap items-center justify-end gap-1">
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => ask({ kind: "file", dir })}
                      disabled={changing}
                      aria-label={t("new_file")}
                      title={changing ? waitTitle : t("new_file")}
                    >
                      <FilePlus className="h-4 w-4" />
                      <span className="hidden md:inline">{t("new_file")}</span>
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => ask({ kind: "folder", dir })}
                      disabled={changing}
                      aria-label={t("new_folder")}
                      title={changing ? waitTitle : t("new_folder")}
                    >
                      <FolderPlus className="h-4 w-4" />
                      <span className="hidden md:inline">{t("new_folder")}</span>
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => fileInput.current?.click()}
                      aria-label={t("upload")}
                      title={t("upload_hint")}
                    >
                      <Upload className="h-4 w-4" />
                      <span className="hidden sm:inline">{t("upload")}</span>
                    </Button>
                    <input
                      ref={fileInput}
                      type="file"
                      multiple
                      hidden
                      data-testid="upload-input"
                      onChange={(e) => {
                        addFiles(Array.from(e.target.files ?? []));
                        e.target.value = "";
                      }}
                    />
                    <Button size="sm" variant="ghost" onClick={() => folderInput.current?.click()} aria-label={t("upload_folder")}>
                      <FolderPlus className="h-4 w-4" /><span className="hidden md:inline">{t("upload_folder")}</span>
                    </Button>
                    <input ref={folderInput} type="file" multiple hidden data-testid="folder-upload-input"
                      {...{ webkitdirectory: "", directory: "" }}
                      onChange={(e) => { addFiles(Array.from(e.target.files ?? [])); e.target.value = ""; }} />
                    <Button size="sm" variant="ghost" disabled={changing} onClick={() => {
                      setTaskPath(joinPath(dir, "")); setSourceURL(""); setTaskDialog("url");
                    }}>{t("from_url")}</Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => {
                        void load(dir);
                        fileOps.refresh();
                        reloadWorlds();
                      }}
                      disabled={listLoading}
                      aria-label={t("refresh")}
                      title={t("refresh")}
                    >
                      <RefreshCw className={cn("h-4 w-4", listLoading && "animate-spin")} />
                    </Button>
                  </div>
                </div>

                <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
                  <form className="flex min-w-0 flex-1 gap-2" onSubmit={(e) => { e.preventDefault(); void searchFiles(); }}>
                    <Input aria-label={t("search_label")} placeholder={t("search_label")} value={query} maxLength={256}
                      onChange={(e) => setQuery(e.target.value)} className="h-8 min-w-24" />
                    <Button size="sm" variant="outline" disabled={!query.trim() || searching || listLoading} type="submit">
                      {searching ? <Loader2 className="h-4 w-4 animate-spin" /> : <Search className="h-4 w-4" />}{t("search")}
                    </Button>
                  </form>
                  {searchActive && <Button size="sm" variant="ghost" onClick={() => void load(dir)}>{t("clear_search")}</Button>}
                  {clipboard?.server === name && <Button size="sm" variant="outline" disabled={changing || listLoading}
                    onClick={() => beginTask({ kind: "transfer", op: clipboard.op, paths: clipboard.paths, to: dir })}>
                    {t("paste_count", { count: clipboard.paths.length })}
                  </Button>}
                </div>
                {checked.size > 0 && <div className="flex flex-wrap items-center gap-2 border-b border-border bg-primary/5 px-4 py-2">
                  <span className="mr-auto text-xs">{t("selected_count", { count: checked.size })}</span>
                  <Button size="sm" variant="ghost" disabled={changing || [...checked].some(isManaged)} onClick={() => copyPaths("copy", [...checked])}>{t("copy")}</Button>
                  <Button size="sm" variant="ghost" disabled={changing || [...checked].some(isManaged)} onClick={() => copyPaths("move", [...checked])}>{t("cut")}</Button>
                  <Button size="sm" variant="outline" disabled={changing || checked.has(SECRET_CONFIG_PATH)} onClick={() => void download()}>{t("download_selected")}</Button>
                  <Button size="sm" variant="outline" disabled={changing || checked.has(SECRET_CONFIG_PATH)} onClick={() => {
                    setTaskPath(joinPath(dir, "archive.zip")); setTaskDialog("zip");
                  }}>{t("zip_selected")}</Button>
                  <Button size="sm" variant="ghost" disabled={changing} onClick={() => setBatchDeleteOpen(true)} className="text-destructive">{t("delete_selected")}</Button>
                </div>}

                {listLoading && (
                  <div role="status" className="flex items-center gap-2 border-b border-border bg-primary/5 px-4 py-2 text-sm text-muted-foreground">
                    <Loader2 className="h-4 w-4 animate-spin text-primary" />
                    {t("loading_folder", { path: listTarget || t("root") })}
                  </div>
                )}

                {held !== null && (
                  <section role="status" className="border-b border-border bg-muted/20 px-4 py-2.5">
                    <p className="flex items-start gap-2.5 text-sm">
                      <Loader2 className="mt-0.5 h-4 w-4 shrink-0 animate-spin text-primary" />
                      <span>{t(holderText(held))}</span>
                    </p>
                  </section>
                )}

                <FileOps
                  ops={fileOps.ops}
                  error={fileOps.error}
                  onDismiss={fileOps.dismiss}
                  onConflicts={showConflicts}
                />

                <UploadQueue
                  items={uploads.items}
                  dir={dir}
                  onReplace={uploads.replace}
                  onRetry={uploads.retry}
                  onRemove={uploads.remove}
                  onReplaceAll={uploads.replaceAll}
                  onSkipAll={uploads.skipAll}
                  onClearDone={uploads.clearDone}
                />

                    {entries && entries.length === 0 ? (
                  <div className="p-4">
                    <EmptyState title={t(searchActive ? "search_empty" : "empty_dir_title")} hint={searchActive ? undefined : t("empty_dir_hint")} />
                  </div>
                ) : (
                  <div className="overflow-x-auto">
                    <table className="w-full border-collapse text-sm">
                      <thead>
                        <tr className="border-b border-border bg-muted/40 text-left text-[11px] uppercase tracking-wider text-muted-foreground">
                          <th className="w-10 px-3 py-2.5"><Checkbox aria-label={t("select_all")}
                            checked={!!entries?.length && entries.every((entry) => checked.has(joinPath(dir, entry.name)))}
                            onChange={(e) => selectAll(e.target.checked)} /></th>
                          <th className="px-4 py-2.5 font-medium">{t("col_name")}</th>
                          <th className="px-4 py-2.5 font-medium">{t("col_size")}</th>
                          <th className="hidden px-4 py-2.5 font-medium sm:table-cell">{t("col_modified")}</th>
                          <th className="w-px px-2 py-2.5">
                            <span className="sr-only">{t("col_actions")}</span>
                          </th>
                        </tr>
                      </thead>
                      <tbody className="divide-y divide-border">
                        {(entries ?? []).map((e) => {
                          const p = joinPath(dir, e.name);
                          const managed = isManaged(p);
                          return (
                            <tr
                              key={e.name}
                              onClick={() => {
                                if (longPressed.current) { longPressed.current = false; return; }
                                setSelected(p);
                                if (e.is_dir) void load(p, true);
                                else void openFile(e);
                              }}
                              onContextMenu={(ev) => { ev.preventDefault(); setMenuPath(p); }}
                              onTouchStart={() => {
                                cancelLongPress(); longPressed.current = false;
                                longPress.current = setTimeout(() => { longPressed.current = true; setMenuPath(p); }, 600);
                              }}
                              onTouchMove={cancelLongPress} onTouchEnd={cancelLongPress} onTouchCancel={cancelLongPress}
                              aria-selected={selected === p}
                              className={cn("cursor-pointer transition-colors hover:bg-muted/30", selected === p && "bg-primary/10 ring-1 ring-inset ring-primary/30")}
                            >
                              <td className="px-3 py-3" onClick={(ev) => ev.stopPropagation()}>
                                <Checkbox aria-label={t("select_item", { name: e.name })} checked={checked.has(p)} onChange={(ev) => {
                                  if (ev.target.checked && checked.size >= 500) { setMsg({ kind: "error", text: t("selection_limit") }); return; }
                                  setChecked((all) => { const next = new Set(all); if (ev.target.checked) next.add(p); else next.delete(p); return next; });
                                }} />
                              </td>
                              <td className="px-4 py-3">
                                {/* The name is a button so the keyboard and a screen
                                    reader reach every row; its click bubbles to the
                                    row, which opens it from anywhere on the row. */}
                                <button
                                  type="button"
                                  aria-label={t(e.is_dir ? "open_folder" : "open_file", { name: e.name })}
                                  className="flex min-w-0 max-w-full items-center gap-2 rounded text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                                >
                                  {e.is_dir ? (
                                    <Folder className="h-4 w-4 shrink-0 text-primary" />
                                  ) : (
                                    <FileText className="h-4 w-4 shrink-0 text-muted-foreground" />
                                  )}
                                  <span className="truncate font-mono text-xs">{e.name}</span>
                                  {(opening === p || (listLoading && listTarget === p)) && (
                                    <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-muted-foreground" />
                                  )}
                                </button>
                              </td>
                              <td className="px-4 py-3 whitespace-nowrap text-muted-foreground">
                                {e.is_dir ? "—" : formatBytes(e.size)}
                              </td>
                              <td
                                className="hidden px-4 py-3 whitespace-nowrap text-xs text-muted-foreground sm:table-cell"
                                title={e.mod_time}
                              >
                                {e.mod_time ? formatRelative(e.mod_time, now, locale) : "—"}
                              </td>
                              <td className="px-2 py-1.5">
                                {/* The actions stop at their own buttons: a click
                                    here must not also open the row. */}
                                <div className="flex items-center justify-end gap-0.5" onClick={(ev) => ev.stopPropagation()}>
                                  {!e.is_dir && isArchive(e.name) && (
                                    <Button
                                      size="sm"
                                      variant="ghost"
                                      className="h-8 w-8 p-0 text-muted-foreground hover:text-foreground"
                                      onClick={() => void startUnzip(e)}
                                      disabled={changing}
                                      aria-label={t("unzip_item", { name: e.name })}
                                      title={changing ? waitTitle : t("unzip_item", { name: e.name })}
                                    >
                                      {unzipping === p ? (
                                        <Loader2 className="h-3.5 w-3.5 animate-spin" />
                                      ) : (
                                        <FileArchive className="h-3.5 w-3.5" />
                                      )}
                                    </Button>
                                  )}
                                  <Button
                                    size="sm"
                                    variant="ghost"
                                    className="h-8 w-8 p-0 text-muted-foreground hover:text-foreground"
                                    onClick={() => void download(e)}
                                    disabled={p === SECRET_CONFIG_PATH || changing}
                                    aria-label={t(e.is_dir ? "download_folder_item" : "download_item", { name: e.name })}
                                    title={
                                      p === SECRET_CONFIG_PATH
                                        ? t("secret_config_no_download")
                                        : downloading === p
                                          ? t("download_preparing", { name: e.name })
                                          : changing
                                            ? waitTitle
                                            : t(e.is_dir ? "download_folder_item" : "download_item", { name: e.name })
                                    }
                                  >
                                    {downloading === p ? (
                                      <Loader2 className="h-3.5 w-3.5 animate-spin" />
                                    ) : (
                                      <Download className="h-3.5 w-3.5" />
                                    )}
                                  </Button>
                                  <Button
                                    size="sm"
                                    variant="ghost"
                                    className="h-8 w-8 p-0 text-muted-foreground hover:text-foreground"
                                    onClick={() => ask({ kind: "rename", dir: parentOf(p), entry: { ...e, name: p.split("/").pop()! } })}
                                    disabled={managed || changing}
                                    aria-label={t("rename_item", { name: e.name })}
                                    title={
                                      managed
                                        ? t("managed_no_rename")
                                        : changing
                                          ? waitTitle
                                          : t("rename_item", { name: e.name })
                                    }
                                  >
                                    <Pencil className="h-3.5 w-3.5" />
                                  </Button>
                                  <Button
                                    size="sm"
                                    variant="ghost"
                                    className="h-8 w-8 p-0 text-muted-foreground hover:bg-destructive/10 hover:text-destructive"
                                    onClick={() => {
                                      setMsg(null);
                                      setDeleting(e);
                                      setDeleteOpen(true);
                                    }}
                                    disabled={changing}
                                    aria-label={t("delete_item", { name: e.name })}
                                    title={changing ? waitTitle : t("delete_item", { name: e.name })}
                                  >
                                    <Trash2 className="h-3.5 w-3.5" />
                                  </Button>
                                  <FileMenu name={e.name} open={menuPath === p} onOpenChange={(v) => setMenuPath(v ? p : null)} actions={[
                                    { label: t("copy"), disabled: changing || managed, run: () => copyPaths("copy", [p]) },
                                    { label: t("cut"), disabled: changing || managed, run: () => copyPaths("move", [p]) },
                                    { label: t("download_file"), disabled: changing || p === SECRET_CONFIG_PATH, run: () => void download(e) },
                                    { label: t("rename"), disabled: changing || managed, run: () => ask({ kind: "rename", dir: parentOf(p), entry: { ...e, name: p.split("/").pop()! } }) },
                                    { label: t("delete"), disabled: changing, run: () => { setDeleting(e); setDeleteOpen(true); } },
                                  ]} />
                                </div>
                              </td>
                            </tr>
                          );
                        })}
                      </tbody>
                    </table>
                  </div>
                )}
                {truncated && (
                  <p className="border-t border-border px-4 py-2 text-xs text-amber-500">
                    {t(searchActive ? "search_truncated" : "list_truncated")}
                  </p>
                )}
              </CardContent>
            </Card>
          )}
          {!statusQ.data.diskFull && diskCard}
        </div>
      )}

      <Dialog open={image !== null} onOpenChange={(v) => { if (!v) setImage(null); }}>
        <DialogContent className="max-w-4xl">
          <DialogHeader><DialogTitle className="break-all font-mono text-sm">{image?.path}</DialogTitle></DialogHeader>
          {image && <img src={image.url} alt={t("image_preview", { path: image.path })} className="mx-auto max-h-[70vh] max-w-full object-contain" />}
        </DialogContent>
      </Dialog>

      <ConfirmDialog open={taskDialog !== null} onOpenChange={(v) => { if (!v) setTaskDialog(null); }}
        title={t(taskDialog === "zip" ? "zip_selected" : "from_url")} confirmLabel={t(taskDialog === "zip" ? "zip_create" : "from_url_start")}
        confirmVariant="default" onConfirm={submitTask} confirmDisabled={!taskPath.trim() || (taskDialog === "url" && !sourceURL.trim()) || needsWorldConfirmation([taskPath.trim()])}>
        {taskDialog === "url" && <label className="grid gap-2 text-sm">{t("url_label")}
          <Input type="url" value={sourceURL} maxLength={8192} onChange={(e) => setSourceURL(e.target.value)} placeholder="https://example.com/file.jar" />
        </label>}
        {taskDialog === "url" && <p className="text-xs text-muted-foreground">{t("url_hint")}</p>}
        <label className="grid gap-2 text-sm">{t("target_path")}
          <Input value={taskPath} onChange={(e) => setTaskPath(e.target.value)} />
        </label>
        <p className="text-xs text-muted-foreground">{t("target_path_hint")}</p>
        {live && worldConfirmation("task-world-confirm")}
      </ConfirmDialog>

      <ConfirmDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen} title={t("batch_delete_title", { count: checked.size })}
        description={t("batch_delete_body")} confirmLabel={t("delete")} onConfirm={deleteChecked}
        confirmDisabled={checked.size === 0 || needsWorldConfirmation([...checked])}>
        <ul className="max-h-48 overflow-auto font-mono text-xs">{[...checked].map((path) => <li key={path}>{path}</li>)}</ul>
        {live && worldConfirmation("batch-delete-world-confirm")}
      </ConfirmDialog>

      {/* File editor dialog */}
      <Dialog open={open !== null || opening !== null} onOpenChange={(v) => !v && requestClose()}>
        <DialogContent className="max-w-4xl">
          <DialogHeader>
            <DialogTitle className="break-all font-mono text-sm">
              {opening ?? open?.path}
            </DialogTitle>
          </DialogHeader>
          {opening !== null && (
            <div role="status" className="flex h-[30vh] items-center justify-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="h-5 w-5 animate-spin text-primary" />{t("opening_file")}
            </div>
          )}
          {open && (
            <>
              {live && open.editable && (!worldDirs || touchesWorld(open.path, worldDirs)) && worldConfirmation("editor-world-confirm")}
              {open.conflict && (
                <div
                  role="alert"
                  className="grid gap-3 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs"
                >
                  <div className="flex items-start gap-2 text-amber-700 dark:text-amber-300">
                    <AlertTriangle className="mt-px h-4 w-4 shrink-0" />
                    <div>
                      <p className="text-sm font-medium">{t("conflict_title")}</p>
                      <p className="mt-0.5 text-foreground/80">{t("conflict_body")}</p>
                    </div>
                  </div>
                  <div className="flex flex-wrap gap-2 pl-6">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => void reloadOpen()}
                      disabled={saving || reloading}
                      title={t("conflict_reload_hint")}
                    >
                      {reloading ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                      ) : (
                        <RefreshCw className="h-4 w-4" />
                      )}
                      {t("conflict_reload")}
                    </Button>
                    <Button
                      size="sm"
                      variant="destructive"
                      onClick={() => void handleSave(true)}
                      disabled={saving || reloading || tooLarge || invalidConfig || needsWorldConfirmation([open.path])}
                      title={t("conflict_overwrite_hint")}
                    >
                      {saving ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                      ) : (
                        <Save className="h-4 w-4" />
                      )}
                      {t("conflict_overwrite")}
                    </Button>
                  </div>
                </div>
              )}
              {open.error && <MessageLine kind="error" message={open.error} />}
              {open.editable && open.path === "server.properties" ? (
                <ServerPropertiesEditor key={open.path} server={name} value={open.text} original={open.original}
                  onChange={(text) => setOpen({ ...open, text })} onSave={() => void handleSave()}
                  onValidityChange={setConfigValid} disabled={saving || reloading} />
              ) : open.editable && YAML_FIELDS[open.path] ? (
                <YamlConfigEditor key={open.path} path={open.path} value={open.text} original={open.original}
                  onChange={(text) => setOpen({ ...open, text })} onSave={() => void handleSave()}
                  onValidityChange={setConfigValid} disabled={saving || reloading} />
              ) : open.editable ? (
                <Suspense fallback={<Loading />}>
                  <TextFileEditor path={open.path} value={open.text} label={t("file_content")}
                    onChange={(text) => setOpen({ ...open, text })} onSave={() => void handleSave()} />
                </Suspense>
              ) : (
                <div className="flex min-h-52 flex-col items-center justify-center gap-3 rounded-md border border-border bg-muted/20 px-6 py-8 text-center">
                  <FileArchive className="h-9 w-9 text-muted-foreground" />
                  <p className="font-medium">{t(open.preview === "large" ? "preview_large_title" : "binary_title")}</p>
                  <p className="max-w-md text-sm text-muted-foreground">{t(open.preview === "large" ? "preview_large_hint" : "binary_hint", { limit: formatBytes(MAX_READ_BYTES) })}</p>
                  <Button variant="outline" disabled={changing}
                    onClick={() => void download({ name: open.path.slice(dir.length ? dir.length + 1 : 0), is_dir: false, size: open.size ?? 0, mod_time: "" })}>
                    {downloading === open.path ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
                    {t("download_file")}
                  </Button>
                </div>
              )}
              {!open.editable && msg && <MessageLine kind={msg.kind} message={msg.text} />}
              <DialogFooter className="items-center gap-2 sm:justify-between">
                <span className={cn("text-xs", tooLarge ? "text-destructive" : "text-muted-foreground")}>
                  {tooLarge
                    ? t("too_large", { limit: formatBytes(MAX_WRITE_BYTES) })
                    : `${formatBytes(open.editable ? dirtyBytes : open.size ?? 0)}${open.editable ? " · UTF-8 · Ctrl/⌘ S" : ""}`}
                </span>
                {confirmDiscard ? (
                  <div role="alert" className="flex flex-wrap items-center justify-end gap-2">
                    <span className="text-sm font-medium">{t("discard_prompt")}</span>
                    <InlineConfirm
                      open
                      confirming={false}
                      onConfirm={discardEdits}
                      onCancel={() => setConfirmDiscard(false)}
                      confirmLabel={t("discard")}
                      cancelLabel={t("keep_editing")}
                      size="default"
                      className="flex items-center gap-2"
                    />
                  </div>
                ) : (
                  <div className="flex items-center gap-2">
                    <Button
                      variant="outline"
                      onClick={requestClose}
                      disabled={saving || reloading}
                    >
                      {t("common:cancel")}
                    </Button>
                    {open.editable && <Button
                      onClick={() => void handleSave()}
                      disabled={saving || reloading || open.conflict || !open.editable || !dirty || tooLarge || invalidConfig || needsWorldConfirmation([open.path])}
                    >
                      {saving ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                      ) : (
                        <Save className="h-4 w-4" />
                      )}
                      {saving ? t("saving") : t("save")}
                    </Button>}
                  </div>
                )}
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      {paperGlobalOpen && <PaperGlobalConfigDialog key={name} name={name}
        onClose={() => setPaperGlobalOpen(false)}
        onSaved={() => {
          setPaperGlobalOpen(false);
          setMsg({ kind: "success", text: t("saved", { path: SECRET_CONFIG_PATH }) });
          void load(dir);
        }}
        disabled={pending || !owned || !accessible || !!statusQ.error || changing}
        needsWorldConfirmation={needsWorldConfirmation([SECRET_CONFIG_PATH])}
        beforeSave={() => requireWorldConfirmation([SECRET_CONFIG_PATH])}
        worldConfirmation={live && (!worldDirs || touchesWorld(SECRET_CONFIG_PATH, worldDirs)) && worldConfirmation("paper-global-world-confirm")} />}

      {naming && (
        <NameDialog
          open
          onOpenChange={(v) => !v && setNaming(null)}
          title={
            naming.kind === "rename"
              ? t("rename_title", { name: naming.entry.name })
              : t(naming.kind === "file" ? "new_file" : "new_folder")
          }
          description={t("in_folder", { dir: naming.dir === "" ? t("root") : naming.dir })}
          label={t("name_label")}
          confirmLabel={t(naming.kind === "rename" ? "rename" : "create")}
          initial={naming.kind === "rename" ? naming.entry.name : ""}
          problem={(v) =>
            problemText(nameProblem(v, entries, naming.kind === "rename" ? naming.entry.name : undefined))
          }
          onSubmit={naming.kind === "file" ? createFile : naming.kind === "folder" ? makeFolder : renameEntry}
        >
          {live && worldConfirmation("name-world-confirm")}
        </NameDialog>
      )}

      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={t(deleting?.is_dir ? "delete_folder_title" : "delete_file_title", { name: deleting?.name ?? "" })}
        description={t(deleting?.is_dir ? "delete_folder_body" : "delete_file_body")}
        confirmLabel={t("delete")}
        onConfirm={deleteEntry}
        confirmDisabled={deleting !== null && needsWorldConfirmation([joinPath(dir, deleting.name)])}
      >
        {live && deleting && (!worldDirs || touchesWorld(joinPath(dir, deleting.name), worldDirs)) && worldConfirmation("delete-world-confirm")}
      </ConfirmDialog>

      <ConfirmDialog
        open={conflictsOpen}
        onOpenChange={setConflictsOpen}
        title={t(conflicts?.op === "unzip" ? "unzip_conflicts_title" : "task_conflicts_title", {
          name: conflicts?.path.split("/").pop() ?? "",
          count: conflictCount(conflicts),
        })}
        description={t(conflicts?.op === "unzip" ? "unzip_conflicts_body" : "task_conflicts_body")}
        confirmLabel={t(conflicts?.op === "unzip" ? "unzip_overwrite" : "task_overwrite")}
        onConfirm={overwriteConflicts}
        confirmDisabled={needsWorldConfirmation(conflicts && taskRequests.current.has(conflicts.id) ? taskTargets(taskRequests.current.get(conflicts.id)!) : []) || (conflicts?.op !== "unzip" && !!conflicts && !taskRequests.current.has(conflicts.id))}
      >
        {live && worldConfirmation("unzip-world-confirm")}
        {conflicts && conflicts.op !== "unzip" && !taskRequests.current.has(conflicts.id) && <p className="text-sm text-muted-foreground">{t("task_retry_unavailable")}</p>}
        <ConflictList op={conflicts} />
      </ConfirmDialog>
    </>
  );
}

function conflictCount(op: FileOp | null): number {
  return op?.error?.conflict_count ?? op?.error?.conflicts?.length ?? 0;
}

// ConflictList names the files an extraction would replace. The op carries the
// first ones and the full count; the rest are counted.
function ConflictList({ op }: { op: FileOp | null }) {
  const { t } = useTranslation("files");
  const listed = op?.error?.conflicts ?? [];
  const more = conflictCount(op) - listed.length;
  if (listed.length === 0) return null;
  return (
    <div className="grid gap-2">
      <ul className="max-h-48 overflow-y-auto rounded-md border border-border bg-muted/30 px-3 py-2 font-mono text-xs">
        {listed.map((path) => (
          <li key={path} className="truncate py-0.5" title={path}>
            {path}
          </li>
        ))}
      </ul>
      {more > 0 && <p className="text-xs text-muted-foreground">{t("unzip_conflicts_more", { count: more })}</p>}
    </div>
  );
}
