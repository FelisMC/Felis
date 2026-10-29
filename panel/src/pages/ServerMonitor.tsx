import { useTranslation } from "react-i18next";
import { ResourceUsage } from "@/components/ResourceUsage";
import { ServerEventsCard } from "@/components/ServerEvents";
import { EmptyState } from "@/components/States";
import { TabHeader } from "@/components/TabHeader";
import { useServer } from "@/pages/ServerLayout";

/** ServerMonitor shows how hard the server works and what has happened to it lately. */
export function ServerMonitor() {
  const { name, data, error, owned } = useServer();
  const { t } = useTranslation("servers");
  const events = data.events ?? [];
  return (
    <div className="flex flex-col gap-4">
      <TabHeader description={t("monitor_link_desc")} />
      {owned && (
        <ResourceUsage
          key={name}
          name={name}
          running={!error && data.phase === "Running" && data.desiredState === "Running"}
          cpu={data.cpu}
          memory={data.memory}
          disk={error && data.disk ? { ...data.disk, stale: true } : data.disk}
          diskFull={data.diskFull}
        />
      )}
      {events.length > 0 ? <ServerEventsCard events={events} /> : <EmptyState title={t("events_empty_title")} hint={t("events_empty_hint")} />}
    </div>
  );
}
