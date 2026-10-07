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
import { MessageLine } from "@/components/MessageLine";
import { InlineConfirm } from "@/components/InlineConfirm";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { NameDialog } from "@/components/files/NameDialog";
import { FileOps } from "@/components/files/FileOps";
import { UploadQueue } from "@/components/files/UploadQueue";
import { useFileOps } from "@/components/files/useFileOps";
import { useUploads } from "@/components/files/useUploads";
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

/** The write ceiling, mirrored from fileedit.MaxWriteBytes (server truth). Reads
 *  are capped at 1 MiB server-side; a larger file is refused there with 413, so
 *  this only gates the save button to keep the common case honest up front. */
const MAX_WRITE_BYTES = 256 * 1024;
const MAX_READ_BYTES = 1024 * 1024;
const TextFileEditor = lazy(() => import("@/components/files/TextFileEditor").then((m) => ({ default: m.TextFileEditor })));
const BINARY_EXTENSION = /\.(jar|zip|gz|tar|7z|rar|dat|mca|mcr|schem|schematic|nbt|db|sqlite|png|jpe?g|gif|webp|ico|ogg|mp[34]|pdf|class|exe|dll|so|bin)$/i;

/** The []byte wire codec: Go's encoding/json renders []byte as base64, so the
 *  editor must speak it explicitly in both directions. Chunked so a ~256 KiB
 *  file never trips the argument-length ceiling of String.fromCharCode. */
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

/** decodeText returns the file's text, or null when the bytes are not valid
 *  UTF-8 or contain NUL. Such files open read-only: a textarea round-trip would
 *  silently corrupt them, and this editor exists to REPAIR configs, never to
 *  damage data it does not understand. */
function decodeText(bytes: Uint8Array): string | null {
  try {
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return text.includes("\u0000") ? null : text;
  } catch {
    return null;
  }
}

/** isZip says whether a file is one the page offers to extract: only .zip is,
 *  since extraction reads the zip format alone. */
function isZip(name: string): boolean {
  return name.toLowerCase().endsWith(".zip");
}

/** The name questions the page asks, each tied to the folder it was asked in. */
type Naming =
  | { kind: "file" | "folder"; dir: string }
  | { kind: "rename"; dir: string; entry: ServerFileEntry };

/** ServerFiles is the world-volume file manager: browse, edit a config (the "one
 *  wrong line in server.properties" repair), make, rename, delete and upload.
 *  Every call is owner-or-admin gated and refused with 409 not_stopped unless the
 *  server is fully stopped (the world volume is RWO), so the page gates up front
 *  instead of letting each call fail. */
export function ServerFiles() {
  const { name = "" } = useParams();
  const [params] = useSearchParams();
  const { t, i18n } = useTranslation("files");
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

  const [dir, setDir] = useState(params.get("path") ?? "");
  const [entries, setEntries] = useState<ServerFileEntry[] | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [listErr, setListErr] = useState<unknown>(null);
  const [listLoading, setListLoading] = useState(false);
  const [listTarget, setListTarget] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const folders = useRef(new Map<string, { at: number; listing: Awaited<ReturnType<typeof api.listServerFiles>> }>());
  // Bytes free on the world volume by the latest listing; null while unknown
  // (the listing says null when the Job could not tell; 0 is a full volume).
  const [free, setFree] = useState<number | null>(null);
  const [msg, setMsg] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  // Every load takes a ticket and only the newest one lands. The rows and the
  // breadcrumbs stay clickable while a folder loads, so a slow answer for a
  // folder already left behind would otherwise replace the one clicked after it.
  const loadSeq = useRef(0);
  const load = useCallback(
    async (p: string, cached = false) => {
      const ticket = ++loadSeq.current;
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

  // Load (and reload after a stop) only once the viewer is resolved as owner and
  // the server is fully stopped — both are hard server-side gates of every call.
  // `dir` stays out of the list: load() itself moves it, and a navigation that
  // re-ran this effect would fetch the same folder twice.
  useEffect(() => {
    if (owned && stopped) void load(dir);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [owned, stopped, load]);
  useEffect(() => {
    if (!stopped) folders.current.clear();
  }, [stopped]);

  // While the server is not stopped, poll the phase so the first successful
  // stop flips the page from the notice to the listing without a manual reload.
  usePolling(statusQ.reload, stopped ? null : STATUS_POLL_FAST_MS);

  // Editor state. `editable` false marks a binary file (rendered read-only).
  // `sha256` is the hash the read returned: every save sends it back, so a file
  // someone changed in the meantime is refused (409 file_changed) and `conflict`
  // turns on instead of their edit being silently overwritten. `error` is a save
  // failure, shown inside the dialog where the person is looking.
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
  const openingSeq = useRef(0);
  useEffect(() => () => { ++openingSeq.current; ++loadSeq.current; }, []);
  const [saving, setSaving] = useState(false);
  const [reloading, setReloading] = useState(false);
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
    // The read path refuses this one outright; saying why beats a Job that
    // answers "invalid path".
    if (p === SECRET_CONFIG_PATH) {
      setMsg({ kind: "error", text: t("secret_config_unreadable") });
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
    if (!open?.editable || saving || (!overwrite && open.conflict) || !dirty || tooLarge) return;
    setSaving(true);
    setMsg(null);
    setOpen({ ...open, error: null });
    try {
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

  // A background op (an extraction, or a file landing from parts) holds the
  // world until it ends, and goes on with the page closed. One that succeeded
  // changed the folder, so the listing is reread; an extraction started here
  // that stopped at files it would replace asks about them at once.
  const startedHere = useRef(new Set<string>());
  const [conflicts, setConflicts] = useState<FileOp | null>(null);
  const [conflictsOpen, setConflictsOpen] = useState(false);
  function showConflicts(op: FileOp) {
    setConflicts(op);
    setConflictsOpen(true);
  }
  function opEnded(op: FileOp) {
    if (op.state === "succeeded") void load(dir);
    else if (op.op === "unzip" && op.error?.code === "file_exists" && startedHere.current.has(op.id)) {
      showConflicts(op);
    }
  }
  const fileOps = useFileOps(name, owned && stopped, opEnded);
  // Backups, restores, world exports and downloads hold the world as well,
  // whichever tab or person started them. A restore replaces the files listed.
  const worldJobs = useWorldJobs(name, owned && stopped, () => void load(dir));
  const held = worldJobs.holder;
  useEffect(() => {
    if (held !== null) folders.current.clear();
  }, [held]);

  // A download holds the world while felis-api gets it ready, and a change
  // sent meanwhile could only be refused.
  const [downloading, setDownloading] = useState<string | null>(null);
  const [unzipping, setUnzipping] = useState<string | null>(null);
  const alive = useRef(true);
  useEffect(
    () => () => {
      alive.current = false;
    },
    [],
  );

  // Uploads run one at a time. The listing is reread once the queue has drained
  // rather than after each file: every listing is a Job of its own.
  const landedSince = useRef(false);
  const uploads = useUploads(name, {
    onLanded: () => {
      landedSince.current = true;
    },
    onOp: fileOps.ignore,
    hold: fileOps.running || downloading !== null || unzipping !== null || held !== null,
    free,
  });
  // Each change is a Job holding the world lock, so while anything else holds
  // it a change could only be refused.
  const changing = uploads.busy || fileOps.running || downloading !== null || unzipping !== null || held !== null;
  // Names what holds the lock now: queued uploads wait on the rest too, so
  // those come first.
  const waitTitle =
    fileOps.running || unzipping !== null
      ? t("wait_for_op")
      : downloading !== null
        ? t("wait_for_download")
        : held !== null
          ? t(holderText(held))
          : t("wait_for_uploads");
  useEffect(() => {
    if (uploads.busy || !landedSince.current) return;
    landedSince.current = false;
    void load(dir);
  }, [uploads.busy, dir, load]);
  // A browser leaving the page takes the uploads with it.
  useUnsavedGuard(uploads.busy);
  // A file dropped anywhere else on the page would be opened by the browser in
  // place of the panel, taking the queue with it; outside the list a drop does
  // nothing. The list itself claims its drops first.
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
  // The entry outlives the dialog closing, so its title holds through the
  // closing animation.
  const [deleting, setDeleting] = useState<ServerFileEntry | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  // dragenter and dragleave fire for every child crossed, so the overlay counts
  // them rather than flickering at each row.
  const [dragDepth, setDragDepth] = useState(0);

  // The name dialog mounts per question, so each one starts from its own name.
  function ask(next: Naming) {
    setMsg(null);
    setNaming(next);
  }

  function problemText(p: NameProblem | null): string | null {
    return p === null ? null : t(p);
  }

  function addFiles(files: readonly File[]) {
    if (files.length === 0) return;
    setMsg(null);
    uploads.add(files, dir, entries);
  }

  // addDropped queues what was dropped. A folder arrives as an entry the browser
  // cannot read as a file, so it is named as skipped instead of failing later.
  function addDropped(data: DataTransfer) {
    const files: File[] = [];
    let folders = 0;
    for (const item of Array.from(data.items ?? [])) {
      if (item.kind !== "file") continue;
      if (item.webkitGetAsEntry?.()?.isDirectory) {
        folders++;
        continue;
      }
      const f = item.getAsFile();
      if (f) files.push(f);
    }
    if (data.items === undefined || data.items.length === 0) files.push(...Array.from(data.files));
    addFiles(files);
    if (folders > 0) setMsg({ kind: "error", text: t("upload_no_folders", { count: folders }) });
  }

  const draggingFiles = (e: React.DragEvent) => Array.from(e.dataTransfer.types).includes("Files");

  async function createFile(n: string) {
    if (!naming) return;
    const p = joinPath(naming.dir, n);
    const r = await api.createServerFile(name, p, "");
    setConfirmDiscard(false);
    setOpen({ path: p, text: "", original: "", editable: true, sha256: r.sha256 ?? "", conflict: false, error: null });
    void load(naming.dir);
  }

  async function makeFolder(n: string) {
    if (!naming) return;
    const p = joinPath(naming.dir, n);
    await api.mkdirServerFolder(name, p);
    setMsg({ kind: "success", text: t("folder_created", { path: p }) });
    void load(naming.dir);
  }

  async function renameEntry(n: string) {
    if (naming?.kind !== "rename") return;
    await api.renameServerFile(name, joinPath(naming.dir, naming.entry.name), joinPath(naming.dir, n));
    setMsg({ kind: "success", text: t("renamed", { from: naming.entry.name, to: n }) });
    void load(naming.dir);
  }

  async function deleteEntry() {
    if (!deleting) return;
    const p = joinPath(dir, deleting.name);
    await api.deleteServerFile(name, p);
    setMsg({ kind: "success", text: t("deleted", { path: p }) });
    void load(dir);
  }

  // unzip starts extracting the archive at p into its own folder. Without
  // overwrite the op stops before touching anything when a file would be
  // replaced, and names those files.
  async function unzip(p: string, overwrite: boolean) {
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
    await unzip(conflicts.path, true);
    fileOps.dismiss(conflicts.id);
  }

  // download has felis-api get the file, or the folder as a .zip, ready and
  // hands it to the browser, as a backup download does. The two secrets never
  // leave: server.properties comes with rcon.password redacted, and the
  // folder holding paper-global.yml comes without it.
  async function download(entry: ServerFileEntry) {
    const p = joinPath(dir, entry.name);
    setMsg(null);
    setDownloading(p);
    try {
      const tk = await api.downloadServerFile(name, p, entry.is_dir);
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
          {msg && <MessageLine kind={msg.kind} message={msg.text} />}
          {statusQ.error != null && (
            <MessageLine
              kind="error"
              message={t("status_refresh_failed", { reason: humanizeError(statusQ.error) })}
            />
          )}
          {!stopped ? (
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
                addDropped(e.dataTransfer);
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
                  <div className="ml-auto flex items-center gap-1">
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
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => {
                        void load(dir);
                        fileOps.refresh();
                      }}
                      disabled={listLoading}
                      aria-label={t("refresh")}
                      title={t("refresh")}
                    >
                      <RefreshCw className={cn("h-4 w-4", listLoading && "animate-spin")} />
                    </Button>
                  </div>
                </div>

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
                    <EmptyState title={t("empty_dir_title")} hint={t("empty_dir_hint")} />
                  </div>
                ) : (
                  <div className="overflow-x-auto">
                    <table className="w-full border-collapse text-sm">
                      <thead>
                        <tr className="border-b border-border bg-muted/40 text-left text-[11px] uppercase tracking-wider text-muted-foreground">
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
                                setSelected(p);
                                if (e.is_dir) void load(p, true);
                                else void openFile(e);
                              }}
                              aria-selected={selected === p}
                              className={cn("cursor-pointer transition-colors hover:bg-muted/30", selected === p && "bg-primary/10 ring-1 ring-inset ring-primary/30")}
                            >
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
                                  {!e.is_dir && isZip(e.name) && (
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
                                    onClick={() => ask({ kind: "rename", dir, entry: e })}
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
                    {t("list_truncated")}
                  </p>
                )}
              </CardContent>
            </Card>
          )}
        </div>
      )}

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
                      disabled={saving || reloading || tooLarge}
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
              {open.editable ? (
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
                      disabled={saving || reloading || open.conflict || !open.editable || !dirty || tooLarge}
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
        />
      )}

      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={t(deleting?.is_dir ? "delete_folder_title" : "delete_file_title", { name: deleting?.name ?? "" })}
        description={t(deleting?.is_dir ? "delete_folder_body" : "delete_file_body")}
        confirmLabel={t("delete")}
        onConfirm={deleteEntry}
      />

      <ConfirmDialog
        open={conflictsOpen}
        onOpenChange={setConflictsOpen}
        title={t("unzip_conflicts_title", {
          name: conflicts?.path.split("/").pop() ?? "",
          count: conflictCount(conflicts),
        })}
        description={t("unzip_conflicts_body")}
        confirmLabel={t("unzip_overwrite")}
        onConfirm={overwriteConflicts}
      >
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
