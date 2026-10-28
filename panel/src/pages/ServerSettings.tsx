import { useTranslation } from "react-i18next";
import { EditServerDialog } from "@/components/EditServerDialog";
import { ExtraPortsCard } from "@/components/ExtraPortsCard";
import { ForwardingDialog } from "@/components/ForwardingDialog";
import { RetireCard } from "@/components/Retirement";
import { SoftwareCard } from "@/components/SoftwareCard";
import { EmptyState } from "@/components/States";
import { useTier } from "@/lib/tier";
import type { AutostartPolicy } from "@/lib/types";
import { useServer } from "@/pages/ServerLayout";

/** ServerSettings gathers what configures a server; giving it up or deleting it sits last, apart. */
export function ServerSettings() {
  const { name, data, error, reload, owned } = useServer();
  const { isAdmin, isOwner } = useTier();
  const { t } = useTranslation("servers");
  const system = name === "login" || name === "lobby";
  const editable = isAdmin || owned;
  const forwarding = isOwner && !system;
  const retirable = owned && (isOwner || data.phase !== "Stopped" || (!data.reaperExempt && !data.retiring));

  if (!editable && !forwarding && !data.software) {
    return <EmptyState title={t("settings_none_title")} hint={t("settings_none_hint")} />;
  }
  return (
    <div className="space-y-6">
      <div className="grid gap-4 lg:grid-cols-2">
        <div className="space-y-4">
          {editable && (
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
            />
          )}
          {forwarding && <ForwardingDialog serverName={name} legacy={!!data.legacyForwarding} onUpdated={reload} />}
        </div>
        <div className="space-y-4">
          {data.software && <SoftwareCard software={data.software} />}
          {owned && <ExtraPortsCard ports={data.extraPorts ?? []} pending={data.extraPortsPending} />}
        </div>
      </div>
      {retirable && (
        <section aria-labelledby="server-danger-zone" className="space-y-3">
          <h2 id="server-danger-zone" className="text-sm font-medium text-destructive">{t("settings_danger_zone")}</h2>
          <RetireCard name={name} label={data.displayName || data.name} isAdmin={isAdmin} isOwner={isOwner} canKill={!error && (data.phase !== "Stopped" || data.desiredState === "Running")} canRetire={!data.reaperExempt && !data.retiring} onChanged={reload} />
        </section>
      )}
    </div>
  );
}
