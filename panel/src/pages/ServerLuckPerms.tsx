import { useState } from "react";
import { useParams } from "react-router-dom";
import { Shield, Users, Search, Plus, Trash2, Undo, History, Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { BackLink } from "@/components/BackLink";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { PhaseBadge, shownPhase, startFailure } from "@/components/PhaseBadge";
import { Loading, ErrorState, NotYours, NotRunning, RefreshError } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { STATUS_POLL_SLOW_MS, STATUS_POLL_FAST_MS, useAsync, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage, ownershipPending } from "@/lib/ownership";
import { cn } from "@/lib/utils";

const MC_NAME = /^[A-Za-z0-9_]{1,16}$/;
const LP_NODE = /^[A-Za-z0-9_.*-]{1,64}$/;
const LP_CTX = /^[A-Za-z0-9_-]{1,48}$/;

const PRESET_GROUPS = ["default", "vip", "vip+", "mvp", "admin", "moderator", "helper", "builder"];
const PRESET_PERMS = [
  "essentials.fly",
  "essentials.tpa",
  "essentials.home",
  "essentials.warp",
  "essentials.back",
  "luckperms.use",
  "minecraft.command.gamemode",
];

interface ActionHistoryItem {
  id: string;
  timestamp: string;
  type: "group" | "permission";
  player: string;
  action: string;
  target: string;
  value?: boolean;
  world?: string;
  // undo is the exact write that puts a permission back as it was before; absent
  // when that state is unknown, so the entry offers no revert (a guess would turn
  // a removed deny into a grant). Group entries invert their action instead.
  undo?: { action: "set" | "unset"; value?: boolean };
  status: "success" | "error";
  output: string;
  reverted?: boolean;
}

export function ServerLuckPerms() {
  const { name = "" } = useParams();
  const { t } = useTranslation("servers");
  const { isAdmin, loading: tierLoading } = useTier();
  
  // Server Status
  const { data, error, loading, reload } = useAsync(() => api.status(name), [name]);
  // The poll is what switches this page over once a wake lands, and back to
  // NotRunning when the server stops (idle stop, the console, a player's timeout).
  usePolling(reload, data && shownPhase(data) === "Running" ? STATUS_POLL_SLOW_MS : STATUS_POLL_FAST_MS);
  
  // Ownership verification
  const {
    data: mine,
    error: mineError,
    reload: reloadMine,
  } = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name]
  );

  // Online Players, read once the server is running: a page opened on a sleeping
  // server fills the list when the poll sees it come up.
  const running = data?.phase === "Running";
  const { data: playersData, loading: playersLoading } = useAsync(
    () => (running ? api.accessPlayers(name) : Promise.resolve(null)),
    [name, running]
  );
  const onlinePlayers = playersData?.players ?? [];

  // Selected Player Profile State
  const [selectedPlayer, setSelectedPlayer] = useState<string>("");
  const [searchQuery, setSearchQuery] = useState("");

  // LuckPerms Profile data for selected player
  const { data: lpInfo, error: lpError, loading: lpLoading, reload: reloadLp } = useAsync(
    () => selectedPlayer ? api.accessLuckPermsInfo(name, selectedPlayer) : Promise.resolve(null),
    [name, selectedPlayer]
  );

  // LuckPerms does not reply over RCON (its commands finish asynchronously, after
  // the response is flushed) — verified live on 5.5.85: every `lp` command
  // returns an empty body while `list`/`plugins` answer normally. A silent,
  // entry-less reply therefore means "could not read", NOT "no groups / no
  // permissions": the two projection panels must not claim the latter on their
  // own. The writes still work, so the page keeps offering them.
  const lpSilent =
    !!lpInfo &&
    !(lpInfo.output ?? "").trim() &&
    (lpInfo.groups?.length ?? 0) === 0 &&
    (lpInfo.permissions?.length ?? 0) === 0;
  // Unread: the lists below know nothing, so they say so instead of "none", and a
  // group or node is removed by typing it — a read row is the only other way in,
  // and on LuckPerms 5.5 there never is one.
  const lpUnread = lpSilent || !!lpError;

  // Form State
  const [groupNameInput, setGroupNameInput] = useState("");
  const [permNodeInput, setPermNodeInput] = useState("");
  const [permValueInput, setPermValueInput] = useState(true);
  const [permWorldInput, setPermWorldInput] = useState("");

  // Feedback & History State
  const [submitting, setSubmitting] = useState(false);
  const [formFeedback, setFormFeedback] = useState<{ kind: "ok" | "err"; msg: string } | null>(null);
  const [history, setHistory] = useState<ActionHistoryItem[]>([]);

  const addHistoryItem = (item: Omit<ActionHistoryItem, "id" | "timestamp">) => {
    const newItem: ActionHistoryItem = {
      ...item,
      id: Math.random().toString(36).substring(2, 9),
      timestamp: new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }),
    };
    setHistory((prev) => [newItem, ...prev].slice(0, 30));
  };

  const clearHistory = () => {
    setHistory([]);
  };

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault();
    const query = searchQuery.trim();
    if (query && MC_NAME.test(query)) {
      setSelectedPlayer(query);
      setFormFeedback(null);
    } else {
      setFormFeedback({ kind: "err", msg: t("luckperms_error_invalid_player") });
    }
  };

  const handleAddGroup = async (groupName: string) => {
    if (!selectedPlayer || submitting) return;
    setSubmitting(true);
    setFormFeedback(null);
    try {
      const res = await api.accessGroup(name, "add", selectedPlayer, groupName);
      addHistoryItem({
        type: "group",
        player: selectedPlayer,
        action: "add",
        target: groupName,
        status: "success",
        output: res.output || t("luckperms_no_output"),
      });
      reloadLp();
    } catch (err) {
      setFormFeedback({ kind: "err", msg: humanizeError(err) });
    } finally {
      setSubmitting(false);
    }
  };

  // Returns whether the server took the removal, so the typed form can clear itself.
  const handleRemoveGroup = async (groupName: string): Promise<boolean> => {
    if (!selectedPlayer || submitting) return false;
    setSubmitting(true);
    setFormFeedback(null);
    try {
      const res = await api.accessGroup(name, "remove", selectedPlayer, groupName);
      addHistoryItem({
        type: "group",
        player: selectedPlayer,
        action: "remove",
        target: groupName,
        status: "success",
        output: res.output || t("luckperms_no_output"),
      });
      reloadLp();
      return true;
    } catch (err) {
      setFormFeedback({ kind: "err", msg: humanizeError(err) });
      return false;
    } finally {
      setSubmitting(false);
    }
  };

  // typedPermission is the node and world the form holds, or null (with the reason
  // shown) when either is malformed.
  const typedPermission = (): { node: string; world: string } | null => {
    const node = permNodeInput.trim();
    const world = permWorldInput.trim();
    if (!LP_NODE.test(node)) {
      setFormFeedback({ kind: "err", msg: t("luckperms_error_invalid_node") });
      return null;
    }
    if (world !== "" && !LP_CTX.test(world)) {
      setFormFeedback({ kind: "err", msg: t("luckperms_error_invalid_world") });
      return null;
    }
    return { node, world };
  };

  // priorValue is the value the last read showed for a node in a world (LuckPerms
  // matches both case-insensitively), or undefined when no row showed one: the
  // node was unset, or nothing could be read. A read still in flight or one whose
  // refresh failed is stale (it predates the last write), so it knows nothing.
  const priorValue = (node: string, world: string): boolean | undefined => {
    if (lpUnread || lpLoading) return undefined;
    return lpInfo?.permissions?.find(
      (p) => p.node.toLowerCase() === node.toLowerCase() && (p.world ?? "").toLowerCase() === world.toLowerCase(),
    )?.value;
  };

  const handleAddPermission = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedPlayer || !permNodeInput.trim() || submitting) return;
    const typed = typedPermission();
    if (!typed) return;
    const { node, world } = typed;
    // Reverting a set restores the value the node had; with none read, it removes
    // the node, which undoes the set whenever the node was not there before.
    const prior = priorValue(node, world);

    setSubmitting(true);
    setFormFeedback(null);
    try {
      const res = await api.accessPermission(name, "set", selectedPlayer, node, permValueInput, world || undefined);
      addHistoryItem({
        type: "permission",
        player: selectedPlayer,
        action: "set",
        target: node,
        value: permValueInput,
        world: world || undefined,
        undo: prior === undefined ? { action: "unset" } : { action: "set", value: prior },
        status: "success",
        output: res.output || t("luckperms_no_output"),
      });
      setPermNodeInput("");
      setPermWorldInput("");
      reloadLp();
    } catch (err) {
      setFormFeedback({ kind: "err", msg: humanizeError(err) });
    } finally {
      setSubmitting(false);
    }
  };

  // Returns whether the server took the unset, so the typed form can clear itself.
  // value is what the node held, when known; only then can the removal be reverted.
  const handleRemovePermission = async (node: string, world?: string, value?: boolean): Promise<boolean> => {
    if (!selectedPlayer || submitting) return false;
    setSubmitting(true);
    setFormFeedback(null);
    try {
      const res = await api.accessPermission(name, "unset", selectedPlayer, node, undefined, world || undefined);
      addHistoryItem({
        type: "permission",
        player: selectedPlayer,
        action: "unset",
        target: node,
        value,
        world: world || undefined,
        undo: value === undefined ? undefined : { action: "set", value },
        status: "success",
        output: res.output || t("luckperms_no_output"),
      });
      reloadLp();
      return true;
    } catch (err) {
      setFormFeedback({ kind: "err", msg: humanizeError(err) });
      return false;
    } finally {
      setSubmitting(false);
    }
  };

  // The typed removal: the node and world as the form holds them, whatever value
  // the node was set to (LuckPerms unsets a node in a context regardless of it).
  const handleRemoveTypedPermission = async () => {
    if (!selectedPlayer || !permNodeInput.trim() || submitting) return;
    const typed = typedPermission();
    if (!typed) return;
    if (await handleRemovePermission(typed.node, typed.world, priorValue(typed.node, typed.world))) {
      setPermNodeInput("");
      setPermWorldInput("");
    }
  };

  const handleRevert = async (item: ActionHistoryItem) => {
    if (submitting || item.reverted) return;
    setSubmitting(true);
    setFormFeedback(null);

    try {
      if (item.type === "group") {
        const inverseAction = item.action === "add" ? "remove" : "add";
        await api.accessGroup(name, inverseAction, item.player, item.target);
      } else {
        const undo = item.undo;
        if (!undo) return;
        await api.accessPermission(
          name,
          undo.action,
          item.player,
          item.target,
          undo.action === "set" ? undo.value : undefined,
          item.world || undefined
        );
      }

      // Mark reverted
      setHistory((prev) =>
        prev.map((h) => (h.id === item.id ? { ...h, reverted: true } : h))
      );
      setFormFeedback({ kind: "ok", msg: t("luckperms_revert_success") });
      if (selectedPlayer === item.player) {
        reloadLp();
      }
    } catch (err) {
      setFormFeedback({ kind: "err", msg: t("luckperms_revert_failed", { reason: humanizeError(err) }) });
    } finally {
      setSubmitting(false);
    }
  };

  const back = (
    <BackLink to={`/servers/${name}`} label={t("luckperms_back_to_console")} />
  );

  if (loading && !data) {
    return (
      <>
        {back}
        <Loading />
      </>
    );
  }
  if (error && !data) {
    return (
      <>
        {back}
        <ErrorState error={error} onRetry={reload} />
      </>
    );
  }
  if (!data) return back;

  const pending = ownershipPending(tierLoading, isAdmin, mine, mineError);
  const owned = canManage(isAdmin, mine, name);
  const phase = shownPhase(data);
  const failure = startFailure(data);

  const header = (
    <PageHeader
      icon={Shield}
      title={data.displayName || data.name}
      subtitle={t("luckperms_desc")}
      actions={<PhaseBadge phase={phase} failure={failure} autoRestarts={data.autoRestarts} />}
      className="mb-6"
    />
  );

  return (
    <>
      {back}
      {header}
      {error && <RefreshError error={error} className="mb-4" />}
      {pending ? (
        <Loading />
      ) : mineError ? (
        <ErrorState error={mineError} onRetry={reloadMine} />
      ) : !owned ? (
        <NotYours title={t("players_not_yours_title")} body={t("players_not_yours_body")} />
      ) : phase !== "Running" ? (
        <NotRunning
          title={t("players_not_running_title")}
          body={t("players_not_running_body")}
          serverName={name}
          phase={data.phase}
          desiredState={data.desiredState}
          failure={failure}
          autoRestarts={data.autoRestarts}
          retiring={data.retiring}
          onWoken={reload}
        />
      ) : (
        <div className="grid grid-cols-1 lg:grid-cols-4 gap-6 items-start">
          {/* Left Directory Sidebar: Single card for Search + Online Players. On a
              wide screen it fills the column; on a phone it fits its content and the
              list scrolls inside it, so the player's panel below is on the first screen. */}
          <Card className="lg:col-span-1 flex flex-col lg:h-[calc(100vh-14rem)] lg:min-h-[500px] lg:max-h-[700px] overflow-hidden">
            <CardHeader className="shrink-0">
              <CardTitle className="flex items-center gap-2">
                <Users className="h-4 w-4 text-primary" />
                {t("luckperms_batch_players")}
              </CardTitle>
            </CardHeader>
            <div className="p-3 shrink-0">
              <form onSubmit={handleSearch} className="relative">
                <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
                <Input
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                  placeholder="Steve"
                  aria-label={t("luckperms_player_lookup")}
                  className="pl-9 font-mono"
                  autoComplete="off"
                  disabled={submitting}
                />
              </form>
            </div>
            <div className="px-4 py-1 shrink-0 bg-muted/40 border-y border-border/30">
              <span className="text-[10px] font-bold text-muted-foreground uppercase tracking-wider">
                {t("access_online_title")}
              </span>
            </div>
            <CardContent className="p-2 overflow-y-auto max-h-48 lg:max-h-none flex-1 space-y-0.5 min-h-0">
              {playersLoading && onlinePlayers.length === 0 ? (
                <div className="flex justify-center py-6">
                  <Loader2 className="h-5 w-5 animate-spin text-primary" />
                </div>
              ) : onlinePlayers.length === 0 ? (
                <p className="text-xs text-muted-foreground/60 text-center py-8 italic select-none">
                  {t("access_online_empty")}
                </p>
              ) : (
                onlinePlayers.map((player) => (
                  <button
                    key={player}
                    type="button"
                    onClick={() => {
                      setSelectedPlayer(player);
                      setSearchQuery(player);
                      setFormFeedback(null);
                    }}
                    className={cn(
                      "w-full text-left px-3 py-2 rounded-md text-xs font-mono transition-all flex items-center justify-between border focus:outline-none",
                      selectedPlayer === player
                        ? "bg-primary/10 border-primary/20 text-primary font-semibold"
                        : "hover:bg-muted/40 text-muted-foreground bg-transparent border-transparent"
                    )}
                  >
                    <span>{player}</span>
                    {selectedPlayer === player && (
                      <div className="h-1.5 w-1.5 rounded-full bg-primary" />
                    )}
                  </button>
                ))
              )}
            </CardContent>
          </Card>

          {/* Right Main Panel */}
          <div className="lg:col-span-3 space-y-6">
            {!selectedPlayer ? (
              <Card className="flex flex-col items-center justify-center py-8 lg:py-24 text-center border-dashed lg:min-h-[500px]">
                <Shield className="h-14 w-14 text-muted-foreground/15 mb-4 animate-pulse" />
                <p className="text-sm text-muted-foreground font-medium max-w-xs leading-normal">
                  {t("luckperms_select_player_prompt")}
                </p>
              </Card>
            ) : (
              <>
                {/* Clean player heading */}
                <div className="pb-3 border-b border-border/60">
                  <span className="text-2xl font-bold font-mono text-foreground tracking-tight">
                    {selectedPlayer}
                  </span>
                </div>

                {/* Ground truth: the raw RCON reply behind the two projection panels.
                    The parse is LuckPerms-format-specific — on a server without the
                    plugin it yields zero entries, and without this disclosure the
                    panels would present that as "no groups / no permissions" while
                    the real reply ("Unknown or incomplete command…") stays invisible.
                    Same disclosure the whitelist/ban/online rosters carry. */}
                {lpInfo?.output?.trim() && (
                  <details className="group/details">
                    <summary className="cursor-pointer select-none text-[10px] text-muted-foreground/70 hover:text-foreground font-mono transition-colors list-none flex items-center gap-1">
                      <span className="transition-transform group-open/details:rotate-90">▶</span>
                      {t("luckperms_rcon_output")}
                    </summary>
                    <pre className="mt-1.5 p-2 rounded bg-muted/60 border border-border/80 font-mono text-[10px] text-foreground/80 overflow-x-auto whitespace-pre-wrap break-all max-h-24">
                      {lpInfo.output}
                    </pre>
                  </details>
                )}

                {/* A read that failed (no LuckPerms on the server, console down) says
                    why here; the panels below would otherwise sit empty with no cause. */}
                {lpError && (
                  <div role="alert" className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-xs text-destructive">
                    {humanizeError(lpError)}
                  </div>
                )}

                {lpSilent && (
                  <div className="rounded-md border border-dashed border-border bg-muted/30 p-3 text-xs text-muted-foreground">
                    {t("luckperms_no_reply")}
                  </div>
                )}

                {/* Operations Feedback */}
                {formFeedback && (
                  <div role={formFeedback.kind === "ok" ? "status" : "alert"} className={cn(
                    "p-3 rounded-md text-xs whitespace-pre-wrap break-all border transition-all animate-in fade-in duration-200",
                    formFeedback.kind === "ok"
                      ? "bg-emerald-500/10 border-emerald-500/30 text-emerald-600 dark:text-emerald-400"
                      : "bg-destructive/10 border-destructive/30 text-destructive"
                  )}>
                    {formFeedback.msg}
                  </div>
                )}

                {/* Groups Management Card */}
                <Card>
                  <CardHeader className="shrink-0">
                    <CardTitle>
                      {t("luckperms_groups_title")}
                    </CardTitle>
                  </CardHeader>
                  <CardContent className="space-y-4">
                    {lpLoading && !lpInfo ? (
                      <div className="flex justify-center py-4">
                        <Loader2 className="h-6 w-6 animate-spin text-primary" />
                      </div>
                    ) : (
                      <div className={cn("space-y-4 transition-opacity", lpLoading && "opacity-60 pointer-events-none")}>
                        <div className="flex flex-wrap gap-1.5">
                          {lpInfo?.groups && lpInfo.groups.length > 0 ? (
                            lpInfo.groups.map((group) => (
                              <Badge
                                key={group}
                                variant="muted"
                                className="font-mono text-xs py-1.5 pl-3 pr-2 gap-1.5 flex items-center border border-border/80 bg-muted/40 text-foreground hover:bg-muted"
                              >
                                {group}
                                <button
                                  type="button"
                                  onClick={() => handleRemoveGroup(group)}
                                  disabled={submitting}
                                  aria-label={t("luckperms_remove_group", { group })}
                                  title={t("luckperms_remove_group", { group })}
                                  className="text-muted-foreground hover:text-destructive hover:bg-muted p-0.5 rounded transition-all focus:outline-none"
                                >
                                  <Trash2 className="h-3.5 w-3.5" />
                                </button>
                              </Badge>
                            ))
                          ) : lpUnread ? (
                            <p className="text-xs text-muted-foreground py-1">{t("luckperms_groups_unread")}</p>
                          ) : (
                            <p className="text-xs text-muted-foreground/60 italic py-1">{t("luckperms_no_parent_groups")}</p>
                          )}
                        </div>

                        <div className="border-t border-border/50 pt-4 flex flex-col sm:flex-row gap-3 items-end">
                          <div className="grid gap-2 flex-1 w-full">
                            <Label htmlFor="lp-group-name">
                              {t("luckperms_group_name")}
                            </Label>
                            <div className="flex gap-2">
                              <Input
                                id="lp-group-name"
                                placeholder={t("luckperms_custom_group_placeholder")}
                                value={groupNameInput}
                                onChange={(e) => setGroupNameInput(e.target.value)}
                                disabled={submitting}
                              />
                              <Select
                                value=""
                                onValueChange={(v) => setGroupNameInput(v)}
                                disabled={submitting}
                              >
                                <SelectTrigger className="w-[120px]" aria-label={t("luckperms_presets")}>
                                  <span>{t("luckperms_presets")}</span>
                                </SelectTrigger>
                                <SelectContent className="border-border">
                                  {PRESET_GROUPS.map((g) => (
                                    <SelectItem key={g} value={g}>{g}</SelectItem>
                                  ))}
                                </SelectContent>
                              </Select>
                            </div>
                          </div>
                          <div className="flex gap-2 w-full sm:w-auto">
                            <Button
                              size="sm"
                              onClick={() => {
                                const val = groupNameInput.trim();
                                if (val) {
                                  handleAddGroup(val);
                                  setGroupNameInput("");
                                }
                              }}
                              disabled={submitting || !groupNameInput.trim()}
                              className="h-9 px-4 shrink-0 flex-1 sm:flex-none hover:bg-primary/90 transition-colors"
                            >
                              <Plus className="h-4 w-4 mr-1" />
                              {t("luckperms_add_group_btn")}
                            </Button>
                            <Button
                              size="sm"
                              variant="outline"
                              onClick={async () => {
                                const val = groupNameInput.trim();
                                if (val && (await handleRemoveGroup(val))) {
                                  setGroupNameInput("");
                                }
                              }}
                              disabled={submitting || !groupNameInput.trim()}
                              className="h-9 px-4 shrink-0 flex-1 sm:flex-none hover:text-destructive transition-colors"
                            >
                              <Trash2 className="h-4 w-4 mr-1" />
                              {t("luckperms_remove_group_btn")}
                            </Button>
                          </div>
                        </div>
                      </div>
                    )}
                  </CardContent>
                </Card>

                {/* Permissions Management Card */}
                <Card>
                  <CardHeader className="shrink-0">
                    <CardTitle>
                      {t("luckperms_permissions_title")}
                    </CardTitle>
                  </CardHeader>
                  <CardContent className="space-y-4">
                    {lpLoading && !lpInfo ? (
                      <div className="flex justify-center py-4">
                        <Loader2 className="h-6 w-6 animate-spin text-primary" />
                      </div>
                    ) : (
                      <div className={cn("space-y-4 transition-opacity", lpLoading && "opacity-60 pointer-events-none")}>
                        <div className="rounded-md border border-border bg-muted/10 overflow-hidden">
                          <div className="overflow-x-auto">
                            <table className="w-full text-left border-collapse text-xs">
                              <thead>
                                <tr className="border-b border-border bg-muted/30 font-semibold text-muted-foreground">
                                  <th className="px-4 py-3">{t("luckperms_node_column")}</th>
                                  <th className="px-4 py-3 w-[90px] text-center">{t("luckperms_value_column")}</th>
                                  <th className="px-4 py-3 w-[120px]">{t("luckperms_world_column")}</th>
                                  <th className="px-4 py-3 w-[70px] text-center">{t("luckperms_actions_column")}</th>
                                </tr>
                              </thead>
                              <tbody>
                                {lpInfo?.permissions && lpInfo.permissions.length > 0 ? (
                                  lpInfo.permissions.map((p, idx) => (
                                    <tr
                                      key={`${p.node}-${p.world || "global"}-${idx}`}
                                      className="border-b border-border/40 last:border-0 hover:bg-muted/20 transition-colors"
                                    >
                                      <td className="px-4 py-2.5 font-mono break-all font-medium text-foreground">{p.node}</td>
                                      <td className="px-4 py-2.5 text-center">
                                        <Badge
                                          variant={p.value ? "default" : "destructive"}
                                          className={cn(
                                            "text-[10px] py-0.5 px-2 font-bold font-mono tracking-wide border",
                                            p.value
                                              ? "border-emerald-500/20 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 hover:bg-emerald-500/15"
                                              : "border-destructive/20 bg-destructive/10 text-destructive hover:bg-destructive/15"
                                          )}
                                        >
                                          {p.value ? "TRUE" : "FALSE"}
                                        </Badge>
                                      </td>
                                      <td className="px-4 py-2.5">
                                        {p.world ? (
                                          <Badge
                                            variant="outline"
                                            className="font-mono text-[10px] py-0 px-1.5 border-border bg-muted/30 text-muted-foreground"
                                          >
                                            {p.world}
                                          </Badge>
                                        ) : (
                                          <span className="text-muted-foreground italic">{t("luckperms_global")}</span>
                                        )}
                                      </td>
                                      <td className="px-4 py-2.5 text-center">
                                        <Button
                                          variant="ghost"
                                          size="sm"
                                          disabled={submitting}
                                          onClick={() => handleRemovePermission(p.node, p.world, p.value)}
                                          aria-label={t("luckperms_remove_perm", { node: p.node })}
                                          title={t("luckperms_remove_perm", { node: p.node })}
                                          className="h-7 w-7 p-0 text-muted-foreground hover:text-destructive hover:bg-muted rounded transition-all focus:outline-none"
                                        >
                                          <Trash2 className="h-3.5 w-3.5" />
                                        </Button>
                                      </td>
                                    </tr>
                                  ))
                                ) : (
                                  <tr>
                                    {lpUnread ? (
                                      <td colSpan={4} className="px-4 py-10 text-center text-muted-foreground">
                                        {t("luckperms_perms_unread")}
                                      </td>
                                    ) : (
                                      <td colSpan={4} className="px-4 py-10 text-center text-muted-foreground/60 italic">
                                        {t("luckperms_no_perms")}
                                      </td>
                                    )}
                                  </tr>
                                )}
                              </tbody>
                            </table>
                          </div>
                        </div>

                        {/* Add Permission Node Inline Form */}
                        <form onSubmit={handleAddPermission} className="border-t border-border/50 pt-4 space-y-4">
                          <Label>
                            {t("luckperms_perm_form_title")}
                          </Label>
                          <div className="grid grid-cols-1 sm:grid-cols-4 gap-3.5">
                            <div className="sm:col-span-2 grid gap-2">
                              <Label htmlFor="lp-node-input">
                                {t("luckperms_node")}
                              </Label>
                              <div className="flex gap-2">
                                <Input
                                  id="lp-node-input"
                                  placeholder="essentials.fly"
                                  value={permNodeInput}
                                  onChange={(e) => setPermNodeInput(e.target.value)}
                                  disabled={submitting}
                                  autoComplete="off"
                                />
                                <Select
                                  value=""
                                  onValueChange={(v) => setPermNodeInput(v)}
                                  disabled={submitting}
                                >
                                  <SelectTrigger className="w-[100px] shrink-0" aria-label={t("luckperms_presets")}>
                                    <span>{t("luckperms_presets")}</span>
                                  </SelectTrigger>
                                  <SelectContent className="border-border">
                                    {PRESET_PERMS.map((p) => (
                                      <SelectItem key={p} value={p}>{p}</SelectItem>
                                    ))}
                                  </SelectContent>
                                </Select>
                              </div>
                            </div>

                            <div className="grid gap-2">
                              <Label htmlFor="lp-value-input">
                                {t("luckperms_value")}
                              </Label>
                              <Select
                                value={permValueInput ? "true" : "false"}
                                onValueChange={(v) => setPermValueInput(v === "true")}
                                disabled={submitting}
                              >
                                <SelectTrigger id="lp-value-input">
                                  <SelectValue />
                                </SelectTrigger>
                                <SelectContent className="border-border">
                                  <SelectItem value="true">{t("luckperms_value_grant")}</SelectItem>
                                  <SelectItem value="false">{t("luckperms_value_deny")}</SelectItem>
                                </SelectContent>
                              </Select>
                            </div>

                            <div className="grid gap-2">
                              <Label htmlFor="lp-world-input">
                                {t("luckperms_world")}
                              </Label>
                              <Input
                                id="lp-world-input"
                                placeholder="world"
                                value={permWorldInput}
                                onChange={(e) => setPermWorldInput(e.target.value)}
                                className="font-mono"
                                disabled={submitting}
                                autoComplete="off"
                              />
                            </div>
                          </div>

                          <div className="flex flex-col sm:flex-row sm:items-center gap-3 pt-1">
                            <p className="text-[11px] text-muted-foreground sm:flex-1">
                              {t("luckperms_remove_perm_hint")}
                            </p>
                            <div className="flex gap-2">
                              <Button
                                type="submit"
                                disabled={submitting || !permNodeInput.trim()}
                                className="flex-1 sm:flex-none h-9 hover:bg-primary/90 transition-colors"
                              >
                                <Plus className="h-4 w-4 mr-1" />
                                {t("luckperms_add_perm_btn")}
                              </Button>
                              <Button
                                type="button"
                                variant="outline"
                                onClick={handleRemoveTypedPermission}
                                disabled={submitting || !permNodeInput.trim()}
                                className="flex-1 sm:flex-none h-9 hover:text-destructive transition-colors"
                              >
                                <Trash2 className="h-4 w-4 mr-1" />
                                {t("luckperms_remove_perm_btn")}
                              </Button>
                            </div>
                          </div>
                        </form>
                      </div>
                    )}
                  </CardContent>
                </Card>
              </>
            )}

            {/* Action History Log */}
            <Card>
              <CardHeader className="shrink-0">
                <div className="flex items-center justify-between">
                  <CardTitle className="flex items-center gap-1.5">
                    <History className="h-4 w-4 text-primary" />
                    {t("luckperms_recent_actions")}
                  </CardTitle>
                  {history.length > 0 && (
                    <button
                      type="button"
                      onClick={clearHistory}
                      className="text-muted-foreground hover:text-destructive transition-colors focus:outline-none"
                      aria-label={t("luckperms_clear_history")}
                      title={t("luckperms_clear_history")}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </button>
                  )}
                </div>
              </CardHeader>
              <CardContent className="pt-3">
                {history.length === 0 ? (
                  <p className="text-xs text-muted-foreground/60 py-4 italic select-none text-center">
                    {t("luckperms_no_recent_actions")}
                  </p>
                ) : (
                  <div className="divide-y divide-border/60 max-h-[320px] overflow-y-auto pr-1">
                    {history.map((h) => {
                      const isSuccess = h.status === "success";
                      const shownValue = h.value === undefined ? "" : ` (${h.value ? "TRUE" : "FALSE"})`;
                      const actionLabel = h.type === "group"
                        ? `${h.action === "add" ? "+" : "-"} ${t("luckperms_group_name")}: ${h.target}`
                        : `${h.action === "set" ? "+" : "-"} ${h.target}${shownValue}${h.world ? ` [${h.world}]` : ""}`;
                      const revertible = h.type === "group" || !!h.undo;
                      const revertTitle = h.undo
                        ? h.undo.action === "set"
                          ? t("luckperms_revert_to_set", { node: h.target, value: h.undo.value ? "TRUE" : "FALSE" })
                          : t("luckperms_revert_to_unset", { node: h.target })
                        : undefined;

                      return (
                        <div
                          key={h.id}
                          className={cn(
                            "py-3 flex flex-col gap-1.5 transition-all",
                            h.reverted && "opacity-45"
                          )}
                        >
                          <div className="flex items-start justify-between gap-3">
                            <div className="flex items-center gap-2 flex-wrap min-w-0 text-xs">
                              <span className="font-mono bg-muted border border-border px-1.5 py-0.5 rounded font-semibold text-foreground">
                                {h.player}
                              </span>
                              <span className={cn(
                                "font-mono break-all",
                                h.reverted ? "line-through text-muted-foreground" : isSuccess ? "text-foreground font-medium" : "text-destructive"
                              )}>
                                {actionLabel}
                              </span>
                            </div>
                            <div className="flex items-center gap-2 shrink-0">
                              <span className="text-[10px] text-muted-foreground font-mono select-none">{h.timestamp}</span>
                              {isSuccess && !h.reverted && revertible && (
                                <Button
                                  variant="outline"
                                  size="sm"
                                  disabled={submitting}
                                  onClick={() => handleRevert(h)}
                                  title={revertTitle}
                                  className="h-6 text-[10px] text-muted-foreground hover:text-primary py-0 px-2 gap-1 focus:outline-none"
                                >
                                  <Undo className="h-3 w-3" />
                                  {t("luckperms_revert")}
                                </Button>
                              )}
                              {isSuccess && !h.reverted && !revertible && (
                                <span className="text-[10px] text-muted-foreground select-none">
                                  {t("luckperms_revert_unknown")}
                                </span>
                              )}
                              {h.reverted && (
                                <Badge variant="outline" className="text-[9px] h-5 border-emerald-500/30 text-emerald-600 bg-emerald-500/10 font-bold px-1.5 select-none uppercase">
                                  {t("luckperms_revert")}
                                </Badge>
                              )}
                            </div>
                          </div>

                          <details className="group/details">
                            <summary className="cursor-pointer select-none text-[10px] text-muted-foreground/70 hover:text-foreground font-mono transition-colors list-none flex items-center gap-1">
                              <span className="transition-transform group-open/details:rotate-90">▶</span>
                              {t("luckperms_rcon_output")}
                            </summary>
                            <pre className="mt-1.5 p-2 rounded bg-muted/60 border border-border/80 font-mono text-[10px] text-foreground/80 overflow-x-auto whitespace-pre-wrap break-all max-h-24">
                              {h.output}
                            </pre>
                          </details>
                        </div>
                      );
                    })}
                  </div>
                )}
              </CardContent>
            </Card>
          </div>
        </div>
      )}
    </>
  );
}
