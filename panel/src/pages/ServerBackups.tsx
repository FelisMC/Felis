import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import {
  AlertTriangle,
  Archive,
  CheckCircle2,
  Clock,
  HardDrive,
  Loader2,
  RotateCcw,
  ShieldCheck,
  ShieldX,
  UserMinus,
  XCircle,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { BackLink } from "@/components/BackLink";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { MessageLine, InlineError } from "@/components/MessageLine";
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
import type { BackupView, ServerJob } from "@/lib/types";

const CELL = "whitespace-nowrap md:px-4 md:py-3.5";

/** BackupRow is one backup in the table, with its own restore action. `isLatest`
 *  marks the row a restore with no pick recovers: the newest one that is not
 *  corrupt, which is what the backend's LatestBackup selects. Under the reason it
 *  shows what the reaper's read-back found — corrupt (restore refused), verified,
 *  or entries the archive could not hold. `showOwner` surfaces the former owner
 *  (admins list every world's backups; a user only ever sees their own). */
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
      : b.reason === "pre_restore"
      ? t("reason_pre_restore")
      : t("reason_label", { reason: b.reason });

  // Below md the row stops being a table row: the when/why block takes the full
  // width, size and expiry follow on one line and the restore button sits at the
  // right, so a phone never has to scroll sideways to reach it.
  return (
    <tr className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3 transition-colors hover:bg-muted/30 md:table-row md:p-0">
      <td className="w-full md:w-auto md:px-4 md:py-3 md:whitespace-nowrap">
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
          <IntegrityNote b={b} now={now} locale={locale} />
        </div>
      </td>
      <td className={cn(CELL, "text-muted-foreground")}>
        <span className="flex items-center gap-1.5">
          <HardDrive className="h-3.5 w-3.5 shrink-0" />
          {formatBytes(b.size_bytes)}
        </span>
      </td>
      <td className={CELL}>
        <span className={cn("text-muted-foreground", expired && "font-medium text-destructive")}>
          {expired ? t("expired") : t("expires_in", { when: formatRelative(b.expires_at, now, locale) })}
        </span>
      </td>
      {showOwner && (
        <td className={cn(CELL, "min-w-0 text-muted-foreground")}>
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
      <td className={cn(CELL, "ml-auto text-right")}>
        {b.corrupt ? (
          <span className="text-xs text-destructive/70 font-medium px-3 py-1.5">
            {t("corrupt_short")}
          </span>
        ) : !expired ? (
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

/** IntegrityNote is what the reaper's read-back says about one archive: corrupt
 *  (it no longer matches what was written, so it cannot be restored), when it
 *  was last read back intact, and how many entries of the world it could not
 *  hold. A backup not read back yet shows nothing, as before. */
function IntegrityNote({ b, now, locale }: { b: BackupView; now: number; locale: string }) {
  const { t } = useTranslation("backups");
  const skipped = b.skipped_entries ?? 0;
  if (!b.corrupt && !b.verified_at && skipped === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[11px]">
      {b.corrupt ? (
        <span
          className="inline-flex items-center gap-1 font-medium text-destructive"
          title={t("corrupt_hint")}
        >
          <ShieldX className="h-3 w-3 shrink-0" />
          {t("corrupt_badge")}
        </span>
      ) : (
        b.verified_at && (
          <span
            className="inline-flex items-center gap-1 text-muted-foreground"
            title={formatAbsolute(b.verified_at, locale)}
          >
            <ShieldCheck className="h-3 w-3 shrink-0 text-emerald-500/80" />
            {t("verified_at", { when: formatRelative(b.verified_at, now, locale) })}
          </span>
        )
      )}
      {skipped > 0 && (
        <span
          className="inline-flex items-center gap-1 text-amber-600 dark:text-amber-500"
          title={t("skipped_entries_hint")}
        >
          <AlertTriangle className="h-3 w-3 shrink-0" />
          {t("skipped_entries", { count: skipped })}
        </span>
      )}
    </div>
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

/** ChainNote says what became of the restore behind a safety snapshot (a backup
 *  job carrying then_restore). A snapshot that failed already reads as a failed
 *  job with its error, so this only covers the chain itself. */
function ChainNote({ job }: { job: ServerJob }) {
  const { t } = useTranslation("backups");
  if (job.state === "failed") return null;
  if (job.then_restore === "pending") {
    return (
      <span className="text-xs text-muted-foreground">
        {job.state === "running" ? t("chain_pending") : t("chain_starting")}
      </span>
    );
  }
  if (job.then_restore === "started") {
    return <span className="text-xs text-muted-foreground">{t("chain_started")}</span>;
  }
  if (job.then_restore === "abandoned") {
    // Known codes get their own wording; one this panel predates falls back to
    // the backend's English.
    const known = ["snapshot_failed", "not_configured", "server_gone", "server_started", "restore_busy"];
    const reason = job.then_restore_reason;
    const text =
      reason && known.includes(reason)
        ? t(`chain_abandoned_${reason}`)
        : job.message
        ? t("chain_abandoned_because", { reason: job.message })
        : t("chain_abandoned");
    return (
      <span
        className="max-w-[22rem] truncate text-xs text-amber-600 dark:text-amber-500"
        title={text}
      >
        {text}
      </span>
    );
  }
  return null;
}

/** RestoreControls is the restore ACTION on each unexpired backup row. Restore
 *  overwrites the world, so it hides behind a single button that opens a confirm
 *  dialog. The dialog states the full cost up front — the server is stopped (online
 *  players drop) and the current world is overwritten by THIS exact backup — and
 *  offers a safety snapshot, on by default: the backend first backs up the world as
 *  it is and restores only once that succeeded, which makes a wrong pick undoable.
 *  Turning it off brings back the irreversible wording. On confirm it runs the whole
 *  chain itself: stop → wait for Stopped → restore.
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
  // restore enqueued — terminal; "snapshot" when the backend backs the world up first
  const [done, setDone] = useState<"snapshot" | "direct" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [safety, setSafety] = useState(true);

  if (backup.corrupt) {
    if (layout === "row") return null;
    return (
      <p className="mt-4 border-t border-primary/20 pt-4 text-xs text-destructive">
        {t("corrupt_cannot_restore")}
      </p>
    );
  }

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
          <span>{t(done === "snapshot" ? "restore_snapshot_started_short" : "restore_started_short")}</span>
        </div>
      );
    }
    return (
      <div className="mt-4 flex items-start gap-2 border-t border-primary/20 pt-4 text-sm text-emerald-500">
        <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0" />
        <span>{t(done === "snapshot" ? "restore_snapshot_started" : "restore_started")}</span>
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
      const res = await api.restoreBackup(serverName, backup.id, safety);
      setDone(res.safety_snapshot ? "snapshot" : "direct");
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
          {t(safety ? "restore_confirm_safe" : "restore_confirm", {
            relative: formatRelative(backup.created_at, now, locale),
            absolute: formatAbsolute(backup.created_at, locale),
          })}
        </DialogDescription>
      </DialogHeader>

      <label
        className={cn(
          "flex cursor-pointer items-start gap-3 rounded-md border p-3 text-sm transition-colors",
          safety ? "border-primary/30 bg-primary/5" : "border-destructive/30 bg-destructive/5",
          submitting && "cursor-not-allowed opacity-60",
        )}
      >
        <input
          type="checkbox"
          className="mt-0.5 h-4 w-4 shrink-0 cursor-pointer accent-primary disabled:cursor-not-allowed"
          checked={safety}
          disabled={submitting}
          onChange={(e) => setSafety(e.target.checked)}
        />
        <span className="flex flex-col gap-1">
          <span className="flex items-center gap-1.5 font-medium text-foreground">
            <ShieldCheck className="h-4 w-4 shrink-0 text-primary" />
            {t("safety_snapshot_label")}
          </span>
          <span className="text-xs text-muted-foreground">
            {t(safety ? "safety_snapshot_hint" : "safety_snapshot_off_hint")}
          </span>
        </span>
      </label>

      {step && (
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          {step === "stopping" ? t("stopping") : t("restoring")}
        </div>
      )}
      <InlineError message={error} />

      <ConfirmFooter
        onCancel={() => setOpen(false)}
        onConfirm={confirmRestore}
        loading={submitting}
        cancelLabel={t("cancel")}
        confirmLabel={t(safety ? "restore_confirm_yes" : "restore_confirm_yes_unsafe")}
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
  // otherwise); while anything is still running — or a safety snapshot still has
  // its restore to start — it re-reads on an interval so the enqueue converges to
  // succeeded/failed here instead of only in kubectl.
  const jobsQ = useAsync(
    () => (owned ? api.serverJobs(name) : Promise.resolve([])),
    [name, owned],
  );
  useEffect(() => {
    if (!(jobsQ.data ?? []).some((j) => j.state === "running" || j.then_restore === "pending")) return;
    const id = setInterval(jobsQ.reload, 5000);
    return () => clearInterval(id);
  }, [jobsQ.data, jobsQ.reload]);
  // A backup job that stops running has just added (or failed to add) a row, so
  // the list is re-read then rather than only on the next visit.
  const runningBackups = useRef<Set<string>>(new Set());
  const reloadBackups = backupsQ.reload;
  useEffect(() => {
    const now = new Set(
      (jobsQ.data ?? []).filter((j) => j.kind === "backup" && j.state === "running").map((j) => j.name),
    );
    const finished = [...runningBackups.current].some((n) => !now.has(n));
    runningBackups.current = now;
    if (finished) reloadBackups();
  }, [jobsQ.data, reloadBackups]);

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
  // API, but re-sorted defensively; the newest backup that is not corrupt is the
  // one a restore with no pick recovers.
  const all = (backupsQ.data ?? [])
    .filter((b) => b.server_name === name)
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
  const latestID = all.find((b) => !b.corrupt)?.id;

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
                  <table className="block w-full border-collapse text-sm md:table">
                    <thead className="hidden md:table-header-group">
                      <tr className="border-b border-border bg-muted/40 text-left text-[11px] uppercase tracking-wider text-muted-foreground">
                        <th className="px-4 py-2.5 font-medium">{t("col_created")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("col_size")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("col_expires")}</th>
                        {isAdmin && <th className="px-4 py-2.5 font-medium">{t("col_owner")}</th>}
                        <th className="px-4 py-2.5 text-right font-medium">{t("col_actions")}</th>
                      </tr>
                    </thead>
                    <tbody className="block divide-y divide-border md:table-row-group">
                      {all.map((b) => (
                        <BackupRow
                          key={b.id}
                          b={b}
                          isLatest={b.id === latestID}
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
                                ? j.then_restore
                                  ? t("job_pre_restore")
                                  : t("job_backup")
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
                            {j.then_restore && <ChainNote job={j} />}
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
