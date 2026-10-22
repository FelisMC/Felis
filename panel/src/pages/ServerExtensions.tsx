import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { FolderOpen, Loader2, Puzzle, RefreshCw, Trash2, Upload } from "lucide-react";
import { useTranslation } from "react-i18next";
import { BackLink } from "@/components/BackLink";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { MessageLine } from "@/components/MessageLine";
import { PageHeader } from "@/components/PageHeader";
import { PhaseBadge, shownPhase } from "@/components/PhaseBadge";
import { EmptyState, ErrorState, Loading, NotYours, RefreshError } from "@/components/States";
import { UploadQueue } from "@/components/files/UploadQueue";
import { useFileOps } from "@/components/files/useFileOps";
import { useFileWorldConfirmation } from "@/components/files/useFileWorldConfirmation";
import { useUploads } from "@/components/files/useUploads";
import { holderText, useWorldJobs } from "@/components/files/useWorldJobs";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api, humanizeError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { STATUS_POLL_FAST_MS, useAsync, usePolling, useUnsavedGuard } from "@/lib/hooks";
import { canManage, ownershipPending } from "@/lib/ownership";
import { useTier } from "@/lib/tier";
import type { ServerExtension } from "@/lib/types";

const LOADER_LABELS: Record<string, string> = { bukkit: "Bukkit", paper: "Paper", fabric: "Fabric", quilt: "Quilt", forge: "Forge", neoforge: "NeoForge", velocity: "Velocity" };

export function ServerExtensions() {
  const { name = "" } = useParams();
  return <InstalledExtensions key={name} name={name} />;
}

function InstalledExtensions({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  const { isAdmin, loading: tierLoading } = useTier();
  const status = useAsync(() => api.status(name), [name]);
  const mine = useAsync(() => isAdmin ? Promise.resolve([]) : api.myServers(), [name, isAdmin]);
  const pending = ownershipPending(tierLoading, isAdmin, mine.data, mine.error);
  const owned = canManage(isAdmin, mine.data, name);
  const live = status.data?.phase === "Running" && status.data.desiredState === "Running";
  const accessible = status.data?.phase === "Stopped" || live;
  const enabled = owned && !pending && accessible;
  const world = useFileWorldConfirmation(name, owned, live);
  const listing = useAsync(() => enabled ? api.listServerExtensions(name) : Promise.resolve(null), [name, enabled]);
  const { reload: reloadExtensions } = listing;
  const { reload: reloadWorlds } = world;
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const [changing, setChanging] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<ServerExtension | null>(null);
  const [target, setTarget] = useState<"plugins" | "mods">("plugins");
  const input = useRef<HTMLInputElement>(null);
  const landed = useRef(false);
  const fileOps = useFileOps(name, enabled, listing.reload);
  const jobs = useWorldJobs(name, enabled, listing.reload);
  const held = !enabled || status.error != null || changing !== null || fileOps.running || jobs.holder !== null;
  const uploads = useUploads(name, {
    onLanded: () => { landed.current = true; },
    onOp: fileOps.ignore,
    beforeUpload: (path) => {
      if (held) throw new Error(t("extensions_wait"));
      world.requireConfirmation([path]);
    },
    hold: held,
  });
  const busy = held || uploads.busy;
  useUnsavedGuard(uploads.busy);
  usePolling(status.reload, STATUS_POLL_FAST_MS);
  useEffect(() => {
    if (uploads.busy || !landed.current) return;
    landed.current = false;
    reloadExtensions();
    reloadWorlds();
  }, [uploads.busy, reloadExtensions, reloadWorlds]);

  function confirmation(id: string) {
    return <label htmlFor={id} className="flex items-start gap-2 text-sm">
      <Checkbox id={id} checked={world.confirmed} onChange={(event) => world.setConfirmed(event.target.checked)} />
      <span>{t("files:live_world_confirm")}</span>
    </label>;
  }

  function requireWritable(paths: string[]) {
    if (busy) throw new Error(t("extensions_wait"));
    world.requireConfirmation(paths);
  }

  async function toggle(entry: ServerExtension) {
    const to = entry.enabled ? entry.path + ".disabled" : entry.path.slice(0, -".disabled".length);
    setMessage(null);
    try {
      requireWritable([entry.path, to]);
      setChanging(entry.path);
      await api.renameServerFile(name, entry.path, to);
      setMessage({ kind: "success", text: t(entry.enabled ? "extensions_disabled_success" : "extensions_enabled_success", { filename: entry.filename }) });
      listing.reload();
    } catch (error) {
      if ((error as { code?: string }).code === "maintenance_in_progress") jobs.refresh();
      setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setChanging(null);
    }
  }

  async function remove() {
    if (!deleting) return;
    requireWritable([deleting.path]);
    setChanging(deleting.path);
    try {
      await api.deleteServerFile(name, deleting.path);
      setMessage({ kind: "success", text: t("extensions_deleted_success", { filename: deleting.filename }) });
      listing.reload();
    } catch (error) {
      if ((error as { code?: string }).code === "maintenance_in_progress") jobs.refresh();
      throw error;
    } finally {
      setChanging(null);
    }
  }

  function add(files: File[]) {
    if (files.length === 0) return;
    try {
      requireWritable(files.map((file) => `${target}/${file.name}`));
      if (files.some((file) => !/\.jar$/i.test(file.name))) throw new Error(t("extensions_jar_required"));
      uploads.add(files, "", null, files.map((file) => `${target}/${file.name}`));
      setMessage(null);
    } catch (error) {
      setMessage({ kind: "error", text: humanizeError(error) });
    }
  }

  const entries = listing.data?.entries ?? [];
  const back = <BackLink to={`/servers/${name}`} label={t("files:back_to_console")} />;
  if (status.loading && !status.data) return <>{back}<Loading /></>;
  if (status.error && !status.data) return <>{back}<ErrorState error={status.error} onRetry={status.reload} /></>;
  if (!status.data) return back;
  return <>
    {back}
    <PageHeader icon={Puzzle} title={status.data.displayName || status.data.name} subtitle={t("extensions_title")}
      actions={<PhaseBadge phase={shownPhase(status.data)} />} className="mb-6" />
    {pending ? <Loading /> : mine.error ? <ErrorState error={mine.error} onRetry={mine.reload} /> : !owned ?
      <NotYours title={t("extensions_not_yours_title")} body={t("extensions_not_yours_body")} /> :
      <div className="space-y-4">
        {status.error != null && <RefreshError error={status.error} />}
        {live && <section role="status" className="space-y-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-4 text-sm">
          <p>{t("extensions_restart_hint")}</p>
          {confirmation("extensions-world-confirm")}
        </section>}
        {message && <MessageLine kind={message.kind} message={message.text} />}
        {!accessible ? <EmptyState title={t("files:stopped_required_title")} hint={t("files:stopped_required_body")} /> :
          <Card><CardContent className="p-0">
            <div className="flex flex-wrap items-center gap-2 border-b border-border p-4">
              <Select value={target} onValueChange={(value) => setTarget(value as "plugins" | "mods")} disabled={busy}>
                <SelectTrigger aria-label={t("extensions_upload_target")} className="w-40"><SelectValue /></SelectTrigger>
                <SelectContent><SelectItem value="plugins">{t("extensions_plugins")}</SelectItem><SelectItem value="mods">{t("extensions_mods")}</SelectItem></SelectContent>
              </Select>
              <Button variant="outline" disabled={busy || world.needsConfirmation([target])} onClick={() => input.current?.click()}>
                <Upload className="h-4 w-4" />{t("extensions_upload")}
              </Button>
              <input ref={input} data-testid="extension-upload-input" type="file" accept=".jar" multiple hidden
                onChange={(event) => { add(Array.from(event.target.files ?? [])); event.target.value = ""; }} />
              <Button variant="ghost" className="ml-auto" disabled={busy || listing.loading} onClick={() => { listing.reload(); world.reload(); jobs.refresh(); fileOps.refresh(); }}
                aria-label={t("extensions_refresh")}>
                {listing.loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />}
              </Button>
            </div>
            {jobs.holder && <p role="status" className="border-b border-border px-4 py-3 text-sm text-muted-foreground">{t(`files:${holderText(jobs.holder)}`)}</p>}
            {!jobs.holder && fileOps.running && <p role="status" className="border-b border-border px-4 py-3 text-sm text-muted-foreground">{t("files:wait_for_op")}</p>}
            <UploadQueue items={uploads.items} dir={target} onReplace={uploads.replace} onRetry={uploads.retry} onRemove={uploads.remove}
              onReplaceAll={uploads.replaceAll} onSkipAll={uploads.skipAll} onClearDone={uploads.clearDone} />
            {listing.loading && !listing.data ? <Loading /> : listing.error && !listing.data ?
              <ErrorState error={listing.error} onRetry={listing.reload} /> : <>
                {listing.error != null && <div className="p-4"><RefreshError error={listing.error} /></div>}
                {entries.length === 0 ? <div className="p-4"><EmptyState title={t("extensions_empty_title")} hint={t("extensions_empty_hint")} /></div> :
                  <div className="divide-y divide-border">{entries.map((entry) => <article key={entry.path} aria-label={entry.filename} className="grid gap-4 p-4 lg:grid-cols-[minmax(0,1fr)_auto]">
                    <div className="min-w-0 space-y-3">
                      <div className="flex flex-wrap items-center gap-2">
                        <Badge variant="outline">{t(entry.kind === "plugin" ? "extensions_plugin" : "extensions_mod")}</Badge>
                        <Badge variant={entry.enabled ? "default" : "outline"}>{t(entry.enabled ? "extensions_enabled" : "extensions_disabled")}</Badge>
                        <span className="break-all font-mono text-xs text-muted-foreground">{entry.filename}</span>
                        <span className="text-xs tabular-nums text-muted-foreground">{formatBytes(entry.size)}</span>
                      </div>
                      {entry.metadata_status === "unavailable" ? <p className="text-sm text-amber-700 dark:text-amber-300">{t("extensions_metadata_unavailable")}</p> :
                        <ul className="grid gap-3">{entry.metadata.map((item, index) => <li key={`${item.id}:${index}`} className="min-w-0">
                          <p className="flex flex-wrap items-baseline gap-x-2"><span className="break-all text-sm font-medium">{item.name}</span>
                            {item.id !== item.name && <span className="break-all font-mono text-xs text-muted-foreground">{item.id}</span>}
                            <span className="break-all text-xs text-muted-foreground">{t("extensions_version", { version: item.version || t("extensions_unknown") })}</span>
                          </p>
                          <dl className="mt-1 grid gap-1 text-xs text-muted-foreground sm:grid-cols-2">
                            <div className="flex min-w-0 gap-1"><dt>{t("extensions_authors")}</dt><dd className="min-w-0 break-all">{[...new Set(item.authors)].join(", ") || t("extensions_unknown")}</dd></div>
                            <div className="flex min-w-0 gap-1"><dt>{t("extensions_loaders")}</dt><dd className="min-w-0 break-all">{[...new Set(item.loaders.map((loader) => LOADER_LABELS[loader] ?? loader))].join(", ") || t("extensions_unknown")}</dd></div>
                          </dl>
                        </li>)}</ul>}
                    </div>
                    <div className="flex flex-wrap items-start gap-2">
                      {entry.config_path && <Button asChild variant="outline" size="sm"><Link to={`/servers/${name}/files?path=${encodeURIComponent(entry.config_path)}`}>
                        <FolderOpen className="h-3.5 w-3.5" />{t("extensions_config")}</Link></Button>}
                      <Button variant="outline" size="sm" disabled={busy || world.needsConfirmation([entry.path])} onClick={() => void toggle(entry)}>
                        {changing === entry.path && <Loader2 className="h-3.5 w-3.5 animate-spin" />}{t(entry.enabled ? "extensions_disable" : "extensions_enable")}
                      </Button>
                      <Button variant="ghost" size="sm" disabled={busy} onClick={() => { setMessage(null); setDeleting(entry); }} aria-label={t("extensions_delete_item", { filename: entry.filename })}>
                        <Trash2 className="h-3.5 w-3.5 text-destructive" />
                      </Button>
                    </div>
                  </article>)}</div>}
                {listing.data?.truncated && <p className="border-t border-border p-4 text-xs text-amber-700 dark:text-amber-300">{t("extensions_truncated")}</p>}
              </>}
          </CardContent></Card>}
      </div>}
    <ConfirmDialog open={deleting !== null} onOpenChange={(open) => { if (!open) setDeleting(null); }}
      title={t("extensions_delete_title", { filename: deleting?.filename ?? "" })} description={t("extensions_delete_body")}
      confirmLabel={t("files:delete")} onConfirm={remove} confirmDisabled={busy || !!deleting && world.needsConfirmation([deleting.path])}>
      {live && deleting && world.needsConfirmation([deleting.path]) && confirmation("extension-delete-world-confirm")}
    </ConfirmDialog>
  </>;
}
