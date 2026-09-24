import { useParams } from "react-router-dom";
import { Users } from "lucide-react";
import { useTranslation } from "react-i18next";
import { BackLink } from "@/components/BackLink";
import { PhaseBadge } from "@/components/PhaseBadge";
import { Loading, ErrorState, NotYours, NotRunning } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { OnlineSection } from "@/components/players/OnlineSection";
import { WhitelistSection } from "@/components/players/WhitelistSection";
import { BansSection } from "@/components/players/BansSection";
import { api } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage, ownershipPending } from "@/lib/ownership";
import type { Phase } from "@/lib/types";

/** ServerPlayers is the per-server player-management subpage (/servers/:name/players):
 *  whitelist today, online roster and bans as they land. It owns its own gating —
 *  ownership (from /me/servers, since GET status never carries `owned`) and server
 *  readiness — because as a route it can be reached directly, not just from a link. */
export function ServerPlayers() {
  const { name = "" } = useParams();
  const { t } = useTranslation("servers");
  const { isAdmin, loading: tierLoading } = useTier();
  const { data, error, loading, reload } = useAsync(() => api.status(name), [name]);
  const {
    data: mine,
    error: mineError,
    reload: reloadMine,
  } = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name],
  );

  const back = (
    <BackLink to={`/servers/${name}`} label={t("players_back_to_console")} />
  );

  if (loading && !data) {
    return (
      <>
        {back}
        <Loading />
      </>
    );
  }
  if (error) {
    return (
      <>
        {back}
        <ErrorState error={error} onRetry={reload} />
      </>
    );
  }
  if (!data) return back;

  // Ownership is still resolving (tier fetch, or /me/servers for a non-admin). We
  // have the server's identity, so show its header with a spinner beneath it rather
  // than flashing the whole management surface at someone who may not own it. If the
  // /me/servers read itself failed, `mineError` breaks the pending state (below) so a
  // real owner sees a retry instead of an eternal spinner — never fail closed to
  // NotYours, which would wrongly tell an owner the server isn't theirs on a blip.
  const pending = ownershipPending(tierLoading, isAdmin, mine, mineError);
  const owned = canManage(isAdmin, mine, name);
  const phase: Phase = data.phase;

  const header = (
    <PageHeader
      icon={Users}
      title={data.displayName || data.name}
      subtitle={t("players_title")}
      actions={<PhaseBadge phase={phase} />}
    />
  );

  return (
    <>
      {back}
      {header}
      {pending ? (
        <Loading />
      ) : mineError ? (
        <ErrorState error={mineError} onRetry={reloadMine} />
      ) : !owned ? (
        <NotYours title={t("players_not_yours_title")} body={t("players_not_yours_body")} />
      ) : phase !== "Running" ? (
        <NotRunning title={t("players_not_running_title")} body={t("players_not_running_body")} wakeLabel={t("wake")} onWake={() => api.wake(name).then(reload)} />
      ) : (
        <div className="space-y-4">
          {/* Ordered as a who-may-be-here gradient: who is on right now → who may
              join → who may NOT. Each collapses to an index row (shared.tsx). */}
          <OnlineSection name={name} />
          <WhitelistSection name={name} />
          <BansSection name={name} />
        </div>
      )}
    </>
  );
}
