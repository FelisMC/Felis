import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import {
  Archive,
  CheckCircle2,
  Clock,
  HardDrive,
  Loader2,
  RotateCcw,
  UserMinus,
  XCircle,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { BackLink } from "@/components/BackLink";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { MessageLine } from "@/components/MessageLine";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { PhaseBadge } from "@/components/PhaseBadge";
import { Loading, ErrorState, EmptyState, NotYours } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage, ownershipPending } from "@/lib/ownership";
import { formatBytes, formatRelative, formatAbsolute, isExpired } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { BackupView } from "@/lib/types";

/** LatestBackupCard renders the most-recent backup as the restore card — the one a
 *  restore actually recovers (the list is created_at-descending and the backend's
 *  LatestBackup selects the same present row), with the restore action beneath. Older
 *  archives are shown separately as a compact, read-only history (HistoryRow): a restore
 *  ALWAYS recovers this latest one, so giving an older backup an action card of its own
 *  would falsely imply you could restore (or delete) it — the backend offers neither.
 *  `showOwner` surfaces the former owner (admins list every world's backups; a user only
 *  ever sees their own). */
function BackupRow({
  b,
  isLatest,
  now,
  locale,
  showOwner,
  serverName,
  onReloadStatus,
}: {
  b: BackupView;
  isLatest: boolean;
  now: number;
  locale: string;
  showOwner?: boolean;
  serverName: string;
  onReloadStatus: () => void;
}) {
  const { t } = useTranslation("backups");
  const expired = isExpired(b.expires_at, now);
  const reasonLabel =
    b.reason === "inactive_15d"
      ? t("reason_inactive")
      : b.reason === "manual"
      ? t("reason_manual")
      : t("reason_label", { reason: b.reason });

  return (
    <tr className="hover:bg-muted/30 transition-colors">
      <td className="px-4 py-3 whitespace-nowrap">
        <div className="flex flex-col gap-0.5">
          <div className="flex items-center gap-2">
            <span
              className="font-medium text-foreground text-sm"
              title={formatAbsolute(b.created_at, locale)}
            >
              {formatRelative(b.created_at, now, locale)}
            </span>
            {isLatest && (
              <span className="inline-flex items-center rounded-md bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-medium text-emerald-500 ring-1 ring-inset ring-emerald-500/20">
                {t("latest_title")}
              </span>
            )}
          </div>
          <div className="text-[11px] text-muted-foreground flex items-center gap-1.5">
            <Clock className="h-3 w-3 text-muted-foreground/60 shrink-0" />
            <span>{reasonLabel}</span>
          </div>
        </div>
      </td>
      <td className="px-4 py-3.5 whitespace-nowrap text-muted-foreground">
        <span className="flex items-center gap-1.5">
          <HardDrive className="h-3.5 w-3.5 shrink-0" />
          {formatBytes(b.size_bytes)}
        </span>
      </td>
      <td className="px-4 py-3.5 whitespace-nowrap">
        <span className={cn("text-muted-foreground", expired && "font-medium text-destructive")}>
          {expired ? t("expired") : t("expires_in", { when: formatRelative(b.expires_at, now, locale) })}
        </span>
      </td>
      {showOwner && (
        <td className="px-4 py-3.5 whitespace-nowrap text-muted-foreground">
          {b.former_owner ? (
            <span className="flex items-center gap-1">
              <UserMinus className="h-3 w-3 shrink-0" />
              {b.former_owner}
            </span>
          ) : (
            <span className="text-muted-foreground/30">—</span>
          )}
        </td>
      )}
      <td className="px-4 py-3.5 whitespace-nowrap text-right">
        {!expired ? (
          <RestoreControls
            serverName={serverName}
            backup={b}
            now={now}
            locale={locale}
            onReloadStatus={onReloadStatus}
            buttonVariant={isLatest ? "destructive" : "outline"}
            buttonSize="sm"
            layout="row"
          />
        ) : (
          <span className="text-xs text-muted-foreground/40 font-medium px-3 py-1.5">
            {t("expired")}
          </span>
        )}
      </td>
    </tr>
  );
}

/** JobStateBadge renders one async Job's state (backup/restore Job history). The
 *  state vocabulary is the API's ("running" | "succeeded" | "failed"); anything
 *  unknown is shown verbatim rather than hidden. */
function JobStateBadge({ state }: { state: string }) {
  const { t } = useTranslation("backups");
  if (state === "running") {
    return (
      <span className="inline-flex items-center gap-1 rounded-md bg-sky-500/10 px-1.5 py-0.5 text-[10px] font-medium text-sky-500 ring-1 ring-inset ring-sky-500/20">
        <Loader2 className="h-3 w-3 animate-spin" />
        {t("job_running")}
      </span>
    );
  }
  if (state === "succeeded") {
    return (
      <span className="inline-flex items-center gap-1 rounded-md bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-medium text-emerald-500 ring-1 ring-inset ring-emerald-500/20">
        <CheckCircle2 className="h-3 w-3" />
        {t("job_succeeded")}
      </span>
    );
  }
  if (state === "failed") {
    return (
      <span className="inline-flex items-center gap-1 rounded-md bg-destructive/10 px-1.5 py-0.5 text-[10px] font-medium text-destructive ring-1 ring-inset ring-destructive/20">
        <XCircle className="h-3 w-3" />
        {t("job_failed")}
      </span>
    );
  }
  return <span className="text-[10px] font-medium text-muted-foreground">{state}</span>;
}


/** RestoreControls is the restore ACTION, living only on the latest backup card.
 *  Restore is the panel's one irreversible operation, so it hides behind a single
 *  button that opens a confirm dialog. The dialog states the full cost up front — the
 *  server is stopped (online players drop) and the current world is overwritten by
 *  THIS exact backup (named by relative + absolute time), and it can't be undone — and
 *  then, on confirm, runs the whole chain itself: stop → wait for Stopped → restore.
 *  The backend refuses a restore unless the world volume is free (409 not_stopped), so
 *  stopping here means the user never has to detour to the console and come back. There
 *  is deliberately no type-the-name step: the friction that matters is owning the
 *  server plus consciously confirming a player-kicking, world-overwriting act.
 *
 *  The stop-and-wait is a bounded async loop (not an effect): after api.stop it polls
 *  status until Stopped is observed, giving up after ~60s with a retryable timeout — so
 *  restore only fires once the volume is provably free. While the chain runs the dialog
 *  is locked (no ✕, no dismiss) so a mid-flight close can't strand it. A 202 is
 *  terminal: the dialog closes and the card shows a "restore started" note instead of
 *  re-offering the trigger, so a second restore Job can't race the first. */
const POLL_MS = 2500;
const MAX_POLLS = 24; // ~60s ceiling before we stop waiting for Stopped
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

function RestoreControls({
  serverName,
  backup,
  now,
  locale,
  onReloadStatus,
  buttonVariant = "destructive",
  buttonSize = "sm",
  layout = "card",
}: {
  serverName: string;
  backup: BackupView;
  now: number;
  locale: string;
  onReloadStatus: () => void;
  buttonVariant?: "destructive" | "outline" | "ghost" | "default";
  buttonSize?: "default" | "sm" | "lg" | "icon";
  layout?: "card" | "row";
}) {
  const { t } = useTranslation("backups");
  const [open, setOpen] = useState(false);
  // submitting flips synchronously on click (before the first await) so a double-click
  // on this destructive confirm can't launch two concurrent restore chains; step only
  // drives which progress label shows once the chain reaches a concrete stage.
  const [submitting, setSubmitting] = useState(false);
  const [step, setStep] = useState<"stopping" | "restoring" | null>(null);
  const [done, setDone] = useState(false); // restore enqueued — terminal
  const [error, setError] = useState<string | null>(null);

  if (isExpired(backup.expires_at, now)) {
    if (layout === "row") return null;
    return (
      <p className="mt-4 border-t border-primary/20 pt-4 text-xs text-destructive">
        {t("expired_cannot_restore")}
      </p>
    );
  }

  if (done) {
    if (layout === "row") {
      return (
        <div className="flex items-center gap-1.5 text-xs text-emerald-500 font-medium">
          <CheckCircle2 className="h-3.5 w-3.5 shrink-0" />
          <span>{t("restore_started_short")}</span>
        </div>
      );
    }
    return (
      <div className="mt-4 flex items-start gap-2 border-t border-primary/20 pt-4 text-sm text-emerald-500">
        <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0" />
        <span>{t("restore_started")}</span>
      </div>
    );
  }

  // Poll until the world volume is provably free. Returns true once Stopped is
  // observed, false after the ceiling — restore MUST NOT proceed on a false.
  async function waitForStopped(): Promise<boolean> {
    for (let i = 0; i < MAX_POLLS; i++) {
      await sleep(POLL_MS);
      const s = await api.status(serverName);
      if (s.phase === "Stopped") return true;
    }
    return false;
  }

  // The whole irreversible chain behind the one confirm. Re-checks live status first
  // (a public server may have autowoken), stops + waits only if needed, then restores.
  async function confirmRestore() {
    setSubmitting(true); // first + synchronous: disables the confirm before any await
    setError(null);
    try {
      const s0 = await api.status(serverName);
      if (s0.phase !== "Stopped") {
        setStep("stopping");
        await api.stop(serverName);
        if (!(await waitForStopped())) {
          setError(t("stop_timeout"));
          return;
        }
      }
      setStep("restoring");
      await api.restoreBackup(serverName, backup.id);
      setDone(true);
      setOpen(false);
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setSubmitting(false);
      setStep(null);
      onReloadStatus(); // resync the header phase badge after stop/restore
    }
  }

  const trigger = (
    <DialogTrigger asChild>
      <Button variant={buttonVariant} size={buttonSize}>
        <RotateCcw className="h-4 w-4" /> {t(layout === "row" ? "restore_btn_short" : "restore_btn")}
      </Button>
    </DialogTrigger>
  );

  const dialogContent = (
    <DialogContent hideClose={submitting}>
      <DialogHeader>
        <DialogTitle>{t("restore_btn")}</DialogTitle>
        <DialogDescription>
          {t("restore_confirm", {
            relative: formatRelative(backup.created_at, now, locale),
            absolute: formatAbsolute(backup.created_at, locale),
          })}
        </DialogDescription>
      </DialogHeader>

      {step && (
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          {step === "stopping" ? t("stopping") : t("restoring")}
        </div>
      )}
      {error && <p className="text-sm text-destructive">{error}</p>}

      <ConfirmFooter
        onCancel={() => setOpen(false)}
        onConfirm={confirmRestore}
        loading={submitting}
        cancelLabel={t("cancel")}
        confirmLabel={t("restore_confirm_yes")}
      />
    </DialogContent>
  );

  if (layout === "row") {
    return (
      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (submitting) return;
          if (next) setError(null);
          setOpen(next);
        }}
      >
        {trigger}
        {dialogContent}
      </Dialog>
    );
  }

  return (
    <div className="mt-4 border-t border-primary/20 pt-4">
      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (submitting) return; // locked while the stop→restore chain runs
          if (next) setError(null); // fresh each open
          setOpen(next);
        }}
      >
        {trigger}
        {dialogContent}
      </Dialog>
    </div>
  );
}

/** ServerBackups is the per-server backup surface (/servers/:name/backups): view the
 *  world archives kept for this server and (B2) roll the world back to the most
 *  recent one. It owns its own gating — ownership from /me/servers, since GET status
 *  never carries `owned` — but deliberately does NOT gate on readiness the way
 *  ServerPlayers does: backups are read from Postgres, not RCON, and a restore in
 *  fact requires the server to be STOPPED, so this page must work while it is asleep. */
export function ServerBackups() {
  const { name = "" } = useParams();
  const { t, i18n } = useTranslation("backups");
  const { isAdmin, loading: tierLoading } = useTier();
  const statusQ = useAsync(() => api.status(name), [name]);
  const mineQ = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name],
  );
  const backupsQ = useAsync(() => api.listBackups(), []);

  // Ownership resolves from /me/servers for a non-admin (status carries no `owned`).
  // While it is pending show the header with a spinner rather than flashing the list
  // at someone who may not own it; if that read itself failed, break to a retry so a
  // real owner never fails closed to NotYours on a transient blip.
  const pending = ownershipPending(tierLoading, isAdmin, mineQ.data, mineQ.error);
  const owned = canManage(isAdmin, mineQ.data, name);

  // The async world-operation history (the backup/restore Jobs behind every 202).
  // Read only once the viewer is resolved as owner-or-admin (the route 403s
  // otherwise); while anything is still running it re-reads on an interval so the
  // enqueue converges to succeeded/failed here instead of only in kubectl.
  const jobsQ = useAsync(
    () => (owned ? api.serverJobs(name) : Promise.resolve([])),
    [name, owned],
  );
  useEffect(() => {
    if (!(jobsQ.data ?? []).some((j) => j.state === "running")) return;
    const id = setInterval(jobsQ.reload, 5000);
    return () => clearInterval(id);
  }, [jobsQ.data, jobsQ.reload]);

  const [backingUp, setBackingUp] = useState(false);
  const [backupMsg, setBackupMsg] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  // Manual backup (POST /backup): the backend enforces the stopped gate, so the
  // button only enables on Stopped and a raced 409 is surfaced in its own words.
  async function handleBackupNow() {
    if (backingUp) return;
    setBackingUp(true);
    setBackupMsg(null);
    try {
      await api.backupNow(name);
      setBackupMsg({ kind: "success", text: t("backup_started") });
      jobsQ.reload();
    } catch (e: any) {
      setBackupMsg({
        kind: "error",
        text: e && e.code === "not_stopped" ? t("backup_requires_stopped") : humanizeError(e),
      });
    } finally {
      setBackingUp(false);
    }
  }

  const back = (
    <BackLink to={`/servers/${name}`} label={t("back_to_console")} />
  );

  if (statusQ.loading && !statusQ.data) {
    return (
      <>
        {back}
        <Loading />
      </>
    );
  }
  if (statusQ.error) {
    return (
      <>
        {back}
        <ErrorState error={statusQ.error} onRetry={statusQ.reload} />
      </>
    );
  }
  if (!statusQ.data) return back;

  const now = Date.now();
  const locale = i18n.language;
  // The global list, narrowed to this server. Already created_at-descending from the
  // API, but re-sorted defensively so all[0] is unambiguously the restore target.
  const all = (backupsQ.data ?? [])
    .filter((b) => b.server_name === name)
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));

  const header = (
    <PageHeader
      icon={Archive}
      title={statusQ.data.displayName || statusQ.data.name}
      subtitle={t("title")}
      actions={
        <div className="flex items-center gap-2">
          {owned && (
            <Button
              size="sm"
              variant="outline"
              onClick={handleBackupNow}
              disabled={backingUp || statusQ.data.phase !== "Stopped"}
              title={
                statusQ.data.phase !== "Stopped" ? t("backup_requires_stopped") : undefined
              }
            >
              {backingUp ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Archive className="h-4 w-4" />
              )}
              {backingUp ? t("backup_in_progress") : t("backup_now")}
            </Button>
          )}
          <PhaseBadge phase={statusQ.data.phase} />
        </div>
      }
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
          {backupMsg && <MessageLine kind={backupMsg.kind} message={backupMsg.text} />}
          <p className="text-sm text-muted-foreground">{t("subtitle")}</p>
          {backupsQ.loading && !backupsQ.data ? (
            <Loading />
          ) : backupsQ.error ? (
            <ErrorState error={backupsQ.error} onRetry={backupsQ.reload} />
          ) : all.length === 0 ? (
            <EmptyState title={t("empty_title")} hint={t("empty_hint")} />
          ) : (
            <>
              <Card className="overflow-hidden">
                <div className="overflow-x-auto">
                  <table className="w-full border-collapse text-sm">
                    <thead>
                      <tr className="border-b border-border bg-muted/40 text-left text-[11px] uppercase tracking-wider text-muted-foreground">
                        <th className="px-4 py-2.5 font-medium">{t("col_created")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("col_size")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("col_expires")}</th>
                        {isAdmin && <th className="px-4 py-2.5 font-medium">{t("col_owner")}</th>}
                        <th className="px-4 py-2.5 text-right font-medium">{t("col_actions")}</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-border">
                      {all.map((b, idx) => (
                        <BackupRow
                          key={b.id}
                          b={b}
                          isLatest={idx === 0}
                          now={now}
                          locale={locale}
                          showOwner={isAdmin}
                          serverName={name}
                          onReloadStatus={() => {
                            statusQ.reload();
                            jobsQ.reload();
                          }}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
              </Card>

              <Card className="border-dashed bg-transparent shadow-none">
                <CardContent className="p-4 text-xs text-muted-foreground">
                  {t("history_note")}
                </CardContent>
              </Card>
            </>
          )}

          {/* Async world-operation history: every backup/restore 202 lands here, so a
              Job that later failed stays visible (with its message) without kubectl. */}
          {jobsQ.error ? (
            <ErrorState error={jobsQ.error} onRetry={jobsQ.reload} />
          ) : (
            <Card className="overflow-hidden">
              <CardContent className="p-4">
                <div className="flex items-center gap-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                  <Clock className="h-3.5 w-3.5" />
                  {t("jobs_title")}
                </div>
                {(jobsQ.data ?? []).length === 0 ? (
                  <p className="mt-2 text-xs text-muted-foreground">{t("jobs_empty")}</p>
                ) : (
                  <ul className="mt-2 divide-y divide-border">
                    {(jobsQ.data ?? []).map((j) => {
                      const at = j.started_at || j.finished_at;
                      return (
                        <li key={j.name} className="flex items-start justify-between gap-3 py-2.5">
                          <div className="flex min-w-0 items-center gap-2">
                            {j.kind === "restore" ? (
                              <RotateCcw className="h-3.5 w-3.5 shrink-0 text-muted-foreground/60" />
                            ) : (
                              <Archive className="h-3.5 w-3.5 shrink-0 text-muted-foreground/60" />
                            )}
                            <span className="text-sm">
                              {j.kind === "restore"
                                ? t("job_restore")
                                : j.kind === "backup"
                                ? t("job_backup")
                                : j.kind}
                            </span>
                            {at && (
                              <span
                                className="text-xs text-muted-foreground"
                                title={formatAbsolute(at, locale)}
                              >
                                {formatRelative(at, now, locale)}
                              </span>
                            )}
                          </div>
                          <div className="flex min-w-0 flex-col items-end gap-0.5">
                            <JobStateBadge state={j.state} />
                            {j.state === "failed" && j.message && (
                              <span
                                className="max-w-[22rem] truncate text-xs text-destructive"
                                title={j.message}
                              >
                                {j.message}
                              </span>
                            )}
                          </div>
                        </li>
                      );
                    })}
                  </ul>
                )}
              </CardContent>
            </Card>
          )}
        </div>
      )}
    </>
  );
}
