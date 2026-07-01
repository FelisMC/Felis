import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  Network,
  Play,
  Square,
  Terminal,
  ExternalLink,
  Search,
  RefreshCw,
  Server,
  Users,
  AlertTriangle,
  UserRound,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from "@/components/ui/select";
import { PhaseBadge, PHASE_KEY, PHASE_COLOR } from "@/components/PhaseBadge";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { CreateServerDialog } from "@/components/CreateServerDialog";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { hostFor, type RuntimeConfig } from "@/lib/config";
import { matchScore } from "@/lib/fuzzy";
import type { AutostartPolicy, FleetServer, Phase, ServerInfo } from "@/lib/types";
import { cn } from "@/lib/utils";

const REFRESH_MS = 10_000;

const PHASES: Phase[] = [
  "Running",
  "Starting",
  "Stopping",
  "Stopped",
  "Failed",
  "Unknown",
];

const POLICY_KEY: Record<AutostartPolicy, string> = {
  ownerOnly: "policy_owneronly",
  public: "policy_public",
  allowlist: "policy_allowlist",
};

function isLive(phase: Phase): boolean {
  return phase === "Running" || phase === "Starting" || phase === "Stopping";
}

const PAGE_SIZE = 6;

interface UnifiedServer {
  name: string;
  subdomain: string;
  phase: Phase;
  ready: boolean;
  desiredState?: "Running" | "Stopped";
  autostartPolicy?: AutostartPolicy;
  playersOnline: number;
  playersMax: number;
  owner?: string;
  endpointAddress?: string | null;
  claimable?: boolean;
  owned?: boolean;
}

function Stat({
  icon: Icon,
  label,
  value,
  accent,
}: {
  icon: typeof Server;
  label: string;
  value: number | string;
  accent?: string;
}) {
  return (
    <Card>
      <CardContent className="flex items-center gap-3 p-4">
        <div
          className="rounded-md p-2"
          style={{
            backgroundColor: accent ? `${accent}26` : undefined,
            color: accent,
          }}
        >
          <Icon className="h-5 w-5" />
        </div>
        <div className="min-w-0">
          <div className="text-2xl font-semibold leading-none tabular-nums text-left">{value}</div>
          <div className="mt-1.5 truncate text-xs text-muted-foreground text-left">{label}</div>
        </div>
      </CardContent>
    </Card>
  );
}

export function ServersPage() {
  const { t } = useTranslation(["ops", "servers"]);
  const { isAdmin } = useTier();
  const cfg = useConfig();

  const fetchFn = useMemo(() => (isAdmin ? api.fleet : api.myServers), [isAdmin]);
  const { data, error, loading, reload } = useAsync(fetchFn, [fetchFn]);

  const [query, setQuery] = useState("");
  const [phaseFilter, setPhaseFilter] = useState<Phase | "all">("all");
  const [page, setPage] = useState(1);

  useEffect(() => {
    const id = window.setInterval(() => {
      if (!document.hidden) reload();
    }, REFRESH_MS);
    const onVisible = () => {
      if (!document.hidden) reload();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [reload]);

  const servers = useMemo<UnifiedServer[]>(() => {
    if (!data) return [];
    if (isAdmin) {
      return (data as FleetServer[]).map((s) => ({
        name: s.name,
        subdomain: s.subdomain,
        phase: s.phase,
        ready: s.ready,
        desiredState: s.desiredState,
        autostartPolicy: s.autostartPolicy,
        playersOnline: s.playersOnline,
        playersMax: s.playersMax,
        owner: s.owner,
        endpointAddress: s.endpointAddress,
      }));
    } else {
      return (data as ServerInfo[]).map((s) => ({
        name: s.name,
        subdomain: s.subdomain,
        phase: s.phase,
        ready: s.phase === "Running",
        desiredState: s.desiredState,
        autostartPolicy: s.autostartPolicy,
        playersOnline: s.players ?? 0,
        playersMax: s.maxPlayers ?? 0,
        owner: s.owned ? t("servers:owned_filter_mine") || "me" : undefined,
        claimable: s.claimable,
        owned: s.owned,
      }));
    }
  }, [data, isAdmin, t]);

  const stats = useMemo(() => {
    const counts: Record<Phase, number> = {
      Running: 0,
      Starting: 0,
      Stopping: 0,
      Stopped: 0,
      Failed: 0,
      Unknown: 0,
    };
    let playersOnline = 0;
    let playersMax = 0;
    for (const s of servers) {
      if (counts[s.phase as Phase] !== undefined) counts[s.phase as Phase]++;
      else counts.Unknown++;
      playersOnline += s.playersOnline ?? 0;
      playersMax += s.playersMax ?? 0;
    }
    return { counts, playersOnline, playersMax, total: servers.length };
  }, [servers]);

  const visible = useMemo(() => {
    const terms = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
    const phaseOk = (s: UnifiedServer) => phaseFilter === "all" || s.phase === phaseFilter;
    if (terms.length === 0) return servers.filter(phaseOk);
    const scored: { s: UnifiedServer; score: number }[] = [];
    for (const s of servers) {
      if (!phaseOk(s)) continue;
      const score = matchScore([s.name, s.subdomain ?? "", s.owner ?? ""], terms);
      if (score >= 0) scored.push({ s, score });
    }
    scored.sort((a, b) => b.score - a.score);
    return scored.map((x) => x.s);
  }, [servers, query, phaseFilter]);

  const totalPages = Math.max(1, Math.ceil(visible.length / PAGE_SIZE));
  const safePage = Math.min(page, totalPages);
  const paged = visible.slice((safePage - 1) * PAGE_SIZE, safePage * PAGE_SIZE);

  const showInitialLoading = loading && !data;
  const showInitialError = !!error && !data;

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex items-center gap-3">
          <Network className="h-6 w-6 text-primary" />
          <div className="text-left">
            <h1 className="text-2xl font-semibold tracking-tight">
              {isAdmin ? t("fleet_title") : t("servers:my_servers_title")}
            </h1>
            <p className="text-sm text-muted-foreground">
              {isAdmin ? t("fleet_subtitle") : t("servers:my_servers_subtitle")}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-3">
          {isAdmin && cfg && <CreateServerDialog cfg={cfg} onCreated={reload} />}
          <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="h-1.5 w-1.5 rounded-full bg-[#22c55e] animate-pulse" />
            {t("fleet_live")}
          </span>
          <Button
            variant="outline"
            size="sm"
            onClick={reload}
            disabled={loading}
            title={t("fleet_refresh")}
          >
            <RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} />
          </Button>
        </div>
      </div>

      {showInitialLoading ? (
        <Loading />
      ) : showInitialError ? (
        <ErrorState error={error} onRetry={reload} />
      ) : !cfg ? (
        <Loading />
      ) : (
        <>
          {error && (
            <div className="flex items-center gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive">
              <AlertTriangle className="h-3.5 w-3.5 shrink-0" />
              {humanizeError(error)}
            </div>
          )}

          {/* Stats Cards in a full grid row */}
          <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
            <Stat icon={Server} label={t("fleet_stat_total")} value={stats.total} />
            <Stat
              icon={Play}
              label={t("fleet_stat_running")}
              value={stats.counts.Running}
              accent={PHASE_COLOR.Running}
            />
            <Stat
              icon={Users}
              label={t("fleet_stat_players")}
              value={`${stats.playersOnline} / ${stats.playersMax}`}
            />
            <Stat
              icon={AlertTriangle}
              label={t("fleet_stat_failed")}
              value={stats.counts.Failed}
              accent={stats.counts.Failed > 0 ? PHASE_COLOR.Failed : undefined}
            />
          </div>

          {/* Distribution card in a full grid row */}
          {stats.total > 0 && (
            <Card>
              <CardContent className="space-y-3 p-4">
                <div className="text-xs font-medium text-muted-foreground text-left">
                  {t("fleet_distribution")}
                </div>
                <div className="flex h-2.5 w-full overflow-hidden rounded-full bg-secondary">
                  {PHASES.map((p) =>
                    stats.counts[p] > 0 ? (
                      <div
                        key={p}
                        className="h-full transition-all"
                        style={{
                          width: `${(stats.counts[p] / stats.total) * 100}%`,
                          backgroundColor: PHASE_COLOR[p],
                        }}
                      />
                    ) : null,
                  )}
                </div>
                <div className="flex flex-wrap gap-x-4 gap-y-1.5">
                  {PHASES.filter((p) => stats.counts[p] > 0).map((p) => (
                    <span key={p} className="inline-flex items-center gap-1.5 text-xs">
                      <span
                        className="h-2 w-2 rounded-full"
                        style={{ backgroundColor: PHASE_COLOR[p] }}
                      />
                      <span className="text-muted-foreground">{t(PHASE_KEY[p])}</span>
                      <span className="font-mono font-semibold text-foreground">
                        {stats.counts[p]}
                      </span>
                    </span>
                  ))}
                </div>
              </CardContent>
            </Card>
          )}

          {/* Independent Search & Filter bar */}
          <div className="flex flex-wrap items-center gap-3">
            <div className="relative min-w-[14rem] flex-1">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value);
                  setPage(1);
                }}
                placeholder={isAdmin ? t("fleet_search_placeholder") : t("servers:search_placeholder")}
                className="pl-9"
              />
            </div>
            <Select
              value={phaseFilter}
              onValueChange={(v) => {
                setPhaseFilter(v as Phase | "all");
                setPage(1);
              }}
            >
              <SelectTrigger className="w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t("fleet_filter_all")}</SelectItem>
                {PHASES.map((p) => (
                  <SelectItem key={p} value={p}>
                    {t(PHASE_KEY[p])}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <span className="shrink-0 text-xs text-muted-foreground">
              {visible.length === servers.length
                ? t("fleet_count", { count: servers.length })
                : t("fleet_count_filtered", {
                    shown: visible.length,
                    total: servers.length,
                  })}
            </span>
          </div>

          {servers.length === 0 ? (
            <EmptyState
              title={isAdmin ? t("fleet_empty_title") : t("servers:no_servers_linked")}
              hint={isAdmin ? t("fleet_empty_hint") : t("servers:no_servers_hint")}
            />
          ) : visible.length === 0 ? (
            <EmptyState
              title={isAdmin ? t("fleet_no_match_title") : t("servers:search_no_match")}
              hint={isAdmin ? t("fleet_no_match_hint") : t("servers:search_clear_btn")}
            />
          ) : (
            <>
              {/* Clean Table Card */}
              <Card className="overflow-hidden border border-border/80">
                <div className="overflow-x-auto">
                  <table className="w-full min-w-[56rem] border-collapse text-sm">
                    <thead>
                      <tr className="border-b border-border bg-muted/40 text-left text-[11px] uppercase tracking-wider text-muted-foreground">
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_status")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_server")}</th>
                        {isAdmin && <th className="px-4 py-2.5 font-medium">{t("fleet_col_owner")}</th>}
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_players")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_policy")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_endpoint")}</th>
                        <th className="px-4 py-2.5 text-right font-medium">
                          {t("fleet_col_actions")}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {paged.map((s) => (
                        <ServerRow
                          key={s.name}
                          server={s}
                          cfg={cfg}
                          isAdmin={isAdmin}
                          onChanged={reload}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
              </Card>

              {totalPages > 1 && (
                <Pagination
                  page={safePage}
                  pageSize={PAGE_SIZE}
                  total={visible.length}
                  onChange={setPage}
                />
              )}
            </>
          )}
        </>
      )}
    </div>
  );
}

function ServerRow({
  server,
  cfg,
  isAdmin,
  onChanged,
}: {
  server: UnifiedServer;
  cfg: RuntimeConfig;
  isAdmin: boolean;
  onChanged: () => void;
}) {
  const { t } = useTranslation("ops");
  const { t: ts } = useTranslation("servers");
  const [busy, setBusy] = useState<null | "wake" | "stop">(null);
  const [error, setError] = useState<string | null>(null);

  async function act(kind: "wake" | "stop", fn: () => Promise<unknown>) {
    setBusy(kind);
    setError(null);
    try {
      await fn();
      onChanged();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(null);
    }
  }

  const live = isLive(server.phase);
  const host = server.subdomain ? hostFor(server.subdomain, cfg) : "";
  const endpoint = server.ready && server.endpointAddress ? server.endpointAddress : null;

  return (
    <>
      <tr className="border-b border-border/50 transition-colors last:border-0 hover:bg-muted/40">
        <td className="px-4 py-3 align-middle text-left">
          <PhaseBadge phase={server.phase} />
        </td>
        <td className="px-4 py-3 align-middle text-left">
          <div className="font-medium text-foreground">{server.name}</div>
          {host && (
            <a
              href={`https://${host}`}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
            >
              {host}
              <ExternalLink className="h-3 w-3" />
            </a>
          )}
        </td>
        {isAdmin && (
          <td className="px-4 py-3 align-middle text-left">
            {server.owner ? (
              <span className="inline-flex max-w-[16rem] items-center gap-1.5 truncate">
                <UserRound className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                <span className="truncate" title={server.owner}>
                  {server.owner}
                </span>
              </span>
            ) : (
              <span className="text-xs text-muted-foreground/70">{t("fleet_unclaimed")}</span>
            )}
          </td>
        )}
        <td className="px-4 py-3 align-middle tabular-nums text-left">
          {live ? (
            <span className="inline-flex items-center gap-1.5">
              <Users className="h-3.5 w-3.5 text-muted-foreground" />
              {server.playersOnline}
              <span className="text-muted-foreground">/ {server.playersMax}</span>
            </span>
          ) : (
            <span className="text-muted-foreground">—</span>
          )}
        </td>
        <td className="px-4 py-3 align-middle text-left">
          {server.autostartPolicy ? (
            <span className="text-xs text-muted-foreground">
              {t(POLICY_KEY[server.autostartPolicy])}
            </span>
          ) : (
            <span className="text-muted-foreground">—</span>
          )}
        </td>
        <td className="px-4 py-3 align-middle text-left">
          {endpoint ? (
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
              {endpoint}
            </code>
          ) : (
            <span className="text-xs text-muted-foreground/70">{t("fleet_endpoint_idle")}</span>
          )}
        </td>
        <td className="px-4 py-3 align-middle">
          <div className="flex items-center justify-end gap-2">
            {live ? (
              <Button
                size="sm"
                variant="destructive"
                disabled={busy !== null}
                onClick={() => act("stop", () => api.stop(server.name))}
              >
                <Square /> {busy === "stop" ? ts("stopping") : ts("stop")}
              </Button>
            ) : (
              <Button
                size="sm"
                variant="default"
                disabled={busy !== null}
                onClick={() => act("wake", () => api.wake(server.name))}
              >
                <Play /> {busy === "wake" ? ts("waking") : ts("wake")}
              </Button>
            )}
            <Link to={`/servers/${server.name}`}>
              <Button size="sm" variant="outline">
                <Terminal /> {ts("console")}
              </Button>
            </Link>
          </div>
        </td>
      </tr>
      {error && (
        <tr className="bg-destructive/5">
          <td colSpan={isAdmin ? 7 : 6} className="px-4 py-1.5 text-xs text-destructive text-left">
            {error}
          </td>
        </tr>
      )}
    </>
  );
}

export default ServersPage;
