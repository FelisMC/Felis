import type { ReactNode } from "react";
import { Pencil } from "lucide-react";
import { useTranslation } from "react-i18next";
import { EditServerDialog, idleLabel, policyOptions, softwareSummary } from "@/components/EditServerDialog";
import { ExtraPortsCard } from "@/components/ExtraPortsCard";
import { ForwardingDialog } from "@/components/ForwardingDialog";
import { DangerZone } from "@/components/Retirement";
import { EmptyState } from "@/components/States";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { formatBytes } from "@/lib/format";
import { memoryLimit } from "@/lib/resourceUsage";
import { useTier } from "@/lib/tier";
import type { AutostartPolicy } from "@/lib/types";
import { useServer } from "@/pages/ServerLayout";

/** ServerSettings shows a server's configuration as it stands, with the edits and the danger zone beside it. */
export function ServerSettings() {
  const { name, data, error, reload, owned } = useServer();
  const { isAdmin, isOwner } = useTier();
  const { t } = useTranslation("servers");
  const system = name === "login" || name === "lobby";
  const editable = isAdmin || owned;
  const forwarding = isOwner && !system;

  if (!editable && !forwarding) {
    return <EmptyState title={t("settings_none_title")} hint={t("settings_none_hint")} />;
  }

  const cores = data.cpu ? (data.cpu.endsWith("m") ? Number(data.cpu.slice(0, -1)) / 1000 : Number(data.cpu)) : null;
  const bytes = (value?: string) => {
    const n = memoryLimit(value);
    return n ? formatBytes(n) : "—";
  };
  const policy = policyOptions(t).find((p) => p.value === data.autostartPolicy)?.label ?? data.autostartPolicy;

  return (
    <div className="space-y-6">
      {editable && (
        <Section
          title={t("settings_config_title")}
          description={t("settings_config_desc")}
          action={
            <EditServerDialog
              serverName={name}
              currentPhase={data.phase}
              mode={isAdmin ? "admin" : "owner"}
              systemService={system}
              currentDisplayName={data.displayName}
              currentTags={data.tags}
              currentPolicy={data.autostartPolicy as AutostartPolicy}
              currentImage={data.image}
              currentMemory={data.memory}
              currentStorage={data.storageSize}
              currentCpu={data.cpu}
              currentIdleStopSeconds={data.idleStopSeconds}
              playerCountUnknown={data.playerCountUnknown}
              currentSoftware={data.software}
              currentResume={data.resumeAfterHostRestart}
              currentCrashRestart={data.restartOnCrash}
              currentMaxCrashRestarts={data.maxCrashRestarts}
              currentNotify={data.notify}
              currentExtraPorts={data.extraPorts}
              currentExtraPortsPending={data.extraPortsPending}
              onUpdated={reload}
              trigger={<Button size="sm" variant="outline"><Pencil />{t("edit_server_title")}</Button>}
            />
          }
        >
          <Item label={t("settings_display_name")}>{data.displayName || data.name}</Item>
          <Item label={t("create_server_policy")}>{policy}</Item>
          <Item label={t("edit_server_idle")}>{idleLabel(t, data.idleStopSeconds ?? 0)}</Item>
          <Item label={t("settings_crash_restart")}>
            {data.restartOnCrash ? t("settings_crash_restart_on", { count: data.maxCrashRestarts ?? 0 }) : t("settings_off")}
          </Item>
          <Item label={t("settings_cpu")}>{cores === null || !Number.isFinite(cores) ? t("settings_unlimited") : t("settings_cpu_value", { cores })}</Item>
          <Item label={t("settings_memory")}>{bytes(data.memory)}</Item>
          <Item label={t("settings_storage")}>{bytes(data.storageSize)}</Item>
          {data.software ? (
            <Item label={t("edit_server_software")}>{softwareSummary(t, data.software)} · {t("software_java_option", { version: data.software.java })}</Item>
          ) : data.image && (
            <Item label={t("create_server_image")}><span className="break-all font-mono text-xs font-normal">{data.image}</span></Item>
          )}
          {!system && (
            <Item label={t("edit_server_tags")}>
              {data.tags && data.tags.length > 0 ? (
                <span className="flex flex-wrap gap-1.5">
                  {data.tags.map((tag) => <span key={tag} className="rounded-md bg-muted px-2 py-0.5 text-xs font-normal">{tag}</span>)}
                </span>
              ) : <span className="font-normal text-muted-foreground">{t("settings_none")}</span>}
            </Item>
          )}
        </Section>
      )}

      {forwarding && (
        <Section
          title={t("settings_network_title")}
          description={t("settings_network_desc")}
          action={<ForwardingDialog serverName={name} legacy={!!data.legacyForwarding} onUpdated={reload} trigger={<Button size="sm" variant="outline">{t("settings_change_forwarding")}</Button>} />}
        >
          <Item label={t("forwarding_title")}>{t(data.legacyForwarding ? "forwarding_legacy" : "forwarding_modern")}</Item>
        </Section>
      )}

      {owned && <ExtraPortsCard ports={data.extraPorts ?? []} pending={data.extraPortsPending} />}

      {owned && (
        <DangerZone
          name={name}
          label={data.displayName || data.name}
          isAdmin={isAdmin}
          isOwner={isOwner}
          canKill={!error && (data.phase !== "Stopped" || data.desiredState === "Running")}
          canRetire={!data.reaperExempt && !data.retiring}
          onChanged={reload}
        />
      )}
    </div>
  );
}

function Section({ title, description, action, children }: { title: string; description?: string; action?: ReactNode; children: ReactNode }) {
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-3 py-4">
        <div className="min-w-0 space-y-1">
          <CardTitle>{title}</CardTitle>
          {description && <p className="text-xs text-muted-foreground">{description}</p>}
        </div>
        {action}
      </CardHeader>
      <CardContent>
        <dl className="grid grid-cols-2 gap-x-6 gap-y-5 md:grid-cols-3 xl:grid-cols-4">{children}</dl>
      </CardContent>
    </Card>
  );
}

function Item({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0 space-y-1">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="break-words text-sm font-medium">{children}</dd>
    </div>
  );
}
