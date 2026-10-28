import { useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { shownPhase, startFailure } from "@/components/PhaseBadge";
import { Loading, ErrorState, NotYours, NotRunning, RefreshError } from "@/components/States";
import { TabHeader } from "@/components/TabHeader";
import { OnlineSection } from "@/components/players/OnlineSection";
import { WhitelistSection } from "@/components/players/WhitelistSection";
import { BansSection } from "@/components/players/BansSection";
import { WakeListSection } from "@/components/players/WakeListSection";
import { api } from "@/lib/api";
import { STATUS_POLL_SLOW_MS, STATUS_POLL_FAST_MS, useAsync, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage, ownershipPending } from "@/lib/ownership";

// Direct navigation still requires ownership and readiness checks.
export function ServerPlayers() {
  const { name = "" } = useParams();
  const { t } = useTranslation("servers");
  const { isAdmin, loading: tierLoading } = useTier();
  const { data, error, loading, reload } = useAsync(() => api.status(name), [name]);
  // The poll is what switches this page over once a wake lands, and back to
  // NotRunning when the server stops (idle stop, the console, a player's timeout).
  usePolling(reload, data && shownPhase(data) === "Running" ? STATUS_POLL_SLOW_MS : STATUS_POLL_FAST_MS);
  const {
    data: mine,
    error: mineError,
    reload: reloadMine,
  } = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name],
  );


  if (loading && !data) {
    return (
      <>
        <Loading />
      </>
    );
  }
  if (error && !data) {
    return (
      <>
        <ErrorState error={error} onRetry={reload} />
      </>
    );
  }
  if (!data) return null;

  // Failed ownership reads offer retry rather than claiming the viewer does not own the server.
  const pending = ownershipPending(tierLoading, isAdmin, mine, mineError);
  const owned = canManage(isAdmin, mine, name);
  const phase = shownPhase(data);
  const failure = startFailure(data);

  const header = <TabHeader description={t("servers:players_link_desc")} />;

  return (
    <>
      {header}
      {error && <RefreshError error={error} className="mb-4" />}
      {pending ? (
        <Loading />
      ) : mineError ? (
        <ErrorState error={mineError} onRetry={reloadMine} />
      ) : !owned ? (
        <NotYours title={t("players_not_yours_title")} body={t("players_not_yours_body")} />
      ) : phase !== "Running" ? (
        <div className="space-y-4">
          <NotRunning
            title={t("players_not_running_title")}
            body={t("players_not_running_body")}
            serverName={name}
            phase={data.phase}
            desiredState={data.desiredState}
            failure={failure}
            autoRestarts={data.autoRestarts}
            retiring={data.retiring}
            diskFull={data.diskFull}
            onWoken={reload}
          />
          {/* The wake list is Felis's own record, so it stays manageable while the
              server sleeps, which is when it decides who may start it. */}
          <WakeListSection name={name} policy={data.autostartPolicy} defaultOpen />
        </div>
      ) : (
        <div className="space-y-4">
          {/* Ordered as a who-may-be-here gradient: who is on right now → who may
              join → who may NOT, then who may wake it while it sleeps. Each
              collapses to an index row (shared.tsx). */}
          <OnlineSection name={name} />
          <WhitelistSection name={name} />
          <BansSection name={name} />
          <WakeListSection name={name} policy={data.autostartPolicy} />
        </div>
      )}
    </>
  );
}
