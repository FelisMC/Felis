import { lazy, Suspense, useMemo } from "react";
import { Link } from "react-router-dom";
import { Activity, Server, Play, Hand } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { api } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";
import type { Phase, ServerInfo } from "@/lib/types";

// three.js is heavy and only the Dashboard renders it — split it into its own
// async chunk so the rest of the panel doesn't pay for it on first load.
const VoxelFleet = lazy(() =>
  import("@/components/VoxelFleet").then((m) => ({ default: m.VoxelFleet })),
);

function Stat({
  icon: Icon,
  label,
  value,
}: {
  icon: typeof Server;
  label: string;
  value: number | string;
}) {
  return (
    <Card>
      <CardContent className="flex items-center gap-3 p-4">
        <div className="rounded-md bg-primary/15 p-2 text-primary">
          <Icon className="h-5 w-5" />
        </div>
        <div>
          <div className="text-2xl font-semibold leading-none">{value}</div>
          <div className="mt-1 text-xs text-muted-foreground">{label}</div>
        </div>
      </CardContent>
    </Card>
  );
}

export function Dashboard() {
  const cfg = useConfig();
  const { data, error, loading, reload } = useAsync(() => api.myServers(), []);
  const { t } = useTranslation("dashboard");

  const counts = useMemo(() => {
    const servers = data ?? [];
    const by = (p: Phase) => servers.filter((s) => s.phase === p).length;
    return {
      total: servers.length,
      running: by("Running"),
      players: servers.reduce((n, s) => n + (s.players ?? 0), 0),
    };
  }, [data]);

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t("title")}</h1>
        <p className="text-sm text-muted-foreground">{t("subtitle")}</p>
      </div>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : (
        <FleetView servers={data ?? []} counts={counts} hasConfig={!!cfg} />
      )}
    </div>
  );
}

function FleetView({
  servers,
  counts,
  hasConfig,
}: {
  servers: ServerInfo[];
  counts: { total: number; running: number; players: number };
  hasConfig: boolean;
}) {
  const { t } = useTranslation("dashboard");
  if (servers.length === 0) {
    return (
      <EmptyState
        title={t("no_servers_title")}
        hint={t("no_servers_hint")}
      >
        <Link to="/servers">
          <Button>{t("go_to_my_servers")}</Button>
        </Link>
      </EmptyState>
    );
  }

  return (
    <>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <Stat icon={Server} label={t("stat_servers")} value={counts.total} />
        <Stat icon={Play} label={t("stat_running")} value={counts.running} />
        <Stat icon={Activity} label={t("stat_players_online")} value={counts.players} />
      </div>

      <Card className="overflow-hidden">
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <CardTitle className="text-base">{t("fleet")}</CardTitle>
          <Link to="/servers">
            <Button variant="ghost" size="sm">
              {t("manage")} <Hand className="h-3.5 w-3.5" />
            </Button>
          </Link>
        </CardHeader>
        <CardContent className="p-0">
          {hasConfig ? (
            <div className="h-[340px] w-full bg-gradient-to-b from-transparent to-primary/5">
              <Suspense fallback={<Loading label={t("loading_scene")} />}>
                <VoxelFleet servers={servers} />
              </Suspense>
            </div>
          ) : (
            <Loading label={t("preparing_scene")} />
          )}
        </CardContent>
      </Card>
    </>
  );
}
