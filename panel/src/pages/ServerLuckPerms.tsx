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
import { PhaseBadge } from "@/components/PhaseBadge";
import { Loading, ErrorState, NotYours, NotRunning } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import type { Phase } from "@/lib/types";
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
  
  // Ownership verification
  const {
    data: mine,
    error: mineError,
    reload: reloadMine,
  } = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name]
  );

  // Online Players
  const { data: playersData, loading: playersLoading } = useAsync(
    () => api.accessPlayers(name),
    [name]
  );
  const onlinePlayers = playersData?.players ?? [];

  // Selected Player Profile State
  const [selectedPlayer, setSelectedPlayer] = useState<string>("");
  const [searchQuery, setSearchQuery] = useState("");

  // LuckPerms Profile data for selected player
  const { data: lpInfo, loading: lpLoading, reload: reloadLp } = useAsync(
    () => selectedPlayer ? api.accessLuckPermsInfo(name, selectedPlayer) : Promise.resolve(null),
    [name, selectedPlayer]
  );

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
        output: res.output || `[RCON] parent add ${groupName}`,
      });
      reloadLp();
    } catch (err) {
      setFormFeedback({ kind: "err", msg: humanizeError(err) });
    } finally {
      setSubmitting(false);
    }
  };

  const handleRemoveGroup = async (groupName: string) => {
    if (!selectedPlayer || submitting) return;
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
        output: res.output || `[RCON] parent remove ${groupName}`,
      });
      reloadLp();
    } catch (err) {
      setFormFeedback({ kind: "err", msg: humanizeError(err) });
    } finally {
      setSubmitting(false);
    }
  };

  const handleAddPermission = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedPlayer || !permNodeInput.trim() || submitting) return;
    const node = permNodeInput.trim();
    const world = permWorldInput.trim();

    if (!LP_NODE.test(node)) {
      setFormFeedback({ kind: "err", msg: t("luckperms_error_invalid_node") });
      return;
    }
    if (world !== "" && !LP_CTX.test(world)) {
      setFormFeedback({ kind: "err", msg: t("luckperms_error_invalid_world") });
      return;
    }

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
        status: "success",
        output: res.output || `[RCON] permission set ${node}`,
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

  const handleRemovePermission = async (node: string, world?: string) => {
    if (!selectedPlayer || submitting) return;
    setSubmitting(true);
    setFormFeedback(null);
    try {
      const res = await api.accessPermission(name, "unset", selectedPlayer, node, undefined, world || undefined);
      addHistoryItem({
        type: "permission",
        player: selectedPlayer,
        action: "unset",
        target: node,
        world: world || undefined,
        status: "success",
        output: res.output || `[RCON] permission unset ${node}`,
      });
      reloadLp();
    } catch (err) {
      setFormFeedback({ kind: "err", msg: humanizeError(err) });
    } finally {
      setSubmitting(false);
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
        const inverseAction = item.action === "set" ? "unset" : "set";
        await api.accessPermission(
          name,
          inverseAction,
          item.player,
          item.target,
          item.action === "set" ? item.value : undefined,
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
      setFormFeedback({ kind: "err", msg: `${t("luckperms_reverting")} ${humanizeError(err)}` });
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
  if (error) {
    return (
      <>
        {back}
        <ErrorState error={error} onRetry={reload} />
      </>
    );
  }
  if (!data) return back;

  const ownershipPending = ownershipPendingCheck(tierLoading, isAdmin, mine, mineError);
  const owned = isAdmin || (mine ?? []).some((s) => s.name === name && s.owned === true);
  const phase: Phase = data.phase;

  const header = (
    <PageHeader
      icon={Shield}
      title={data.displayName || data.name}
      subtitle={t("luckperms_desc")}
      actions={<PhaseBadge phase={phase} />}
      className="mb-6"
    />
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
        <NotYours title={t("players_not_yours_title")} body={t("players_not_yours_body")} />
      ) : phase !== "Running" ? (
        <NotRunning title={t("players_not_running_title")} body={t("players_not_running_body")} wakeLabel={t("wake")} onWake={() => api.wake(name).then(reload)} />
      ) : (
        <div className="grid grid-cols-1 lg:grid-cols-4 gap-6 items-start">
          {/* Left Directory Sidebar: Single card for Search + Online Players */}
          <Card className="lg:col-span-1 flex flex-col h-[calc(100vh-14rem)] lg:min-h-[500px] max-h-[700px] border-border bg-card/60 backdrop-blur-sm overflow-hidden">
            <CardHeader className="p-4 pb-2 border-b border-border/50 shrink-0">
              <CardTitle className="text-xs font-bold uppercase tracking-wider text-muted-foreground flex items-center gap-2">
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
                  className="pl-9 h-9 text-xs font-mono bg-background/50 border-input focus-visible:ring-primary/30"
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
            <CardContent className="p-2 overflow-y-auto flex-1 space-y-0.5 min-h-0">
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
              <Card className="flex flex-col items-center justify-center py-24 text-center border-dashed border-border bg-card/20 min-h-[500px]">
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

                {/* Operations Feedback */}
                {formFeedback && (
                  <div className={cn(
                    "p-3 rounded-md text-xs whitespace-pre-wrap break-all border transition-all animate-in fade-in duration-200",
                    formFeedback.kind === "ok"
                      ? "bg-emerald-500/10 border-emerald-500/30 text-emerald-600 dark:text-emerald-400"
                      : "bg-destructive/10 border-destructive/30 text-destructive"
                  )}>
                    {formFeedback.msg}
                  </div>
                )}

                {/* Groups Management Card */}
                <Card className="border-border bg-card/40 shadow-sm">
                  <CardHeader className="p-5 pb-3 border-b border-border/50 bg-muted/20 shrink-0">
                    <CardTitle className="text-xs font-bold uppercase tracking-wider text-muted-foreground">
                      {t("luckperms_groups_title")}
                    </CardTitle>
                  </CardHeader>
                  <CardContent className="p-5 space-y-4">
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
                                  className="text-muted-foreground hover:text-destructive hover:bg-muted p-0.5 rounded transition-all focus:outline-none"
                                >
                                  <Trash2 className="h-3.5 w-3.5" />
                                </button>
                              </Badge>
                            ))
                          ) : (
                            <p className="text-xs text-muted-foreground/60 italic py-1">{t("luckperms_no_parent_groups")}</p>
                          )}
                        </div>

                        <div className="border-t border-border/50 pt-4 flex flex-col sm:flex-row gap-3 items-end">
                          <div className="grid gap-1.5 flex-1 w-full">
                            <Label className="text-xs text-muted-foreground font-semibold">
                              {t("luckperms_group_name")}
                            </Label>
                            <div className="flex gap-2">
                              <Input
                                placeholder={t("luckperms_custom_group_placeholder")}
                                value={groupNameInput}
                                onChange={(e) => setGroupNameInput(e.target.value)}
                                className="h-9 text-xs bg-background/50 border-input focus-visible:ring-primary/20"
                                disabled={submitting}
                              />
                              <Select
                                value=""
                                onValueChange={(v) => setGroupNameInput(v)}
                                disabled={submitting}
                              >
                                <SelectTrigger className="w-[120px] h-9 text-xs bg-background/50 border-input text-muted-foreground">
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
                            className="h-9 px-4 shrink-0 w-full sm:w-auto hover:bg-primary/90 transition-colors"
                          >
                            <Plus className="h-4 w-4 mr-1" />
                            {t("luckperms_add_group_btn")}
                          </Button>
                        </div>
                      </div>
                    )}
                  </CardContent>
                </Card>

                {/* Permissions Management Card */}
                <Card className="border-border bg-card/40 shadow-sm">
                  <CardHeader className="p-5 pb-3 border-b border-border/50 bg-muted/20 shrink-0">
                    <CardTitle className="text-xs font-bold uppercase tracking-wider text-muted-foreground">
                      {t("luckperms_permissions_title")}
                    </CardTitle>
                  </CardHeader>
                  <CardContent className="p-5 space-y-4">
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
                                          onClick={() => handleRemovePermission(p.node, p.world)}
                                          className="h-7 w-7 p-0 text-muted-foreground hover:text-destructive hover:bg-muted rounded transition-all focus:outline-none"
                                        >
                                          <Trash2 className="h-3.5 w-3.5" />
                                        </Button>
                                      </td>
                                    </tr>
                                  ))
                                ) : (
                                  <tr>
                                    <td colSpan={4} className="px-4 py-10 text-center text-muted-foreground/60 italic">
                                      {t("luckperms_no_perms")}
                                    </td>
                                  </tr>
                                )}
                              </tbody>
                            </table>
                          </div>
                        </div>

                        {/* Add Permission Node Inline Form */}
                        <form onSubmit={handleAddPermission} className="border-t border-border/50 pt-4 space-y-4">
                          <Label className="text-xs font-semibold text-muted-foreground">
                            {t("luckperms_add_perm_btn")}
                          </Label>
                          <div className="grid grid-cols-1 sm:grid-cols-4 gap-3.5">
                            <div className="sm:col-span-2 grid gap-1.5">
                              <Label htmlFor="lp-node-input" className="text-[10px] text-muted-foreground font-semibold">
                                {t("luckperms_node")}
                              </Label>
                              <div className="flex gap-2">
                                <Input
                                  id="lp-node-input"
                                  placeholder="essentials.fly"
                                  value={permNodeInput}
                                  onChange={(e) => setPermNodeInput(e.target.value)}
                                  className="h-9 text-xs bg-background/50 border-input focus-visible:ring-primary/20"
                                  disabled={submitting}
                                  autoComplete="off"
                                />
                                <Select
                                  value=""
                                  onValueChange={(v) => setPermNodeInput(v)}
                                  disabled={submitting}
                                >
                                  <SelectTrigger className="w-[100px] h-9 text-xs shrink-0 bg-background/50 border-input text-muted-foreground">
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

                            <div className="grid gap-1.5">
                              <Label htmlFor="lp-value-input" className="text-[10px] text-muted-foreground font-semibold">
                                {t("luckperms_value")}
                              </Label>
                              <Select
                                value={permValueInput ? "true" : "false"}
                                onValueChange={(v) => setPermValueInput(v === "true")}
                                disabled={submitting}
                              >
                                <SelectTrigger id="lp-value-input" className="h-9 text-xs bg-background/50 border-input">
                                  <SelectValue />
                                </SelectTrigger>
                                <SelectContent className="border-border">
                                  <SelectItem value="true">{t("luckperms_value_grant")}</SelectItem>
                                  <SelectItem value="false">{t("luckperms_value_deny")}</SelectItem>
                                </SelectContent>
                              </Select>
                            </div>

                            <div className="grid gap-1.5">
                              <Label htmlFor="lp-world-input" className="text-[10px] text-muted-foreground font-semibold">
                                {t("luckperms_world")}
                              </Label>
                              <Input
                                id="lp-world-input"
                                placeholder="world"
                                value={permWorldInput}
                                onChange={(e) => setPermWorldInput(e.target.value)}
                                className="h-9 text-xs font-mono bg-background/50 border-input focus-visible:ring-primary/20"
                                disabled={submitting}
                                autoComplete="off"
                              />
                            </div>
                          </div>

                          <div className="flex justify-end pt-1">
                            <Button
                              type="submit"
                              disabled={submitting || !permNodeInput.trim()}
                              className="w-full sm:w-auto h-9 hover:bg-primary/90 transition-colors"
                            >
                              <Plus className="h-4 w-4 mr-1" />
                              {t("luckperms_add_perm_btn")}
                            </Button>
                          </div>
                        </form>
                      </div>
                    )}
                  </CardContent>
                </Card>
              </>
            )}

            {/* Action History Log */}
            <Card className="border-border bg-card/40 shadow-sm">
              <CardHeader className="p-5 pb-3 border-b border-border/50 bg-muted/20 shrink-0">
                <div className="flex items-center justify-between">
                  <CardTitle className="text-xs font-bold uppercase tracking-wider text-muted-foreground flex items-center gap-1.5">
                    <History className="h-4 w-4 text-primary" />
                    {t("luckperms_recent_actions")}
                  </CardTitle>
                  {history.length > 0 && (
                    <button
                      type="button"
                      onClick={clearHistory}
                      className="text-muted-foreground hover:text-destructive transition-colors focus:outline-none"
                      title={t("luckperms_clear_history")}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </button>
                  )}
                </div>
              </CardHeader>
              <CardContent className="p-4 pt-3">
                {history.length === 0 ? (
                  <p className="text-xs text-muted-foreground/60 py-4 italic select-none text-center">
                    {t("luckperms_no_recent_actions")}
                  </p>
                ) : (
                  <div className="divide-y divide-border/60 max-h-[320px] overflow-y-auto pr-1">
                    {history.map((h) => {
                      const isSuccess = h.status === "success";
                      const actionLabel = h.type === "group"
                        ? `${h.action === "add" ? "+" : "-"} ${t("luckperms_group_name")}: ${h.target}`
                        : `${h.action === "set" ? `+ ${h.target} (${h.value ? "TRUE" : "FALSE"})` : `- ${h.target}`}${h.world ? ` [${h.world}]` : ""}`;

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
                              {isSuccess && !h.reverted && (
                                <Button
                                  variant="outline"
                                  size="sm"
                                  disabled={submitting}
                                  onClick={() => handleRevert(h)}
                                  className="h-6 text-[10px] text-muted-foreground hover:text-primary py-0 px-2 gap-1 focus:outline-none"
                                >
                                  <Undo className="h-3 w-3" />
                                  {t("luckperms_revert")}
                                </Button>
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

function ownershipPendingCheck(
  tierLoading: boolean,
  isAdmin: boolean,
  mine: any[] | null,
  mineError: any
): boolean {
  return tierLoading || (!isAdmin && mine === null && !mineError);
}