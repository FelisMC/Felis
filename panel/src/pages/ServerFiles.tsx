import { useCallback, useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import {
  AlertTriangle,
  ArrowUp,
  ChevronRight,
  FileText,
  Folder,
  FolderOpen,
  Loader2,
  RefreshCw,
  Save,
  Square,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { BackLink } from "@/components/BackLink";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { MessageLine } from "@/components/MessageLine";
import { InlineConfirm } from "@/components/InlineConfirm";
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
import { formatBytes, formatRelative } from "@/lib/format";
import type { ServerFileEntry } from "@/lib/types";
import { cn } from "@/lib/utils";

/** The write ceiling, mirrored from fileedit.MaxWriteBytes (server truth). Reads
 *  are capped at 1 MiB server-side; a larger file is refused there with 413, so
 *  this only gates the save button to keep the common case honest up front. */
const MAX_WRITE_BYTES = 256 * 1024;

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

function joinPath(dir: string, name: string): string {
  return dir === "" ? name : `${dir}/${name}`;
}

function parentOf(dir: string): string {
  const i = dir.lastIndexOf("/");
  return i === -1 ? "" : dir.slice(0, i);
}

/** ServerFiles is the world-volume file editor (the "one wrong line in
 *  server.properties" repair). Every call is owner-or-admin gated and refused
 *  with 409 not_stopped unless the server is fully stopped (the world volume is
 *  RWO), so the page gates up front instead of letting each call fail. */
export function ServerFiles() {
  const { name = "" } = useParams();
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

  const [dir, setDir] = useState("");
  const [entries, setEntries] = useState<ServerFileEntry[] | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [listErr, setListErr] = useState<unknown>(null);
  const [listLoading, setListLoading] = useState(false);
  const [msg, setMsg] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  const load = useCallback(
    async (p: string) => {
      setListLoading(true);
      setListErr(null);
      try {
        const r = await api.listServerFiles(name, p);
        setEntries(r.entries ?? []);
        setTruncated(r.truncated === true);
        setDir(p);
      } catch (e) {
        setEntries(null);
        setListErr(e);
      } finally {
        setListLoading(false);
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
  } | null>(null);
  const [opening, setOpening] = useState<string | null>(null);
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
    setOpen(null);
  }

  function discardEdits() {
    setConfirmDiscard(false);
    setOpen(null);
  }

  async function readInto(p: string) {
    const r = await api.readServerFile(name, p);
    const text = decodeText(base64ToBytes(r.content ?? ""));
    setConfirmDiscard(false);
    setOpen({
      path: p,
      text: text ?? "",
      original: text ?? "",
      editable: text !== null,
      sha256: r.sha256 ?? "",
      conflict: false,
      error: null,
    });
  }

  async function openFile(entry: ServerFileEntry) {
    const p = joinPath(dir, entry.name);
    setOpening(p);
    setMsg(null);
    try {
      await readInto(p);
    } catch (e) {
      setMsg({ kind: "error", text: humanizeError(e) });
    } finally {
      setOpening(null);
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
    if (!open || saving) return;
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
            <Card className="overflow-hidden">
              <CardContent className="p-0">
                {/* Location bar: parent button + clickable breadcrumbs + refresh */}
                <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => void load(parentOf(dir))}
                    disabled={dir === "" || listLoading}
                  >
                    <ArrowUp className="h-4 w-4" />
                    {t("up")}
                  </Button>
                  <nav className="flex flex-wrap items-center gap-1 text-sm">
                    <button
                      type="button"
                      onClick={() => void load("")}
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
                            onClick={() => void load(target)}
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
                  <div className="ml-auto">
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => void load(dir)}
                      disabled={listLoading}
                      aria-label={t("refresh")}
                      title={t("refresh")}
                    >
                      <RefreshCw className={cn("h-4 w-4", listLoading && "animate-spin")} />
                    </Button>
                  </div>
                </div>

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
                          <th className="px-4 py-2.5 font-medium">{t("col_modified")}</th>
                        </tr>
                      </thead>
                      <tbody className="divide-y divide-border">
                        {(entries ?? []).map((e) => (
                          <tr
                            key={e.name}
                            onClick={() => (e.is_dir ? void load(joinPath(dir, e.name)) : void openFile(e))}
                            className="cursor-pointer transition-colors hover:bg-muted/30"
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
                                {opening === joinPath(dir, e.name) && (
                                  <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-muted-foreground" />
                                )}
                              </button>
                            </td>
                            <td className="px-4 py-3 whitespace-nowrap text-muted-foreground">
                              {e.is_dir ? "—" : formatBytes(e.size)}
                            </td>
                            <td
                              className="px-4 py-3 whitespace-nowrap text-xs text-muted-foreground"
                              title={e.mod_time}
                            >
                              {e.mod_time ? formatRelative(e.mod_time, now, locale) : "—"}
                            </td>
                          </tr>
                        ))}
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
      <Dialog open={open !== null} onOpenChange={(v) => !v && requestClose()}>
        <DialogContent className="max-w-3xl">
          <DialogHeader>
            <DialogTitle className="break-all font-mono text-sm">
              {open?.path}
            </DialogTitle>
            {open && !open.editable && (
              <p className="text-xs text-muted-foreground">{t("binary_hint")}</p>
            )}
          </DialogHeader>
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
              <textarea
                value={open.text}
                onChange={(e) => setOpen({ ...open, text: e.target.value })}
                readOnly={!open.editable}
                spellCheck={false}
                className="h-[50vh] w-full resize-none rounded-md border border-input bg-background p-3 font-mono text-xs leading-relaxed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              />
              <DialogFooter className="items-center gap-2 sm:justify-between">
                <span className={cn("text-xs", tooLarge ? "text-destructive" : "text-muted-foreground")}>
                  {tooLarge
                    ? t("too_large", { limit: formatBytes(MAX_WRITE_BYTES) })
                    : formatBytes(dirtyBytes)}
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
                    <Button
                      onClick={() => void handleSave()}
                      disabled={saving || reloading || open.conflict || !open.editable || !dirty || tooLarge}
                    >
                      {saving ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                      ) : (
                        <Save className="h-4 w-4" />
                      )}
                      {saving ? t("saving") : t("save")}
                    </Button>
                  </div>
                )}
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>
    </>
  );
}
