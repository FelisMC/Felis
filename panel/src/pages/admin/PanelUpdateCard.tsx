import { useId, useState } from "react";
import { AlertTriangle, ArrowUpCircle, ExternalLink, Loader2, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { HostTaskNotice, HostTaskStatus, useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";
import { api } from "@/lib/api";
import { useAsync, useRefreshSignal } from "@/lib/hooks";

/** PanelUpdateCard offers the host's pending Felis update and follows it to the end. */
export function PanelUpdateCard({ inWindow }: { inWindow: boolean }) {
  const { t, i18n } = useTranslation("admin");
  const titleId = useId();
  const report = useAsync(() => api.getUpdateReport(), []);
  useRefreshSignal(report.reload);
  const host = useHostTask(["update"]);
  const [confirming, setConfirming] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);
  const [startedId, setStartedId] = useState<string | null>(null);

  const felis = report.data?.report?.components.find((c) => c.name === "felis-api" && c.state === "available" && c.latest);
  const task = host.task;
  const shown =
    task && (task.state === "running" || task.id === startedId || (felis && task.request.params?.version === felis.latest))
      ? task
      : null;
  if (!felis && !shown) return null;

  const target = felis?.latest ?? shown?.request.params?.version ?? "";
  const updated = shown?.state === "succeeded";
  const reconnecting = host.running && (!!host.listError || !!host.detailError);
  const rollback = `https://docs.felismc.com${i18n.language.startsWith("zh") ? "" : "/en"}/operations/upgrade#rollback`;

  return (
    <Card role="region" aria-labelledby={titleId}>
      <CardHeader>
        <CardTitle id={titleId} className="flex items-center gap-2">
          <ArrowUpCircle className="h-4 w-4 text-primary" aria-hidden="true" />
          {t("update_now_title", { version: target })}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        {felis && (
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
            <dt className="text-muted-foreground">{t("update_now_current")}</dt>
            <dd className="font-mono">{felis.current ?? "—"}</dd>
            <dt className="text-muted-foreground">{t("update_now_target")}</dt>
            <dd className="font-mono">{felis.latest}</dd>
          </dl>
        )}
        {felis?.note && <p className="text-xs leading-relaxed text-muted-foreground">{felis.note}</p>}
        <p className="leading-relaxed text-muted-foreground">{t("update_now_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        {felis && !updated && (
          <Button
            disabled={!host.available || host.busy}
            onClick={() => {
              setAcknowledged(false);
              setConfirming(true);
            }}
          >
            {host.running || host.starting ? <Loader2 className="animate-spin" /> : <ArrowUpCircle />}
            {t("update_now_button")}
          </Button>
        )}
        {host.error && <MessageLine kind="error" message={host.error} />}
        {reconnecting && (
          <p role="status" className="flex items-center gap-2 text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            {t("update_now_reconnecting")}
          </p>
        )}
        {shown && <HostTaskStatus task={shown} />}
        {updated && (
          <div className="space-y-3">
            <MessageLine kind="success" message={t("update_now_done", { version: target })} />
            <p className="text-xs text-muted-foreground">{t("update_now_reload_hint")}</p>
            <Button onClick={() => window.location.reload()}>
              <RefreshCw />
              {t("update_now_reload")}
            </Button>
          </div>
        )}
        {shown?.state === "failed" && (
          <div className="space-y-2 rounded-md border border-border p-3 text-xs leading-relaxed">
            <p>{t("update_now_rollback")}</p>
            <a href={rollback} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">
              {t("update_now_rollback_link")}
              <ExternalLink className="h-3 w-3" aria-hidden="true" />
            </a>
          </div>
        )}
      </CardContent>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={t("update_now_confirm_title", { version: target })}
        description={t("update_now_description")}
        confirmLabel={t("update_now_confirm_btn")}
        confirmVariant="default"
        confirmDisabled={!inWindow && !acknowledged}
        onConfirm={async () => {
          const created = await host.start({
            action: "update",
            params: { version: target, now: inWindow ? "false" : "true" },
            confirmMaintenance: true,
          });
          if (created) setStartedId(created.id);
        }}
      >
        {!inWindow && (
          <div className="grid gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs">
            <p className="flex items-start gap-1.5 text-amber-700 dark:text-amber-300">
              <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
              {t("update_now_outside_window")}
            </p>
            <label className="flex cursor-pointer items-center gap-2 font-medium text-foreground">
              <Checkbox checked={acknowledged} onChange={(e) => setAcknowledged(e.target.checked)} />
              {t("update_now_outside_confirm")}
            </label>
          </div>
        )}
      </ConfirmDialog>
      {host.reauth.dialog}
    </Card>
  );
}
