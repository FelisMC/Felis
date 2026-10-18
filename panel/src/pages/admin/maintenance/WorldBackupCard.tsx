import { useState } from "react";
import { Loader2, Play, Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { HostTaskNotice, HostTaskStatus, useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useRefreshSignal } from "@/lib/hooks";

// Live backup disables saving only during world copying.
export function WorldBackupCard() {
  const { t } = useTranslation("admin");
  const host = useHostTask(["backup-now"]);
  const fleet = useAsync(api.fleet, []);
  useRefreshSignal(fleet.reload);
  const servers = (fleet.data ?? []).filter((s) => !s.system);
  const [all, setAll] = useState(true);
  const [chosen, setChosen] = useState<string[]>([]);
  const [stop, setStop] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const ready = all || chosen.length > 0;
  function request() {
    const params: Record<string, string> = {};
    if (stop) params.stop = "true";
    if (!all) params.servers = chosen.join(",");
    return host.start({ action: "backup-now", params, confirmMaintenance: stop });
  }
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><Save className="h-4 w-4 text-primary" aria-hidden="true" />{t("world_backup_title")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("world_backup_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <div className="flex flex-wrap gap-2" role="radiogroup" aria-label={t("world_backup_scope")}>
          {[true, false].map((value) => (
            <Button key={String(value)} type="button" role="radio" aria-checked={all === value} variant="outline" size="sm" disabled={host.busy}
              className={all === value ? "border-primary/40 bg-primary/5 text-primary" : ""} onClick={() => setAll(value)}>
              {t(value ? "world_backup_all" : "world_backup_some")}
            </Button>
          ))}
        </div>
        {!all && (
          <fieldset className="space-y-2">
            <legend className="sr-only">{t("world_backup_some")}</legend>
            {fleet.error != null && <MessageLine kind="error" message={humanizeError(fleet.error)} />}
            {servers.length === 0 && !fleet.loading && <p className="text-sm text-muted-foreground">{t("world_backup_no_servers")}</p>}
            <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
              {servers.map((s) => (
                <label key={s.name} className="flex items-center gap-2 rounded-md border border-border px-3 py-2 text-sm">
                  <Checkbox checked={chosen.includes(s.name)} disabled={host.busy}
                    onChange={(e) => { const on = e.target.checked; setChosen((list) => on ? [...list, s.name] : list.filter((n) => n !== s.name)); }} />
                  <span className="truncate">{s.displayName || s.name}</span>
                  <span className="ml-auto truncate font-mono text-[11px] text-muted-foreground">{s.name}</span>
                </label>
              ))}
            </div>
          </fieldset>
        )}
        <div className="flex items-start justify-between gap-4 rounded-lg border border-border p-4">
          <div className="space-y-1">
            <Label htmlFor="world-backup-stop">{t("world_backup_stop")}</Label>
            <p className="text-xs leading-relaxed text-muted-foreground">{t("world_backup_stop_hint")}</p>
          </div>
          <Switch id="world-backup-stop" checked={stop} disabled={host.busy} onCheckedChange={setStop} />
        </div>
        <Button disabled={!host.available || host.busy || !ready} onClick={() => (stop ? setConfirming(true) : void request())}>
          {host.starting ? <Loader2 className="animate-spin" /> : <Play />}{t("world_backup_start")}
        </Button>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={t("world_backup_stop_title")}
        description={t("world_backup_stop_confirm")}
        confirmLabel={t("world_backup_start")}
        onConfirm={async () => { await request(); }}
      />
      {host.reauth.dialog}
    </Card>
  );
}
