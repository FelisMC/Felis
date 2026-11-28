import { useId, useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  BookOpen,
  DoorOpen,
  Network,
  Play,
  Terminal,
  RefreshCw,
  RotateCcw,
  Square,
  Users,
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
import { CopyAddress } from "@/components/CopyAddress";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { RefreshContext, useAsync, useConfig, usePageRefresh, usePolling } from "@/lib/hooks";
import { useFeatures } from "@/lib/features";
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

function policyLabelKey(policy: AutostartPolicy, teamMode: boolean): string {
  if (policy === "ownerOnly" && teamMode) return "policy_manual";
  return POLICY_KEY[policy];
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
  const { features } = useFeatures();
  const teamMode = features.mode === "team";
  const showOwnership = !teamMode;
  const cfg = useConfig();
  const toast = useToast();
  const tagsId = useId();

  const fetchFn = useMemo<() => Promise<FleetServer[] | MyServerView[]>>(
    () => (isAdmin ? api.fleet : api.myServers),
    [isAdmin],
  );
  const { data, error, loading, reload } = useAsync(fetchFn, [fetchFn]);
  const quota = useAsync(() => (isAdmin || teamMode ? Promise.resolve(null) : api.myServerQuota()), [isAdmin, teamMode]);
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
    <div className="space-y-4">
      <PageHeader
        icon={Network}
        title={isAdmin ? t("fleet_title") : t("servers:my_servers_title")}
        subtitle={isAdmin ? t("fleet_subtitle") : t("servers:my_servers_subtitle")}
        actions={
          <div className="flex items-center gap-2">
            <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
              <span className="h-[5.5px] w-[5.5px] rounded-full bg-primary animate-status-breathe" />
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
              <RefreshCw className={cn("h-3.5 w-3.5", refreshing && "animate-spin")} />
            </Button>
            {isAdmin && cfg && <CreateServerDialog cfg={cfg} onCreated={reload} />}
            {selfCreate && cfg && (
              <CreateServerDialog cfg={cfg} self={{ quota: selfCreate }} onCreated={() => { reload(); quota.reload(); }} />
            )}
          </div>
        }
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

          <div className="flex flex-wrap items-center gap-2">
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
              <SelectTrigger className="w-[calc(50%-0.25rem)] sm:w-36" aria-label={t("fleet_filter_phase")}>
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
              <SelectTrigger className="w-[calc(50%-0.25rem)] sm:w-36" aria-label={t("fleet_sort")}>
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
              <SelectTrigger className="w-[calc(50%-0.25rem)] sm:w-36" aria-label={t("servers:page_size_label")}>
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
          </div>

          {(picked.length > 0 || progress !== null || outcomes !== null) && (
            <div className="animate-slide-down">
              <BatchBar
                picked={picked}
                progress={progress}
                outcomes={outcomes}
                onRun={(kind) => void runBatch(kind, picked)}
                onClear={() => setSelected(new Set())}
                onDismiss={() => setOutcomes(null)}
              />
            </div>
          )}

          {servers.length === 0 ? (
            <EmptyState
              title={isAdmin ? t("fleet_empty_title") : t("servers:no_servers_linked")}
              hint={isAdmin ? t("fleet_empty_hint") : t("servers:no_servers_hint")}
            />
          ) : (
            <>
              <Card className="overflow-hidden">
                <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 border-b border-border/60 px-3.5 py-2 text-xs text-muted-foreground">
                  <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1">
                    <span className="font-mono font-medium text-foreground tabular-nums">
                      {visible.length === servers.length
                        ? t("fleet_count", { count: servers.length })
                        : t("fleet_count_filtered", { shown: visible.length, total: servers.length })}
                    </span>
                    <span className="text-border">·</span>
                    <button
                      type="button"
                      onClick={() => { setPhaseFilter(phaseFilter === "Running" ? "all" : "Running"); setPage(1); }}
                      className={cn(
                        "inline-flex items-center gap-1 rounded border border-transparent px-1.5 py-0.5 transition-[color,background-color,border-color,transform] duration-150 ease-out hover:bg-accent active:scale-[0.97]",
                        phaseFilter === "Running" && "border-border bg-accent font-medium text-foreground",
                      )}
                    >
                      <span className="h-[5.5px] w-[5.5px] rounded-full" style={{ backgroundColor: PHASE_COLOR.Running }} />
                      <span className="inline-flex flex-row-reverse items-center gap-1">
                        <span className="font-mono font-medium text-foreground tabular-nums">{stats.counts.Running}</span>
                        <div className="mt-1 inline" style={{ marginTop: 0 }}>{t("fleet_stat_running")}</div>
                      </span>
                    </button>
                    {PHASES.filter((p) => p !== "Running" && stats.counts[p] > 0).map((p) => (
                      <button
                        key={p}
                        type="button"
                        onClick={() => { setPhaseFilter(phaseFilter === p ? "all" : p); setPage(1); }}
                        className={cn(
                          "inline-flex items-center gap-1 rounded border border-transparent px-1.5 py-0.5 transition-[color,background-color,border-color,transform] duration-150 ease-out hover:bg-accent active:scale-[0.97]",
                          phaseFilter === p && "border-border bg-accent font-medium text-foreground",
                        )}
                      >
                        <span className="h-[5.5px] w-[5.5px] rounded-full" style={{ backgroundColor: PHASE_COLOR[p] }} />
                        <span>{t(PHASE_KEY[p])}</span>
                        <span className="font-mono font-medium text-foreground tabular-nums">{stats.counts[p]}</span>
                      </button>
                    ))}
                    {stats.playersMax > 0 && (
                      <>
                        <span className="text-border">·</span>
                        <span className="inline-flex items-center gap-1 font-mono tabular-nums">
                          <Users className="h-3 w-3 opacity-75" />
                          {stats.playersOnline} / {stats.playersMax}
                        </span>
                      </>
                    )}
                  </div>
                  {availableTags.length > 0 && (
                    <div role="group" aria-labelledby={tagsId} className="flex flex-wrap items-center gap-1">
                      <span id={tagsId} className="mr-0.5 text-[11px] text-muted-foreground">
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
                              "rounded border px-1.5 py-0.5 text-[11px] leading-none transition-[color,background-color,border-color,transform] duration-150 ease-out active:scale-[0.96] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40",
                              on
                                ? "border-primary/35 bg-primary/15 font-medium text-primary"
                                : "border-transparent bg-secondary/50 text-muted-foreground hover:bg-secondary hover:text-foreground",
                            )}
                          >
                            {label}
                          </button>
                        );
                      })}
                    </div>
                  )}
                </div>

                {visible.length === 0 ? (
                  <div className="p-4">
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
                  </div>
                ) : (
                  <>
                    <div className="space-y-3 p-3 lg:hidden">
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
                            teamMode={teamMode}
                            select={rowSelection(s)}
                            onChanged={reload}
                          />
                        ))}
                      </ul>
                    </div>
                    <div className="hidden overflow-x-auto lg:block">
                      <table className="w-full border-collapse text-xs">
                        <thead>
                          <tr className="whitespace-nowrap border-b border-border/60 bg-muted text-left text-[11px] text-muted-foreground">
                            <th className="w-7 py-2 pl-3 pr-0 font-normal">
                              <Checkbox
                                aria-label={t("servers:select_page")}
                                checked={pageAllSelected}
                                disabled={pageRows.length === 0 || progress !== null}
                                onChange={(e) => select(pageRows.map((s) => s.name), e.target.checked)}
                              />
                            </th>
                            <th className="px-2.5 py-2 font-normal 2xl:px-3">{t("fleet_col_server")}</th>
                            <th className="px-2.5 py-2 font-normal 2xl:px-3">{t("fleet_col_status")}</th>
                            {isAdmin && showOwnership && <th className="px-2.5 py-2 font-normal 2xl:px-3">{t("fleet_col_owner")}</th>}
                            <th className="w-[1%] px-2.5 py-2 font-normal 2xl:px-3">{t("fleet_col_players")}</th>
                            <th className={cn("w-[1%] px-2.5 py-2 font-normal 2xl:px-3", isAdmin && "hidden xl:table-cell")}>{t("servers:usage_cpu")} / {t("servers:usage_memory")}</th>
                            {isAdmin && <th className="hidden w-[1%] px-2.5 py-2 font-normal min-[1400px]:table-cell 2xl:px-3">{t("fleet_col_endpoint")}</th>}
                            <th className="w-[1%] px-3 py-2 text-right font-normal 2xl:px-3.5">
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
                              teamMode={teamMode}
                              select={rowSelection(s)}
                              onChanged={reload}
                            />
                          ))}
                        </tbody>
                      </table>
                    </div>
                  </>
                )}
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

function ClaimAction({
  server,
  onChanged,
  inline = false,
}: {
  server: UnifiedServer;
  onChanged: () => void;
  inline?: boolean;
}) {
  const { t: ts } = useTranslation("servers");
  const [busy, setBusy] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function confirmClaim() {
    setBusy(true);
    setError(null);
    try {
      await api.claim(server.name);
      onChanged();
      setConfirmOpen(false);
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      {inline ? (
        <button
          type="button"
          disabled={busy}
          onClick={() => setConfirmOpen(true)}
          className="rounded px-1.5 py-0.5 text-[11px] font-medium text-primary/90 transition-[color,background-color,transform] duration-150 hover:bg-primary/12 hover:text-primary active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:opacity-45"
        >
          {ts("claim")}
        </button>
      ) : (
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={() => setConfirmOpen(true)}
          className="border-primary/35 text-primary hover:bg-primary/10 hover:text-primary"
        >
          <Hand /> {ts("claim")}
        </Button>
      )}

      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{ts("claim_server_title")}</DialogTitle>
            <DialogDescription>
              {ts("claim_server_desc", { name: server.displayName || server.name })}
            </DialogDescription>
          </DialogHeader>
          {error && (
            <p role="alert" className="text-xs text-destructive">
              {error}
            </p>
          )}
          <ConfirmFooter
            onCancel={() => setConfirmOpen(false)}
            onConfirm={confirmClaim}
            disabled={busy}
            loading={busy}
            cancelLabel={ts("access_cancel")}
            confirmLabel={ts("claim")}
            confirmVariant="default"
          />
        </DialogContent>
      </Dialog>
    </>
  );
}

// ServerActions is the action cluster of one server — claim, start/stop, console —
// shared by the desktop table row and the phone card so both offer the same things.
function ServerActions({
  server,
  isAdmin,
  teamMode,
  onChanged,
  compact = false,
  className,
}: {
  server: UnifiedServer;
  isAdmin: boolean;
  teamMode: boolean;
  onChanged: () => void;
  compact?: boolean;
  className?: string;
}) {
  const { t: ts } = useTranslation("servers");
  const cfg = useConfig();

  if (server.system) {
    // Staff customize system services in the dedicated lobby view.
    return (
      <div className={cn("flex justify-end", className)}>
        {isAdmin ? (
          <Button asChild variant="outline" size="sm">
            <Link to={`/admin/lobby?space=${server.name}`}>{server.name === "login" ? <BookOpen /> : <DoorOpen />}{ts(server.name === "login" ? "system_customize_login" : "system_customize")}</Link>
          </Button>
        ) : (
          <span className="text-sm text-muted-foreground" title={ts(cfg?.noLobby ? "system_service_hint_no_lobby" : "system_service_hint")}>{ts("system_service")}</span>
        )}
      </div>
    );
  }

  // Unclaimed servers expose claim only, except administrators retain wake and console access.
  const claimable = !teamMode && server.claimable && !server.owned;
  // In compact admin rows, Claim lives inline beside "Unclaimed" in the Owner column.
  const showClaimHere = claimable && !(compact && isAdmin);
  const showMigrate = isAdmin && cfg?.distributed;
  const showPower = !claimable || isAdmin;
  const showConsole = server.owned || isAdmin;
  const hasLeadingActions = Boolean(showMigrate || showClaimHere || showPower);

  return (
    <div className={cn("flex flex-col items-end gap-1", className)}>
      <div
        className={cn(
          "flex items-center justify-end",
          compact ? "flex-nowrap gap-1.5" : "flex-wrap gap-1.5",
        )}
      >
        {showMigrate && <MigrationDialog name={server.name} nodeName={server.nodeName} stopped={server.phase === "Stopped" && !server.ready && server.desiredState === "Stopped" && !server.retiring} onChanged={onChanged} />}
        {showClaimHere && <ClaimAction server={server} onChanged={onChanged} />}
        {showPower && (
          <PowerButton
            name={server.name}
            phase={server.phase}
            desiredState={server.desiredState}
            failed={startFailure(server) !== null}
            playersOnline={server.playersOnline}
            playerCountUnknown={server.playerCountUnknown}
            retiring={server.retiring}
            diskFull={server.diskFull}
            quiet={compact}
            onChanged={onChanged}
          />
        )}
        {compact && hasLeadingActions && showConsole && (
          <span aria-hidden="true" className="h-3.5 w-px shrink-0 bg-border/70" />
        )}
        {showConsole && (
          <Link to={`/servers/${server.name}`}>
            <Button size="sm" variant="outline" className={compact ? "h-7 px-2.5 text-xs" : undefined}>
              <Terminal /> {ts("console")}
            </Button>
          </Link>
        )}
      </div>
    </div>
  );
}

function OwnerLabel({
  server,
  onChanged,
  inlineClaim = false,
}: {
  server: UnifiedServer;
  onChanged?: () => void;
  inlineClaim?: boolean;
}) {
  const { t } = useTranslation("ops");
  const { t: ts } = useTranslation("servers");
  const noLobby = useConfig()?.noLobby === true;
  if (server.system) {
    return (
      <span className="text-muted-foreground/70" title={ts(noLobby ? "system_service_hint_no_lobby" : "system_service_hint")}>
        {ts("system_service")}
      </span>
    );
  }
  if (server.owner) {
    return (
      <span className="inline-flex min-w-0 max-w-[8rem] items-center gap-1.5 xl:max-w-[11rem] min-[1400px]:max-w-[13rem] 2xl:max-w-[16rem]">
        {server.owned && (
          <span className="shrink-0 rounded bg-primary/12 px-1 text-[10px] font-medium leading-4 text-primary">
            {t("fleet_owner_you")}
          </span>
        )}
        <span className="truncate text-foreground/85" title={server.owner}>
          {server.owner}
        </span>
      </span>
    );
  }
  if (server.ownerUnknown) {
    return (
      <span className="text-muted-foreground/65" title={t("fleet_owner_unknown_hint")}>
        {t("fleet_owner_unknown")}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="text-muted-foreground/55">{t("fleet_unclaimed")}</span>
      {inlineClaim && server.claimable && !server.owned && onChanged && (
        <ClaimAction server={server} onChanged={onChanged} inline />
      )}
    </span>
  );
}

function PlayersLabel({ server }: { server: UnifiedServer }) {
  if (!isLive(server.phase)) return <span className="text-muted-foreground/35">—</span>;
  return (
    <span className="inline-flex items-baseline gap-1 whitespace-nowrap tabular-nums">
      <span className="font-medium text-foreground">{server.playersOnline}</span>
      <span className="text-[11px] text-muted-foreground/65">/ {server.playersMax}</span>
    </span>
  );
}

function EndpointLabel({ server }: { server: UnifiedServer }) {
  return <div className="space-y-0.5">{server.nodeName && <code className="block text-[11px] text-muted-foreground">{server.nodeName}</code>}<EndpointAddress server={server} /></div>;
}

function EndpointAddress({ server }: { server: UnifiedServer }) {
  const { t } = useTranslation("ops");
  const endpoint = server.ready && server.endpointAddress ? server.endpointAddress : null;
  if (!endpoint) {
    return <span className="text-muted-foreground/40">{t("fleet_endpoint_idle")}</span>;
  }
  return (
    <code className="break-all font-mono text-[11.5px] text-foreground/80 xl:whitespace-nowrap xl:break-normal">
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

function TagChips({ tags, inline = false }: { tags?: string[]; inline?: boolean }) {
  const { t } = useTranslation("servers");
  if (!tags?.length) return null;
  return (
    <ul aria-label={t("tags_label")} className={cn("flex shrink-0 flex-wrap gap-1", !inline && "mt-1")}>
      {tags.map((tag) => (
        <li
          key={tag}
          title={tag}
          className="max-w-full truncate rounded bg-secondary/70 px-1.5 text-[10.5px] leading-4 text-muted-foreground"
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
  teamMode,
  select,
  onChanged,
}: {
  server: UnifiedServer;
  cfg: RuntimeConfig;
  isAdmin: boolean;
  teamMode: boolean;
  select?: RowSelection;
  onChanged: () => void;
}) {
  const { t } = useTranslation("ops");
  const address = server.subdomain ? joinAddress(server.subdomain, cfg) : "";

  const cpu = cpuUsage(server.cpuMilli, server.cpu);
  const mem = memoryUsage(server.memoryBytes, server.memory);

  return (
    <tr className="group border-b border-border/50 transition-colors last:border-0 hover:bg-accent/40">
      <td className="w-7 py-2.5 pl-3 pr-0 align-middle">
        <RowSelect server={server} select={select} />
      </td>
      <td className="px-2.5 py-2.5 text-left align-middle 2xl:px-3">
        <div className="flex min-w-0 max-w-[14rem] items-center gap-x-1.5 gap-y-0.5 xl:max-w-[18rem] 2xl:max-w-[26rem]">
          <ServerName server={server} isAdmin={isAdmin} compactSlug={isAdmin} />
          <TagChips tags={server.tags} inline />
        </div>
        {address && (
          <CopyAddress
            address={address}
            className={cn("mt-0.5 flex-nowrap", isAdmin ? "max-w-[13rem] xl:max-w-[17rem] 2xl:max-w-[24rem]" : "max-w-[24rem]")}
          />
        )}
      </td>
      <td className="whitespace-nowrap px-2.5 py-2.5 text-left align-middle 2xl:px-3">
        <PhaseBadge phase={shownPhase(server)} failure={startFailure(server)} autoRestarts={server.autoRestarts} />
        {server.autostartPolicy && (
          <div className="mt-0.5 text-[10.5px] text-muted-foreground/55">
            {t(policyLabelKey(server.autostartPolicy, teamMode))}
          </div>
        )}
      </td>
      {isAdmin && !teamMode && (
        <td className="whitespace-nowrap px-2.5 py-2.5 text-left align-middle text-muted-foreground 2xl:px-3">
          <OwnerLabel server={server} onChanged={onChanged} inlineClaim />
        </td>
      )}
      <td className="whitespace-nowrap px-2.5 py-2.5 text-left align-middle font-mono 2xl:px-3">
        <PlayersLabel server={server} />
      </td>
      <td className={cn("whitespace-nowrap px-2.5 py-2.5 font-mono text-muted-foreground tabular-nums 2xl:px-3", isAdmin && "hidden xl:table-cell")}>
        {cpu === "—" && mem === "—" ? (
          <span className="text-muted-foreground/35">—</span>
        ) : (
          <div className="flex flex-col leading-snug">
            <span className="text-foreground/90">{cpu}</span>
            <span className="text-[11px] text-muted-foreground/65">{mem}</span>
          </div>
        )}
      </td>
      {isAdmin && (
        <td className="hidden whitespace-nowrap px-2.5 py-2.5 text-left align-middle min-[1400px]:table-cell 2xl:px-3">
          <EndpointLabel server={server} />
        </td>
      )}
      <td className="whitespace-nowrap px-3 py-2.5 align-middle 2xl:px-3.5">
        <ServerActions server={server} isAdmin={isAdmin} teamMode={teamMode} onChanged={onChanged} compact />
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
  teamMode,
  select,
  onChanged,
}: {
  server: UnifiedServer;
  cfg: RuntimeConfig;
  isAdmin: boolean;
  teamMode: boolean;
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
                  <ServerName server={server} isAdmin={isAdmin} />
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
            {isAdmin && !teamMode && (
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
                <dd className="text-xs">{t(policyLabelKey(server.autostartPolicy, teamMode))}</dd>
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
            <ServerActions server={server} isAdmin={isAdmin} teamMode={teamMode} onChanged={onChanged} />
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
          <Button size="sm" variant="outline" disabled={busy} onClick={onClear}>
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
            <Button size="sm" variant="outline" onClick={onDismiss}>
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
function ServerName({
  server,
  isAdmin,
  compactSlug = false,
}: {
  server: UnifiedServer;
  isAdmin: boolean;
  compactSlug?: boolean;
}) {
  const navigate = useNavigate();
  const label = server.displayName || server.name;
  const clickable = !server.system && (server.owned || isAdmin);
  return (
    <>
      {clickable ? (
        <span
          onClick={() => navigate(`/servers/${server.name}`)}
          className="cursor-pointer truncate font-medium text-foreground transition-colors hover:text-primary hover:underline"
        >
          {label}
        </span>
      ) : (
        <span className="truncate font-medium text-foreground">{label}</span>
      )}
      {label !== server.name && (
        <span className={cn("truncate font-mono text-[11px] text-muted-foreground/55", compactSlug && "hidden 2xl:inline")}>
          {server.name}
        </span>
      )}
    </>
  );
}
