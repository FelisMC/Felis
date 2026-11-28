import { useTranslation } from "react-i18next";
import { ForwardingDialog } from "@/components/ForwardingDialog";
import { DangerZone } from "@/components/Retirement";
import { ServerAccessCard } from "@/components/ServerAccessCard";
import { ServerConfigForm, SettingRow, SettingsSection } from "@/components/ServerConfigForm";
import { EmptyState } from "@/components/States";
import { Button } from "@/components/ui/button";
import { useFeatures } from "@/lib/features";
import { canDeleteServer } from "@/lib/ownership";
import { useTier } from "@/lib/tier";
import type { AutostartPolicy } from "@/lib/types";
import { useServer } from "@/pages/ServerLayout";

/** ServerSettings edits a server's parameters in place, with the danger zone below them. */
export function ServerSettings() {
  const { name, data, error, reload, canOperate, canManage, actualOwned } = useServer();
  const { isAdmin, isOwner } = useTier();
  const { features } = useFeatures();
  const { t } = useTranslation("servers");
  const system = name === "login" || name === "lobby";
  const teamMode = features.mode === "team";
  const editable = canManage;
  const forwarding = isOwner && !system;
  const showAccessCard = !system && (isOwner || (isAdmin && !data.protected));

  if (!editable && !forwarding && !showAccessCard) {
    return <EmptyState title={t("settings_none_title")} hint={t("settings_none_hint")} />;
  }

  const forwardingRow = forwarding && (
    <SettingRow label={t("forwarding_title")} hint={t("settings_network_desc")}>
      <div className="flex items-center justify-between gap-3 lg:justify-end">
        <span className="text-sm">{t(data.legacyForwarding ? "forwarding_legacy" : "forwarding_modern")}</span>
        <ForwardingDialog serverName={name} legacy={!!data.legacyForwarding} onUpdated={reload}
          trigger={<Button size="sm" variant="outline">{t("settings_change_forwarding")}</Button>} />
      </div>
    </SettingRow>
  );

  return (
    <div className="flex flex-col gap-6">
      {editable ? (
        <ServerConfigForm
          serverName={name}
          currentPhase={data.phase}
          mode={isAdmin && canManage ? "admin" : "owner"}
          systemService={system}
          currentDisplayName={data.displayName}
          currentTags={data.tags}
          currentPolicy={data.autostartPolicy as AutostartPolicy}
          currentImage={data.image}
          currentMemory={data.memory}
          currentStorage={data.storageSize}
          currentCpu={data.cpu}
          currentIdleStopSeconds={data.idleStopSeconds}
          currentIdleAction={data.idleAction}
          playerCountUnknown={data.playerCountUnknown}
          currentSoftware={data.software}
          currentResume={data.resumeAfterHostRestart}
          currentCrashRestart={data.restartOnCrash}
          currentMaxCrashRestarts={data.maxCrashRestarts}
          currentNotify={data.notify}
          currentExtraPorts={data.extraPorts}
          currentExtraPortsPending={data.extraPortsPending}
          onUpdated={reload}
          network={forwardingRow}
        />
      ) : forwardingRow && (
        <SettingsSection title={t("settings_network_title")}>{forwardingRow}</SettingsSection>
      )}

      {showAccessCard && (
        <ServerAccessCard
          serverName={name}
          isProtected={!!data.protected}
          isOwner={isOwner}
          onProtectionChanged={reload}
        />
      )}

      {canManage && (
        <DangerZone
          name={name}
          label={data.displayName || data.name}
          isAdmin={isAdmin}
          isOwner={isOwner}
          canKill={canOperate && !error && (data.phase !== "Stopped" || data.desiredState === "Running")}
          canRelease={!teamMode && !data.reaperExempt && !data.retiring && !data.protected && (isOwner || isAdmin || actualOwned)}
          canDelete={!data.reaperExempt && !data.retiring && canDeleteServer(isOwner, isAdmin, data.protected)}
          onChanged={reload}
        />
      )}
    </div>
  );
}
