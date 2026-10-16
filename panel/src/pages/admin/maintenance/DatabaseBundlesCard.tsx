import { useEffect, useState } from "react";
import { Archive, Loader2, RotateCcw, Save, ShieldCheck, Trash2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { HostTaskNotice, HostTaskStatus, useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";
import { Pagination } from "@/components/Pagination";
import { formatAbsolute, formatBytes } from "@/lib/format";
import type { AsyncState } from "@/lib/hooks";
import { useUnsavedGuard } from "@/lib/hooks";
import type { HostOverview } from "@/lib/types";

const LABEL_KEY: Record<string, string> = {
  daily: "dbbackup_label_daily",
  "pre-migrate": "dbbackup_label_pre_migrate",
  "pre-restore": "dbbackup_label_pre_restore",
  offsite: "dbbackup_label_offsite",
  manual: "dbbackup_label_manual",
};

type HostTask = ReturnType<typeof useHostTask>;

/** BUNDLE_PAGE_SIZE keeps the list short: manual backups are never pruned. */
const BUNDLE_PAGE_SIZE = 10;

/** DatabaseBundlesCard lists the database bundles on the host: each can be verified or
 *  restored, and the daily schedule that writes them can be changed. A restore stops the
 *  panel and the operator while it runs; the host bundles the current database first. */
export function DatabaseBundlesCard({ overview, db }: { overview: AsyncState<HostOverview>; db: HostTask }) {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const schedule = useHostTask(["host-settings"]);
  const bundles = overview.data?.databaseBackups ?? [];
  const settings = overview.data?.settings;
  const [restore, setRestore] = useState<string | null>(null);
  const [remove, setRemove] = useState<string | null>(null);
  const [page, setPage] = useState(1);
  const pages = Math.max(1, Math.ceil(bundles.length / BUNDLE_PAGE_SIZE));
  const current = Math.min(page, pages);
  const shown = bundles.slice((current - 1) * BUNDLE_PAGE_SIZE, current * BUNDLE_PAGE_SIZE);
  const newest = bundles[0]?.name;
  // A finished deletion or manual backup changes the list; read it again once.
  const finished = db.task?.state === "succeeded" && ["db-delete", "db-backup"].includes(db.task.request.action) ? db.task.id : null;
  useEffect(() => {
    if (finished) overview.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [finished]);
  const [typed, setTyped] = useState("");
  const [time, setTime] = useState("");
  const [keep, setKeep] = useState("");
  useEffect(() => {
    if (settings) {
      setTime(settings.dbBackupTime);
      setKeep(settings.dbBackupKeep ? String(settings.dbBackupKeep) : "");
    }
  }, [settings]);
  const dirty = !!settings && (time !== settings.dbBackupTime || keep !== String(settings.dbBackupKeep || ""));
  useUnsavedGuard(dirty);
  const validTime = /^([01][0-9]|2[0-3]):[0-5][0-9]$/.test(time);
  const keepNumber = Number(keep);
  const validKeep = /^\d+$/.test(keep) && keepNumber >= 1 && keepNumber <= 365;
  async function saveSchedule() {
    const params: Record<string, string> = {};
    if (time !== settings?.dbBackupTime) params.dbBackupTime = time;
    if (keep !== String(settings?.dbBackupKeep || "")) params.dbBackupKeep = keep;
    if (await schedule.start({ action: "host-settings", params, confirmMaintenance: false })) overview.reload();
  }
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><Archive className="h-4 w-4 text-primary" aria-hidden="true" />{t("db_bundles_title")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("db_bundles_description")}</p>
        <HostTaskNotice otherRunning={db.otherRunning} available={db.available} loading={db.loading} />
        {overview.data?.databaseBackupsError && <MessageLine kind="error" message={overview.data.databaseBackupsError} />}
        {overview.data && bundles.length === 0 && <p className="text-sm text-muted-foreground">{t("db_bundles_none")}</p>}
        {bundles.length > 0 && (
          <ul className="divide-y divide-border rounded-lg border border-border">
            {shown.map((b) => (
              <li key={b.name} className="flex flex-wrap items-center justify-between gap-3 p-3">
                <div className="min-w-0 space-y-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-sm font-medium">{formatAbsolute(b.created, locale)}</span>
                    <Badge variant="muted">{LABEL_KEY[b.label] ? t(LABEL_KEY[b.label]) : b.label}</Badge>
                    <span className="text-xs text-muted-foreground">{formatBytes(b.size)}</span>
                  </div>
                  <p className="truncate font-mono text-[11px] text-muted-foreground" title={b.name}>{b.name}</p>
                </div>
                <div className="flex shrink-0 gap-2">
                  <Button variant="outline" size="sm" disabled={db.busy} onClick={() => void db.start({ action: "db-verify", params: { bundle: b.name }, confirmMaintenance: false })}>
                    <ShieldCheck />{t("db_bundle_verify")}
                  </Button>
                  <Button variant="outline" size="sm" disabled={db.busy} onClick={() => { setRestore(b.name); setTyped(""); }}>
                    <RotateCcw />{t("db_bundle_restore")}
                  </Button>
                  <Button variant="outline" size="icon" className="h-8 w-8" aria-label={t("db_bundle_delete_label", { name: b.name })}
                    title={b.name === newest ? t("db_bundle_newest") : undefined}
                    disabled={db.busy || b.name === newest} onClick={() => setRemove(b.name)}>
                    <Trash2 />
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
        {bundles.length > BUNDLE_PAGE_SIZE && <Pagination page={current} pageSize={BUNDLE_PAGE_SIZE} total={bundles.length} onChange={setPage} />}
        {db.error && <MessageLine kind="error" message={db.error} />}
        {db.task && <HostTaskStatus task={db.task} />}

        <section aria-label={t("db_schedule_title")} className="space-y-4 border-t border-border pt-5">
          <div className="space-y-1">
            <h3 className="text-sm font-semibold">{t("db_schedule_title")}</h3>
            <p className="text-xs leading-relaxed text-muted-foreground">{t("db_schedule_hint")}</p>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="db-backup-time">{t("db_schedule_time")}</Label>
              <Input id="db-backup-time" type="time" value={time} disabled={!settings || schedule.busy} onChange={(e) => setTime(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="db-backup-keep">{t("db_schedule_keep")}</Label>
              <Input id="db-backup-keep" type="number" min={1} max={365} value={keep} disabled={!settings || schedule.busy} onChange={(e) => setKeep(e.target.value)} />
            </div>
          </div>
          {settings && !settings.dbBackupTime && <p className="text-xs text-muted-foreground">{t("db_schedule_custom")}</p>}
          <div className="flex flex-wrap items-center gap-3">
            <Button disabled={!dirty || !validTime || !validKeep || schedule.busy} onClick={() => void saveSchedule()}>
              {schedule.starting ? <Loader2 className="animate-spin" /> : <Save />}{t("db_schedule_save")}
            </Button>
            {dirty && <Button variant="outline" disabled={schedule.busy} onClick={() => { setTime(settings?.dbBackupTime ?? ""); setKeep(settings?.dbBackupKeep ? String(settings.dbBackupKeep) : ""); }}>{t("platform_discard")}</Button>}
          </div>
          {schedule.error && <MessageLine kind="error" message={schedule.error} />}
          {schedule.task && <HostTaskStatus task={schedule.task} />}
        </section>
      </CardContent>
      <ConfirmDialog
        open={restore !== null}
        onOpenChange={(open) => { if (!open) setRestore(null); }}
        title={t("db_restore_title")}
        description={t("db_restore_description")}
        confirmLabel={t("db_bundle_restore")}
        confirmDisabled={typed.trim() !== restore}
        onConfirm={async () => {
          if (restore) await db.start({ action: "db-restore", params: { bundle: restore }, confirmMaintenance: true });
        }}
      >
        <div className="space-y-2">
          <Label htmlFor="db-restore-confirm">{t("db_restore_type", { name: restore ?? "" })}</Label>
          <Input id="db-restore-confirm" value={typed} autoComplete="off" spellCheck={false} onChange={(e) => setTyped(e.target.value)} placeholder={restore ?? ""} className="font-mono text-xs" />
        </div>
      </ConfirmDialog>
      <ConfirmDialog
        open={remove !== null}
        onOpenChange={(open) => { if (!open) setRemove(null); }}
        title={t("db_delete_title")}
        description={t("db_delete_description", { name: remove ?? "" })}
        confirmLabel={t("db_bundle_delete")}
        confirmVariant="destructive"
        onConfirm={async () => {
          if (remove) await db.start({ action: "db-delete", params: { bundle: remove }, confirmMaintenance: false });
        }}
      />
      {db.reauth.dialog}
      {schedule.reauth.dialog}
    </Card>
  );
}
