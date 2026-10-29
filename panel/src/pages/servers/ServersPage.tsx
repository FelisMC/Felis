import { useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  BookOpen,
  DoorOpen,
  Network,
  Play,
  Terminal,
  RefreshCw,
  RotateCcw,
  Server,
  Square,
  Users,
  AlertTriangle,
  UserRound,
  Hand,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { SearchInput } from "@/components/SearchInput";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from "@/components/ui/select";
import { PhaseBadge, PHASE_KEY, PHASE_COLOR, shownPhase, startFailure } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
import { Loading, ErrorState, EmptyState, RefreshError } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { useToast } from "@/components/Toaster";
import { DistributedNodes, MigrationDialog } from "@/components/DistributedNodes";
import { CreateServerDialog } from "@/components/CreateServerDialog";
import { StatCard } from "@/components/StatCard";
import { CopyAddress } from "@/components/CopyAddress";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { RefreshContext, useAsync, useConfig, usePageRefresh, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { joinAddress, type RuntimeConfig } from "@/lib/config";
import { matchScore } from "@/lib/fuzzy";
import type { AutostartPolicy, DesiredState, FleetServer, Phase, MyServerView, RetireState } from "@/lib/types";
import { cn } from "@/lib/utils";
import { cpuUsage, memoryUsage } from "@/lib/resourceUsage";

const REFRESH_MS = 10_000;

const PHASES: Phase[] = [
  "Running",
  "Starting",
  "Stopping",
  "Failed",
  "Hibernating",
  "Hibernated",
  "Stopped",
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

// Twenty stays the default: an even count fills the two-column cards, and a
// fleet of a dozen or so needs no paging at all.
const PAGE_SIZES = [20, 50, 100];
const PAGE_SIZE_KEY = "felis-servers-page-size";

function savedPageSize(): number {
  try {
    const saved = Number(window.localStorage.getItem(PAGE_SIZE_KEY));
    return PAGE_SIZES.find((size) => size === saved) ?? PAGE_SIZES[0];
  } catch {
    return PAGE_SIZES[0];
  }
}

type SortKey = "name" | "status" | "players";

const SORT_KEYS: SortKey[] = ["name", "status", "players"];

const SORT_LABEL: Record<SortKey, string> = {
  name: "fleet_sort_name",
  status: "fleet_sort_status",
  players: "fleet_sort_players",
};

const labelOrder = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

function byName(a: UnifiedServer, b: UnifiedServer): number {
  return (
    labelOrder.compare(a.displayName || a.name, b.displayName || b.name) ||
    (a.name < b.name ? -1 : a.name > b.name ? 1 : 0)
  );
}

// Use name as a stable tiebreaker between periodic list refreshes.
function compareBy(key: SortKey): (a: UnifiedServer, b: UnifiedServer) => number {
  switch (key) {
    case "status":
      return (a, b) => PHASES.indexOf(shownPhase(a)) - PHASES.indexOf(shownPhase(b)) || byName(a, b);
    case "players":
      return (a, b) => b.playersOnline - a.playersOnline || byName(a, b);
    default:
      return byName;
  }
}

interface UnifiedServer {
  diskFull?: boolean;
  cpu?: string;
  memory?: string;
  cpuMilli?: number;
  memoryBytes?: number;
  nodeName?: string;
  name: string;
  subdomain: string;
  /** The owner-chosen label; the list leads with it and keeps the name beside. */
  displayName?: string;
  phase: Phase;
  ready: boolean;
  desiredState?: DesiredState;
  autostartPolicy?: AutostartPolicy;
  playersOnline: number;
  playersMax: number;
  playerCountUnknown?: boolean;
  autoRestarts?: number;
  startGaveUp?: boolean;
  owner?: string;
  endpointAddress?: string | null;
  claimable?: boolean;
  owned?: boolean;
  /** The fleet's owner lookup failed, so an absent owner proves nothing. */
  ownerUnknown?: boolean;
  system?: boolean;
  /** A pending retirement (owner and admin views): the row offers no start. */
  retiring?: RetireState;
  /** Owner and admin views only. */
  tags?: string[];
}

// Platform services have their own controls; everything else is the owner's or an admin's to select.
function selectable(s: UnifiedServer, isAdmin: boolean): boolean {
  return !s.system && (isAdmin || s.owned === true);
}

interface RowSelection {
  checked: boolean;
  disabled: boolean;
  onChange: (checked: boolean) => void;
}

type BatchKind = "wake" | "stop" | "restart";

interface BatchProgress {
  current: number;
  total: number;
  label: string;
}

interface BatchOutcome {
  name: string;
  label: string;
  error: string | null;
}

export function ServersPage() {
  const { t } = useTranslation(["ops", "servers"]);
  const { isAdmin } = useTier();
  const cfg = useConfig();
  const toast = useToast();
  const tagsId = useId();

  const fetchFn = useMemo<() => Promise<FleetServer[] | MyServerView[]>>(
    () => (isAdmin ? api.fleet : api.myServers),
    [isAdmin],
  );
  const { data, error, loading, reload } = useAsync(fetchFn, [fetchFn]);
  // A player with a quota (their own, or the Owner's default) creates servers of
  // their own here; the read also says why not when they cannot.
  const quota = useAsync(() => (isAdmin ? Promise.resolve(null) : api.myServerQuota()), [isAdmin]);
  const selfCreate =
    !isAdmin && quota.data && (quota.data.canCreate || quota.data.reason === "quota_full") ? quota.data : null;

  const [query, setQuery] = useState("");
  const [phaseFilter, setPhaseFilter] = useState<Phase | "all">("all");
  const [sort, setSort] = useState<SortKey>("name");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(savedPageSize);
  // Lower-case tag keys: the same tag in another letter case is one filter.
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [progress, setProgress] = useState<BatchProgress | null>(null);
  const [outcomes, setOutcomes] = useState<BatchOutcome[] | null>(null);

  function changePageSize(next: number) {
    setPageSize(next);
    setPage(1);
    try {
      window.localStorage.setItem(PAGE_SIZE_KEY, String(next));
    } catch {
      // Keep the choice for this visit when browser storage is unavailable.
    }
  }

  function toggleTag(key: string) {
    setTagFilter((keys) => (keys.includes(key) ? keys.filter((k) => k !== key) : [...keys, key]));
    setPage(1);
  }

  function select(names: readonly string[], on: boolean) {
    setSelected((current) => {
      const next = new Set(current);
      for (const name of names) {
        if (on) next.add(name);
        else next.delete(name);
      }
      return next;
    });
  }

  usePolling(reload, REFRESH_MS);

  // Background polling must not disable the manual refresh button.
  const [refreshing, setRefreshing] = useState(false);
  if (refreshing && !loading) setRefreshing(false);
  const cards = usePageRefresh();
  const refresh = () => {
    setRefreshing(true);
    reload();
    cards.refresh();
  };

  const servers = useMemo<UnifiedServer[]>(() => {
    if (!data) return [];
    if (isAdmin) {
      return (data as FleetServer[]).map((s) => ({
        cpu: s.cpu, memory: s.memory, cpuMilli: s.cpuMilli, memoryBytes: s.memoryBytes,
        name: s.name,
        nodeName: s.nodeName,
        displayName: s.displayName,
        subdomain: s.subdomain,
        phase: s.phase,
        ready: s.ready,
        desiredState: s.desiredState,
        diskFull: s.diskFull,
        autostartPolicy: s.autostartPolicy,
        playersOnline: s.playersOnline,
        playersMax: s.playersMax,
        playerCountUnknown: s.playerCountUnknown,
        autoRestarts: s.autoRestarts,
        startGaveUp: s.startGaveUp,
        owner: s.owner,
        endpointAddress: s.endpointAddress,
        claimable: s.claimable,
        owned: s.owned,
        ownerUnknown: s.ownerUnknown,
        system: s.system,
        retiring: s.retiring,
        tags: s.tags,
      }));
    } else {
      return (data as MyServerView[]).map((s) => ({
        cpu: s.cpu, memory: s.memory, cpuMilli: s.cpuMilli, memoryBytes: s.memoryBytes,
        name: s.name,
        displayName: s.displayName,
        subdomain: s.subdomain,
        phase: s.phase ?? "Unknown",
        ready: s.phase === "Running",
        desiredState: s.desiredState,
        diskFull: s.diskFull,
        autostartPolicy: s.autostartPolicy,
        playersOnline: s.playersOnline,
        playersMax: s.playersMax,
        playerCountUnknown: s.playerCountUnknown,
        autoRestarts: s.autoRestarts,
        startGaveUp: s.startGaveUp,
        owner: s.owned ? t("servers:owned_filter_mine") || "me" : undefined,
        claimable: s.claimable,
        owned: s.owned,
        retiring: s.retiring,
        tags: s.tags,
      }));
    }
  }, [data, isAdmin, t]);

  const availableTags = useMemo(() => {
    const labels = new Map<string, string>();
    for (const s of servers) {
      for (const tag of s.tags ?? []) {
        if (!labels.has(tag.toLowerCase())) labels.set(tag.toLowerCase(), tag);
      }
    }
    return [...labels].map(([key, label]) => ({ key, label })).sort((a, b) => labelOrder.compare(a.label, b.label));
  }, [servers]);
  // A tag no server carries any more drops out of the filter instead of hiding every row.
  const activeTags = useMemo(
    () => tagFilter.filter((key) => availableTags.some((tag) => tag.key === key)),
    [tagFilter, availableTags],
  );

  const stats = useMemo(() => {
    const counts: Record<Phase, number> = {
      Running: 0,
      Starting: 0,
      Stopping: 0,
      Stopped: 0,
      Failed: 0,
      Unknown: 0,
      Hibernating: 0,
      Hibernated: 0,
    };
    let playersOnline = 0;
    let playersMax = 0;
    for (const s of servers) {
      const phase = shownPhase(s);
      if (counts[phase] !== undefined) counts[phase]++;
      else counts.Unknown++;
      playersOnline += s.playersOnline ?? 0;
      playersMax += s.playersMax ?? 0;
    }
    return { counts, playersOnline, playersMax, total: servers.length };
  }, [servers]);

  const visible = useMemo(() => {
    const terms = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
    const rowOk = (s: UnifiedServer) =>
      (phaseFilter === "all" || shownPhase(s) === phaseFilter) &&
      activeTags.every((key) => s.tags?.some((tag) => tag.toLowerCase() === key));
    const order = compareBy(sort);
    if (terms.length === 0) return servers.filter(rowOk).sort(order);
    const scored: { s: UnifiedServer; score: number }[] = [];
    for (const s of servers) {
      if (!rowOk(s)) continue;
      const score = matchScore([s.name, s.displayName ?? "", s.subdomain ?? "", s.owner ?? ""], terms);
      if (score >= 0) scored.push({ s, score });
    }
    // A search leads with the best match; the picked order ranks equal matches.
    scored.sort((a, b) => b.score - a.score || order(a.s, b.s));
    return scored.map((x) => x.s);
  }, [servers, query, phaseFilter, activeTags, sort]);

  const totalPages = Math.max(1, Math.ceil(visible.length / pageSize));
  const safePage = Math.min(page, totalPages);
  const paged = visible.slice((safePage - 1) * pageSize, safePage * pageSize);

  // Only servers the current filters show count as selected, so a batch never reaches a hidden one.
  const picked = useMemo(
    () => visible.filter((s) => selected.has(s.name) && selectable(s, isAdmin)),
    [visible, selected, isAdmin],
  );
  const pageRows = paged.filter((s) => selectable(s, isAdmin));
  const pageAllSelected = pageRows.length > 0 && pageRows.every((s) => selected.has(s.name));
  const rowSelection = (s: UnifiedServer): RowSelection | undefined =>
    selectable(s, isAdmin)
      ? { checked: selected.has(s.name), disabled: progress !== null, onChange: (on) => select([s.name], on) }
      : undefined;

  async function runBatch(kind: BatchKind, targets: UnifiedServer[]) {
    setOutcomes(null);
    const done: BatchOutcome[] = [];
    for (const [i, s] of targets.entries()) {
      const label = s.displayName || s.name;
      setProgress({ current: i + 1, total: targets.length, label });
      try {
        await api[kind](s.name);
        done.push({ name: s.name, label, error: null });
      } catch (e) {
        done.push({ name: s.name, label, error: humanizeError(e) });
      }
    }
    setProgress(null);
    setOutcomes(done);
    select(done.filter((o) => o.error === null).map((o) => o.name), false);
    reload();
    const failed = done.filter((o) => o.error !== null).length;
    const summary = t("servers:batch_summary", { done: done.length - failed, failed });
    if (failed > 0) toast.error(summary);
    else toast.success(summary);
  }

  const showInitialLoading = loading && !data;
  const showInitialError = !!error && !data;

  return (
    <div className="space-y-6">
      <PageHeader
        icon={Network}
        title={isAdmin ? t("fleet_title") : t("servers:my_servers_title")}
        subtitle={isAdmin ? t("fleet_subtitle") : t("servers:my_servers_subtitle")}
        actions={
          <div className="flex items-center gap-3">
            <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
              <span className="h-1.5 w-1.5 rounded-full bg-[#22c55e] animate-pulse" />
              {t("fleet_live")}
            </span>
            <Button
              variant="outline"
              size="sm"
              onClick={refresh}
              disabled={refreshing}
              aria-label={t("fleet_refresh")}
              title={t("fleet_refresh")}
            >
              <RefreshCw className={cn("h-4 w-4", refreshing && "animate-spin")} />
            </Button>
            {isAdmin && cfg && <CreateServerDialog cfg={cfg} onCreated={reload} />}
            {selfCreate && cfg && (
              <CreateServerDialog cfg={cfg} self={{ quota: selfCreate }} onCreated={() => { reload(); quota.reload(); }} />
            )}
          </div>
        }
        className="mb-6"
      />

      {isAdmin && cfg?.distributed && <RefreshContext.Provider value={cards.scope}><DistributedNodes /></RefreshContext.Provider>}

      {showInitialLoading ? (
        <Loading />
      ) : showInitialError ? (
        <ErrorState error={error} onRetry={reload} />
      ) : !cfg ? (
        <Loading />
      ) : (
        <>
          {error && <RefreshError error={error} />}

          {/* Stats Cards in a full grid row */}
          <div className="grid grid-cols-2 gap-4 xl:grid-cols-4">
            <StatCard icon={Server} label={t("fleet_stat_total")} value={stats.total} />
            <StatCard
              icon={Play}
              label={t("fleet_stat_running")}
              value={stats.counts.Running}
              accentColor={PHASE_COLOR.Running}
            />
            <StatCard
              icon={Users}
              label={t("fleet_stat_players")}
              value={`${stats.playersOnline} / ${stats.playersMax}`}
            />
            <StatCard
              icon={AlertTriangle}
              label={t("fleet_stat_failed")}
              value={stats.counts.Failed}
              accentColor={stats.counts.Failed > 0 ? PHASE_COLOR.Failed : undefined}
            />
          </div>

          {/* Distribution card in a full grid row */}
          {stats.total > 0 && (
            <Card>
              <CardContent className="space-y-3">
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
            <SearchInput
              value={query}
              onChange={(v) => {
                setQuery(v);
                setPage(1);
              }}
              placeholder={isAdmin ? t("fleet_search_placeholder") : t("servers:search_placeholder")}
            />
            <Select
              value={phaseFilter}
              onValueChange={(v) => {
                setPhaseFilter(v as Phase | "all");
                setPage(1);
              }}
            >
              <SelectTrigger className="w-[calc(50%-0.375rem)] sm:w-44" aria-label={t("fleet_filter_phase")}>
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
            <Select
              value={sort}
              onValueChange={(v) => {
                setSort(v as SortKey);
                setPage(1);
              }}
            >
              <SelectTrigger className="w-[calc(50%-0.375rem)] sm:w-44" aria-label={t("fleet_sort")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {SORT_KEYS.map((k) => (
                  <SelectItem key={k} value={k}>
                    {t(SORT_LABEL[k])}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select value={String(pageSize)} onValueChange={(v) => changePageSize(Number(v))}>
              <SelectTrigger className="w-[calc(50%-0.375rem)] sm:w-44" aria-label={t("servers:page_size_label")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {PAGE_SIZES.map((size) => (
                  <SelectItem key={size} value={String(size)}>
                    {t("servers:page_size_option", { size })}
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

          {availableTags.length > 0 && (
            <div role="group" aria-labelledby={tagsId} className="flex flex-wrap items-center gap-1.5">
              <span id={tagsId} className="mr-1 text-xs text-muted-foreground">
                {t("servers:tags_label")}
              </span>
              {availableTags.map(({ key, label }) => {
                const on = activeTags.includes(key);
                return (
                  <button
                    key={key}
                    type="button"
                    aria-pressed={on}
                    onClick={() => toggleTag(key)}
                    className={cn(
                      "rounded-full border px-2.5 py-0.5 text-xs transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                      on
                        ? "border-primary bg-primary text-primary-foreground"
                        : "border-border bg-card text-muted-foreground hover:bg-accent hover:text-foreground",
                    )}
                  >
                    {label}
                  </button>
                );
              })}
            </div>
          )}

          {(picked.length > 0 || progress !== null || outcomes !== null) && (
            <BatchBar
              picked={picked}
              progress={progress}
              outcomes={outcomes}
              onRun={(kind) => void runBatch(kind, picked)}
              onClear={() => setSelected(new Set())}
              onDismiss={() => setOutcomes(null)}
            />
          )}

          {servers.length === 0 ? (
            <EmptyState
              title={isAdmin ? t("fleet_empty_title") : t("servers:no_servers_linked")}
              hint={isAdmin ? t("fleet_empty_hint") : t("servers:no_servers_hint")}
            />
          ) : visible.length === 0 ? (
            <EmptyState
              title={isAdmin ? t("fleet_no_match_title") : t("servers:search_no_match")}
              hint={t("fleet_no_match_hint")}
            >
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  setQuery("");
                  setPhaseFilter("all");
                  setTagFilter([]);
                  setPage(1);
                }}
              >
                {t("servers:search_clear_btn")}
              </Button>
            </EmptyState>
          ) : (
            <>
              {/* Cards until the table fits (two per row from md). The admin
                  table needs about 1400px of content width for its seven columns,
                  which the page has from a 1700px window beside the sidebar; the
                  player's five columns fit from xl. A row of actions wraps only
                  when its column cannot hold it. */}
              <div className={cn("space-y-3", isAdmin ? "min-[1700px]:hidden" : "xl:hidden")}>
                {pageRows.length > 0 && (
                  <label className="flex w-fit cursor-pointer items-center gap-2 text-xs text-muted-foreground">
                    <Checkbox
                      checked={pageAllSelected}
                      disabled={progress !== null}
                      onChange={(e) => select(pageRows.map((s) => s.name), e.target.checked)}
                    />
                    {t("servers:select_page")}
                  </label>
                )}
                <ul className="grid gap-3 md:grid-cols-2">
                  {paged.map((s) => (
                    <ServerMobileCard
                      key={s.name}
                      server={s}
                      cfg={cfg}
                      isAdmin={isAdmin}
                      select={rowSelection(s)}
                      onChanged={reload}
                    />
                  ))}
                </ul>
              </div>
              <Card
                className={cn(
                  "hidden overflow-hidden border border-border/80",
                  isAdmin ? "min-[1700px]:block" : "xl:block",
                )}
              >
                <div className="overflow-x-auto">
                  <table className="w-full border-collapse text-sm">
                    <thead>
                      <tr className="whitespace-nowrap border-b border-border bg-muted/40 text-left text-[11px] uppercase tracking-wider text-muted-foreground">
                        <th className="w-10 py-2.5 pl-4 pr-0 font-medium">
                          <Checkbox
                            aria-label={t("servers:select_page")}
                            checked={pageAllSelected}
                            disabled={pageRows.length === 0 || progress !== null}
                            onChange={(e) => select(pageRows.map((s) => s.name), e.target.checked)}
                          />
                        </th>
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_server")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_status")}</th>
                        {isAdmin && <th className="px-4 py-2.5 font-medium">{t("fleet_col_owner")}</th>}
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_players")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("servers:usage_cpu")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("servers:usage_memory")}</th>
                        <th className="px-4 py-2.5 font-medium">{t("fleet_col_policy")}</th>
                        {isAdmin && <th className="px-4 py-2.5 font-medium">{t("fleet_col_endpoint")}</th>}
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
                          select={rowSelection(s)}
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
                  pageSize={pageSize}
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

// ServerActions is the action cluster of one server — claim, start/stop, console —
// shared by the desktop table row and the phone card so both offer the same things.
function ServerActions({
  server,
  isAdmin,
  onChanged,
  className,
}: {
  server: UnifiedServer;
  isAdmin: boolean;
  onChanged: () => void;
  className?: string;
}) {
  const { t: ts } = useTranslation("servers");
  const cfg = useConfig();
  const [busy, setBusy] = useState<null | "claim">(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function act(kind: "claim", fn: () => Promise<unknown>) {
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

  if (server.system) {
    // Staff customize system services in the dedicated lobby view.
    return (
      <div className={cn("flex justify-end", className)}>
        {isAdmin ? (
          <Button asChild variant="outline" size="sm">
            <Link to={`/admin/lobby?space=${server.name}`}>{server.name === "login" ? <BookOpen /> : <DoorOpen />}{ts(server.name === "login" ? "system_customize_login" : "system_customize")}</Link>
          </Button>
        ) : (
          <span className="text-sm text-muted-foreground" title={ts("system_service_hint")}>{ts("system_service")}</span>
        )}
      </div>
    );
  }

  // Unclaimed servers expose claim only, except administrators retain wake and console access.
  const claimable = server.claimable && !server.owned;

  return (
    <div className={cn("flex flex-col items-end gap-1", className)}>
      <div className="flex flex-wrap items-start justify-end gap-2">
        {isAdmin && cfg?.distributed && <MigrationDialog name={server.name} nodeName={server.nodeName} stopped={server.phase === "Stopped" && !server.ready && server.desiredState === "Stopped" && !server.retiring} onChanged={onChanged} />}
        {claimable && (
          <>
            <Button
              size="sm"
              variant="outline"
              disabled={busy !== null}
              onClick={() => setConfirmOpen(true)}
              className="text-primary hover:text-primary hover:bg-primary/5 border-primary/20"
            >
              <Hand /> {ts("claim")}
            </Button>

            <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
              <DialogContent className="sm:max-w-md">
                <DialogHeader>
                  <DialogTitle>{ts("claim_server_title")}</DialogTitle>
                  <DialogDescription>
                    {ts("claim_server_desc", { name: server.displayName || server.name })}
                  </DialogDescription>
                </DialogHeader>
                <ConfirmFooter
                  onCancel={() => setConfirmOpen(false)}
                  onConfirm={async () => {
                    await act("claim", () => api.claim(server.name));
                    setConfirmOpen(false);
                  }}
                  disabled={busy !== null}
                  loading={busy === "claim"}
                  cancelLabel={ts("access_cancel")}
                  confirmLabel={ts("claim")}
                  confirmVariant="default"
                />
              </DialogContent>
            </Dialog>
          </>
        )}
        {(!claimable || isAdmin) && (
          <PowerButton
            name={server.name}
            phase={server.phase}
            desiredState={server.desiredState}
            failed={startFailure(server) !== null}
            playersOnline={server.playersOnline}
            playerCountUnknown={server.playerCountUnknown}
            retiring={server.retiring}
            diskFull={server.diskFull}
            onChanged={onChanged}
          />
        )}
        {(server.owned || isAdmin) && (
          <Link to={`/servers/${server.name}`}>
            <Button size="sm" variant="outline">
              <Terminal /> {ts("console")}
            </Button>
          </Link>
        )}
      </div>
      {error && (
        <p role="alert" className="max-w-xs text-right text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}

function OwnerLabel({ server }: { server: UnifiedServer }) {
  const { t } = useTranslation("ops");
  const { t: ts } = useTranslation("servers");
  if (server.system) {
    return (
      <span className="text-muted-foreground" title={ts("system_service_hint")}>
        {ts("system_service")}
      </span>
    );
  }
  if (server.owner) {
    return (
      <span className="inline-flex min-w-0 max-w-full items-center gap-1.5 md:max-w-[13rem]">
        <UserRound className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
        {server.owned && (
          <span className="shrink-0 rounded-full border px-1.5 text-[10px] font-medium leading-4 text-foreground">
            {t("fleet_owner_you")}
          </span>
        )}
        <span className="truncate" title={server.owner}>
          {server.owner}
        </span>
      </span>
    );
  }
  if (server.ownerUnknown) {
    return (
      <span className="text-muted-foreground" title={t("fleet_owner_unknown_hint")}>
        {t("fleet_owner_unknown")}
      </span>
    );
  }
  return <span className="text-muted-foreground">{t("fleet_unclaimed")}</span>;
}

function PlayersLabel({ server }: { server: UnifiedServer }) {
  if (!isLive(server.phase)) return <span className="text-muted-foreground">—</span>;
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap tabular-nums">
      <Users className="h-3.5 w-3.5 text-muted-foreground" />
      {server.playersOnline}
      <span className="text-muted-foreground">/ {server.playersMax}</span>
    </span>
  );
}

function EndpointLabel({ server }: { server: UnifiedServer }) {
  return <div className="space-y-1">{server.nodeName && <code className="text-xs">{server.nodeName}</code>}<EndpointAddress server={server} /></div>;
}

function EndpointAddress({ server }: { server: UnifiedServer }) {
  const { t } = useTranslation("ops");
  const endpoint = server.ready && server.endpointAddress ? server.endpointAddress : null;
  if (!endpoint) {
    return <span className="text-muted-foreground">{t("fleet_endpoint_idle")}</span>;
  }
  return (
    <code className="break-all rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground xl:whitespace-nowrap xl:break-normal">
      {endpoint}
    </code>
  );
}

function RowSelect({ server, select }: { server: UnifiedServer; select?: RowSelection }) {
  const { t } = useTranslation("servers");
  if (!select) return null;
  return (
    <Checkbox
      aria-label={t("select_server", { name: server.displayName || server.name })}
      checked={select.checked}
      disabled={select.disabled}
      onChange={(e) => select.onChange(e.target.checked)}
    />
  );
}

function TagChips({ tags }: { tags?: string[] }) {
  const { t } = useTranslation("servers");
  if (!tags?.length) return null;
  return (
    <ul aria-label={t("tags_label")} className="mt-1 flex flex-wrap gap-1">
      {tags.map((tag) => (
        <li
          key={tag}
          title={tag}
          className="max-w-full truncate rounded-full border border-border bg-muted/50 px-2 text-[11px] leading-5 text-muted-foreground"
        >
          {tag}
        </li>
      ))}
    </ul>
  );
}

function ServerRow({
  server,
  cfg,
  isAdmin,
  select,
  onChanged,
}: {
  server: UnifiedServer;
  cfg: RuntimeConfig;
  isAdmin: boolean;
  select?: RowSelection;
  onChanged: () => void;
}) {
  const { t } = useTranslation("ops");
  const address = server.subdomain ? joinAddress(server.subdomain, cfg) : "";

  return (
    <tr className="border-b border-border/50 transition-colors last:border-0 hover:bg-muted/40">
      <td className="w-10 py-3 pl-4 pr-0 align-middle">
        <RowSelect server={server} select={select} />
      </td>
      <td className="px-4 py-3 align-middle text-left">
        <div className="flex min-w-0 items-baseline gap-2">
          <ServerName server={server} />
        </div>
        <TagChips tags={server.tags} />
        {address && <CopyAddress address={address} className="mt-0.5 flex-nowrap md:max-w-[18rem]" />}
      </td>
      <td className="whitespace-nowrap px-4 py-3 align-middle text-left">
        <PhaseBadge phase={shownPhase(server)} failure={startFailure(server)} autoRestarts={server.autoRestarts} />
      </td>
      {isAdmin && (
        <td className="whitespace-nowrap px-4 py-3 align-middle text-left">
          <OwnerLabel server={server} />
        </td>
      )}
      <td className="whitespace-nowrap px-4 py-3 align-middle text-left">
        <PlayersLabel server={server} />
      </td>
      <td className="whitespace-nowrap px-4 py-3 tabular-nums">{cpuUsage(server.cpuMilli, server.cpu)}</td>
      <td className="whitespace-nowrap px-4 py-3 tabular-nums">{memoryUsage(server.memoryBytes, server.memory)}</td>
      <td className="whitespace-nowrap px-4 py-3 align-middle text-left">
        {server.autostartPolicy ? (
          <span className="text-foreground">
            {t(POLICY_KEY[server.autostartPolicy])}
          </span>
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </td>
      {isAdmin && (
        <td className="whitespace-nowrap px-4 py-3 align-middle text-left">
          <EndpointLabel server={server} />
        </td>
      )}
      <td className="whitespace-nowrap px-4 py-3 align-middle">
        <ServerActions server={server} isAdmin={isAdmin} onChanged={onChanged} />
      </td>
    </tr>
  );
}

// ServerMobileCard is one server below xl, where the table's columns would not
// fit: name and phase on top, the facts as a short list, the actions at the
// bottom within thumb reach.
function ServerMobileCard({
  server,
  cfg,
  isAdmin,
  select,
  onChanged,
}: {
  server: UnifiedServer;
  cfg: RuntimeConfig;
  isAdmin: boolean;
  select?: RowSelection;
  onChanged: () => void;
}) {
  const { t } = useTranslation("ops");
  const address = server.subdomain ? joinAddress(server.subdomain, cfg) : "";

  return (
    <li className="flex min-w-0">
      <Card className="flex min-w-0 flex-1 flex-col border">
        <CardContent className="flex flex-1 flex-col gap-3">
          <div className="space-y-1">
            <div className="flex items-start justify-between gap-3">
              <div className="flex min-w-0 items-center gap-2">
                <RowSelect server={server} select={select} />
                <div className="flex min-w-0 items-baseline gap-2">
                  <ServerName server={server} />
                </div>
              </div>
              <PhaseBadge phase={shownPhase(server)} failure={startFailure(server)} autoRestarts={server.autoRestarts} />
            </div>
            <TagChips tags={server.tags} />
            {/* Its own line: the join address is what a player came for, so it
                gets the card's full width rather than sharing it with the badge. */}
            {address && <CopyAddress address={address} />}
          </div>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
            {isAdmin && (
              <>
                <dt className="text-xs text-muted-foreground">{t("fleet_col_owner")}</dt>
                <dd className="min-w-0">
                  <OwnerLabel server={server} />
                </dd>
              </>
            )}
            <dt className="text-xs text-muted-foreground">{t("fleet_col_players")}</dt>
            <dd>
              <PlayersLabel server={server} />
            </dd>
            {server.autostartPolicy && (
              <>
                <dt className="text-xs text-muted-foreground">{t("fleet_col_policy")}</dt>
                <dd className="text-xs">{t(POLICY_KEY[server.autostartPolicy])}</dd>
              </>
            )}
            {isAdmin && (
              <>
                <dt className="text-xs text-muted-foreground">{t("fleet_col_endpoint")}</dt>
                <dd className="min-w-0">
                  <EndpointLabel server={server} />
                </dd>
              </>
            )}
          </dl>
          <div className="mt-auto border-t border-border/50 pt-3">
            <ServerActions server={server} isAdmin={isAdmin} onChanged={onChanged} />
          </div>
        </CardContent>
      </Card>
    </li>
  );
}

// BatchBar acts on the selected servers one after another; a stop or restart asks once for the whole batch.
function BatchBar({
  picked,
  progress,
  outcomes,
  onRun,
  onClear,
  onDismiss,
}: {
  picked: UnifiedServer[];
  progress: BatchProgress | null;
  outcomes: BatchOutcome[] | null;
  onRun: (kind: BatchKind) => void;
  onClear: () => void;
  onDismiss: () => void;
}) {
  const { t } = useTranslation("servers");
  const [confirming, setConfirming] = useState<"stop" | "restart" | null>(null);
  const kind = confirming ?? "stop";
  const busy = progress !== null;
  const withPlayers = picked.filter((s) => s.playersOnline > 0 || s.playerCountUnknown);

  return (
    <section aria-label={t("batch_label")} className="space-y-3 rounded-lg border border-primary/30 bg-primary/5 p-3">
      {picked.length > 0 && (
        <div className="flex flex-wrap items-center gap-2">
          <span className="mr-auto text-sm font-medium">{t("batch_selected", { count: picked.length })}</span>
          <Button size="sm" disabled={busy} onClick={() => onRun("wake")}>
            <Play /> {t("wake")}
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => setConfirming("stop")}>
            <Square /> {t("stop")}
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => setConfirming("restart")}>
            <RotateCcw /> {t("restart")}
          </Button>
          <Button size="sm" variant="ghost" disabled={busy} onClick={onClear}>
            {t("batch_clear")}
          </Button>
        </div>
      )}
      {progress && (
        <p role="status" className="text-sm text-muted-foreground">
          {t("batch_progress", { current: progress.current, total: progress.total, name: progress.label })}
        </p>
      )}
      {outcomes && (
        <div className="space-y-1.5">
          <div className="flex items-center justify-between gap-2">
            <p className="text-sm font-medium">{t("batch_results")}</p>
            <Button size="sm" variant="ghost" onClick={onDismiss}>
              {t("batch_results_dismiss")}
            </Button>
          </div>
          <ul className="space-y-1 text-sm">
            {outcomes.map((o) => (
              <li key={o.name} className={cn("flex flex-wrap gap-x-2", o.error !== null && "text-destructive")}>
                <span className="font-medium">{o.label}</span>
                <span>{o.error ?? t("batch_result_ok")}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      <Dialog open={confirming !== null} onOpenChange={(open) => !open && setConfirming(null)}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t(`batch_${kind}_title`)}</DialogTitle>
            <DialogDescription>{t(`batch_${kind}_summary`, { count: picked.length })}</DialogDescription>
          </DialogHeader>
          {withPlayers.length > 0 && (
            <div className="space-y-2 text-sm">
              <p>{t("batch_players_intro")}</p>
              <ul className="list-disc space-y-0.5 pl-5">
                {withPlayers.map((s) => (
                  <li key={s.name}>
                    {s.playersOnline > 0
                      ? t("batch_player_item", { name: s.displayName || s.name, count: s.playersOnline })
                      : t("batch_player_unknown_item", { name: s.displayName || s.name })}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <ConfirmFooter
            onCancel={() => setConfirming(null)}
            onConfirm={() => {
              setConfirming(null);
              onRun(kind);
            }}
            cancelLabel={t("common:cancel")}
            confirmLabel={t(kind)}
          />
        </DialogContent>
      </Dialog>
    </section>
  );
}

export default ServersPage;

/** ServerName leads with the display name and keeps the server name beside it,
 *  since the name is what the URL, the console and the subdomain use. */
function ServerName({ server }: { server: UnifiedServer }) {
  const label = server.displayName || server.name;
  return (
    <>
      <span className="truncate font-medium text-foreground">{label}</span>
      {label !== server.name && (
        <span className="truncate font-mono text-xs text-muted-foreground">{server.name}</span>
      )}
    </>
  );
}
