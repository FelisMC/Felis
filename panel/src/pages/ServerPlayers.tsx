import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Moon, ShieldX, Users } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { PhaseBadge } from "@/components/PhaseBadge";
import { Loading, ErrorState } from "@/components/States";
import { OnlineSection } from "@/components/players/OnlineSection";
import { WhitelistSection } from "@/components/players/WhitelistSection";
import { BansSection } from "@/components/players/BansSection";
import { api } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import type { Phase } from "@/lib/types";

/** NotYours is the explicit "this server isn't yours to manage" state. Player
 *  management is owner-or-admin gated on the backend, but this page is a real route:
 *  a non-owner (or anyone who types the URL) reaches it, so it must say so plainly
 *  and offer a way back — never render blank. */
function NotYours() {
  const { t } = useTranslation("servers");
  return (
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-24 text-center">
      <ShieldX className="h-8 w-8 text-destructive" />
      <div>
        <p className="font-medium">{t("players_not_yours_title")}</p>
        <p className="mt-1 text-sm text-muted-foreground">{t("players_not_yours_body")}</p>
      </div>
      <Link to="/servers" className="text-sm font-medium text-primary hover:underline">
        {t("my_servers_breadcrumb")}
      </Link>
    </div>
  );
}

/** NotRunning is the page-level asleep state. Every section here needs a live RCON
 *  connection, so a stopped server has nothing to manage — the whole page collapses
 *  to one honest "wake it first" panel with the wake control, rather than three
 *  separately-empty sections. */
function NotRunning({ onWake }: { onWake: () => void }) {
  const { t } = useTranslation("servers");
  return (
    <div className="flex flex-col items-center justify-center gap-4 rounded-lg border border-dashed border-border bg-muted/20 py-16 text-center">
      <Moon className="h-8 w-8 text-muted-foreground/70" />
      <div>
        <p className="font-medium">{t("players_not_running_title")}</p>
        <p className="mx-auto mt-1 max-w-sm text-sm text-muted-foreground">
          {t("players_not_running_body")}
        </p>
      </div>
      <Button size="sm" onClick={onWake}>
        {t("wake")}
      </Button>
    </div>
  );
}

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
    <Link
      to={`/servers/${name}`}
      className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
    >
      <ArrowLeft className="h-4 w-4" /> {t("players_back_to_console")}
    </Link>
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
  const ownershipPending =
    tierLoading || (!isAdmin && mine === null && !mineError);
  const owned =
    isAdmin || (mine ?? []).some((s) => s.name === name && s.owned === true);
  const phase: Phase = data.phase;

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-3">
        <Users className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">
            {data.displayName || data.name}
          </h1>
          <p className="text-sm text-muted-foreground">{t("players_title")}</p>
        </div>
      </div>
      <PhaseBadge phase={phase} />
    </div>
  );

  return (
    <>
      {back}
      {header}
      {ownershipPending ? (
        <Loading />
      ) : mineError ? (
        <ErrorState error={mineError} onRetry={reloadMine} />
      ) : !owned ? (
        <NotYours />
      ) : phase !== "Running" ? (
        <NotRunning onWake={() => api.wake(name).then(reload)} />
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
