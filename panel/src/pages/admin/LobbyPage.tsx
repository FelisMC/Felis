import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { BookOpen, ChevronRight, DoorOpen, FolderOpen, Globe, Loader2, Map, MessageSquare, Package, RotateCw, Save, Shield, Sun, Terminal, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { PageHeader } from "@/components/PageHeader";
import { Switch } from "@/components/ui/switch";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { PhaseBadge, shownPhase, startFailure } from "@/components/PhaseBadge";
import { PowerButton, SUBMITTED_HOLD_MS } from "@/components/PowerButton";
import { CopyAddress } from "@/components/CopyAddress";
import { EditServerDialog } from "@/components/EditServerDialog";
import { ErrorState } from "@/components/States";
import { InlineError, MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { joinAddress } from "@/lib/config";
import { cn } from "@/lib/utils";
import type { ServerStatus } from "@/lib/types";
import { STATUS_POLL_FAST_MS, useAsync, useConfig, usePolling, useUnsavedGuard } from "@/lib/hooks";
import { experienceValid, LOGIN_GROUPS, LOBBY_GROUPS, readExperience, writeExperience, type Experience } from "@/lib/experience";

const GROUP_ICONS: Record<string, LucideIcon> = {
  appearance: MessageSquare,
  players: Shield,
  world: Sun,
  loginBook: BookOpen,
};

function ExperienceSettings({ name, server, onDirtyChange, onChanged }: { name: string; server: ServerStatus | null; onDirtyChange: (dirty: boolean) => void; onChanged: () => void }) {
  const { t } = useTranslation("lobby");
  const groups = name === "login" ? LOGIN_GROUPS : LOBBY_GROUPS;
  const query = useAsync(() => readExperience(name, groups), [name]);
  const [draft, setDraft] = useState<{ values: Experience; original: string; sha256: string } | null>(null);
  const [saving, setSaving] = useState(false);
  const [needsRestart, setNeedsRestart] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [confirmRestart, setConfirmRestart] = useState(false);
  const running = server?.phase === "Running" && server.desiredState === "Running";
  useEffect(() => {
    if (!restarting) return;
    if (!running) { setRestarting(false); return; }
    const hold = window.setTimeout(() => setRestarting(false), SUBMITTED_HOLD_MS);
    return () => window.clearTimeout(hold);
  }, [running, restarting]);
  const [message, setMessage] = useState<{ kind: "error" | "success"; text: string } | null>(null);
  useEffect(() => {
    if (query.data && !draft) {
      setDraft({ ...query.data, original: JSON.stringify(query.data.values) });
    }
  }, [query.data, draft]);
  const dirty = draft !== null && JSON.stringify(draft.values) !== draft.original;
  useUnsavedGuard(dirty);
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  function setValue(key: string, value: unknown) {
    setDraft((current) => current && { ...current, values: { ...current.values, [key]: value } });
  }

  async function save() {
    if (!draft || saving || !experienceValid(draft.values, groups)) return;
    setSaving(true);
    setMessage(null);
    try {
      const sha256 = await writeExperience(name, draft.values, draft.sha256);
      setDraft({ ...draft, original: JSON.stringify(draft.values), sha256 });
      setNeedsRestart(true);
      setMessage({ kind: "success", text: t("saved") });
    } catch (error) {
      setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setSaving(false);
    }
  }

  async function restart() {
    setRestarting(true);
    setMessage(null);
    try {
      await api.restart(name);
      setNeedsRestart(false);
      setMessage({ kind: "success", text: t("restart_requested") });
      onChanged();
    } catch (error) {
      setRestarting(false);
      throw error;
    }
  }

  return (
    <div aria-busy={query.loading} className="space-y-4">
      {query.loading && <p role="status" className="flex items-center gap-2 rounded-lg border border-border bg-muted/20 px-3 py-2.5 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 shrink-0 animate-spin" />{t("loading_settings")}</p>}
      {query.error != null && <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-3"><InlineError message={t("read_failed", { reason: humanizeError(query.error) })} /><Button variant="outline" size="sm" onClick={query.reload}>{t("common:try_again")}</Button></div>}
      {groups.map((group) => {
        const Icon = GROUP_ICONS[group.key];
        const fields = group.fields.filter((field) => typeof field.value !== "boolean");
        const switches = group.fields.filter((field) => typeof field.value === "boolean");
        return (
          <Card key={group.key} className="overflow-hidden">
            <CardHeader className="flex-row flex-wrap items-center gap-3">
              <span className="shrink-0 text-primary"><Icon className="h-4 w-4" /></span>
              <div className="space-y-2">
                <CardTitle>{t(group.key)}</CardTitle>
                <p className="text-xs text-muted-foreground">{t(`${group.key}_description`)}</p>
              </div>
            </CardHeader>
            <CardContent className="space-y-5">
              <div className="grid items-start gap-x-5 gap-y-4 sm:grid-cols-2">
                {fields.map((field) => {
                  const value = draft ? draft.values[field.key] ?? field.value : "";
                  const id = `experience-${field.key}`;
                  return (
                    <div key={id} className="min-w-0 space-y-2">
                      <Label htmlFor={id} className="block">{t(field.key)}</Label>
                      {field.choices ? (
                        <Select value={(value as string) || undefined} onValueChange={(choice) => setValue(field.key, choice)} disabled={!draft || saving || restarting}>
                          <SelectTrigger id={id}><SelectValue placeholder={query.loading ? t("common:loading") : "—"} /></SelectTrigger>
                          <SelectContent>{field.choices.map((choice) => <SelectItem key={choice} value={choice}>{t(choice)}</SelectItem>)}</SelectContent>
                        </Select>
                      ) : field.multiline ? (
                        <Textarea id={id} value={value as string} placeholder={!draft && query.loading ? t("common:loading") : undefined} maxLength={512} onChange={(e) => setValue(field.key, e.target.value)} disabled={!draft || saving || restarting} className="h-32" />
                      ) : (
                        <Input id={id} type={typeof field.value === "number" ? "number" : "text"} value={value as string | number} placeholder={!draft && query.loading ? t("common:loading") : undefined} min={field.min} max={field.max} step={1} maxLength={512} onChange={(e) => setValue(field.key, typeof field.value === "number" ? (e.target.value === "" ? "" : Number(e.target.value)) : e.target.value)} disabled={!draft || saving || restarting} />
                      )}
                    </div>
                  );
                })}
              </div>
              <div className={cn("grid gap-x-6 border-t border-border/60", switches.length > 1 && "sm:grid-cols-2")}>
                {switches.map((field) => (
                  <label key={field.key} htmlFor={`experience-${field.key}`} className="flex min-h-12 cursor-pointer items-center justify-between gap-4 py-3 text-sm">
                    <span>{t(field.key)}</span>
                    <Switch id={`experience-${field.key}`} checked={draft ? (draft.values[field.key] ?? field.value) as boolean : false} onCheckedChange={(value) => setValue(field.key, value)} disabled={!draft || saving || restarting} />
                  </label>
                ))}
              </div>
            </CardContent>
          </Card>
        );
      })}
      <p className="text-sm text-muted-foreground">{t(name === "login" ? "login_settings_hint" : "lobby_settings_hint")}</p>
      {message && <MessageLine kind={message.kind} message={message.text} />}
      {draft && !experienceValid(draft.values, groups) && <InlineError message={t("invalid_values")} />}
      <Card className={cn(needsRestart && "border-amber-500/30 bg-amber-500/5")}><CardFooter className="border-t-0">
        <p className={cn("text-xs", needsRestart ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground")}>{t(dirty ? "unsaved" : needsRestart ? "restart_required" : "apply_on_start")}</p>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" disabled={!draft || !running || dirty || saving || restarting} onClick={() => {
            if ((server?.playersOnline ?? 0) > 0 || server?.playerCountUnknown) setConfirmRestart(true);
            else void restart().catch((error) => setMessage({ kind: "error", text: humanizeError(error) }));
          }}><RotateCw className={cn(restarting && "animate-spin")} />{t(restarting ? "restarting" : "restart")}</Button>
          <Button onClick={() => void save()} disabled={!draft || !dirty || saving || restarting || !experienceValid(draft.values, groups)}><Save className="h-4 w-4" />{t(saving ? "saving" : "save")}</Button>
        </div>
      </CardFooter></Card>
      <ConfirmDialog open={confirmRestart} onOpenChange={setConfirmRestart} title={t("restart_title")} description={t("restart_hint")} confirmLabel={t("restart")} onConfirm={restart} />
    </div>
  );
}

function BuilderAccess({ ready }: { ready: boolean }) {
  const { t } = useTranslation("lobby");
  const [player, setPlayer] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ kind: "error" | "success"; text: string } | null>(null);
  async function grant(value: boolean) {
    setBusy(true);
    setMessage(null);
    try {
      const result = await api.accessPermission("lobby", "set", player.trim(), "felis.lobby.build", value);
      setMessage({ kind: "success", text: result.output });
    } catch (error) {
      setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setBusy(false);
    }
  }
  return <Card className="overflow-hidden">
    <CardHeader><CardTitle>{t("builder_title")}</CardTitle></CardHeader>
    <CardContent className="space-y-3 pt-4">
      <p className="text-sm text-muted-foreground">{t("builder_hint")}</p>
      <Label htmlFor="builder-player">{t("builder_player")}</Label>
      <Input id="builder-player" value={player} onChange={(e) => setPlayer(e.target.value)} placeholder="Steve" />
      <div className="flex flex-wrap gap-2">
        <Button disabled={!ready || busy || !/^[A-Za-z0-9_]{1,16}$/.test(player.trim())} onClick={() => void grant(true)}>{t("builder_grant")}</Button>
        <Button variant="outline" disabled={!ready || busy || !/^[A-Za-z0-9_]{1,16}$/.test(player.trim())} onClick={() => void grant(false)}>{t("builder_revoke")}</Button>
      </div>
      {!ready && <p className="text-sm text-muted-foreground">{t("builder_running")}</p>}
      {message && <MessageLine kind={message.kind} message={message.text} />}
    </CardContent>
  </Card>;
}

export function LobbyPage() {
  const { t } = useTranslation("lobby");
  const [params, setParams] = useSearchParams();
  const name = params.get("space") === "lobby" ? "lobby" : "login";
  const [dirty, setDirty] = useState(false);
  const [nextSpace, setNextSpace] = useState<string | null>(null);
  const cfg = useConfig();
  const status = useAsync(() => api.status(name), [name]);
  usePolling(status.reload, STATUS_POLL_FAST_MS);
  const data = status.data;
  function selectSpace(space: string) {
    if (space === name) return;
    if (dirty) setNextSpace(space);
    else setParams({ space });
  }
  const links = [
    { icon: Map, title: "map_title", hint: name === "login" ? "login_map_hint" : "lobby_map_hint", to: `/servers/${name}/files` },
    { icon: Package, title: "plugins_title", hint: "plugins_hint", to: `/servers/${name}/files?path=plugins` },
    { icon: FolderOpen, title: "files_title", hint: "files_hint", to: `/servers/${name}/files` },
    { icon: Terminal, title: "console_title", hint: name === "login" ? "login_console_hint" : "console_hint", to: `/servers/${name}` },
    { icon: Shield, title: "backups_title", hint: "backups_hint", to: `/servers/${name}/backups` },
  ];
  return <div className="space-y-6">
    <PageHeader icon={DoorOpen} title={t("title")} subtitle={t("subtitle")} />
    <Card><CardContent className="flex flex-wrap items-start justify-between gap-4">
      <div className="flex min-w-0 gap-3">
        <Globe className="mt-0.5 h-4 w-4 shrink-0 text-primary" />
        <div className="space-y-1">
          <p className="text-sm font-medium">{t("connection_version", { version: cfg?.gameVersion ?? t("unknown_version") })}</p>
          {cfg && <CopyAddress address={joinAddress("login", cfg)} />}
          <p className="max-w-2xl text-xs text-muted-foreground">{t("version_hint")}</p>
        </div>
      </div>
      <Link to="/admin/images" className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline">{t("images_link")}<ChevronRight className="h-3.5 w-3.5" /></Link>
    </CardContent></Card>
    <div className="inline-flex gap-1 rounded-lg bg-muted/60 p-1" role="group" aria-label={t("space_label")}>
      {(["login", "lobby"] as const).map((space) => (
        <Button key={space} variant="ghost" aria-pressed={name === space} onClick={() => selectSpace(space)} className={cn("px-4", name === space ? "bg-card text-primary hover:bg-card" : "text-muted-foreground")}>
          {space === "lobby" ? <DoorOpen /> : <BookOpen />}{t(space)}
        </Button>
      ))}
    </div>
    <p className="text-sm text-muted-foreground">{t(`${name}_description`)}</p>
    {status.error != null && <ErrorState error={humanizeError(status.error)} onRetry={status.reload} />}
    <Card className="flex flex-wrap items-center justify-between gap-3 p-5">
      <div className="min-w-0 space-y-2"><div className="flex flex-wrap items-center gap-3"><h2 className="text-sm font-semibold">{data?.displayName || t(name)}</h2>{data ? <PhaseBadge phase={shownPhase(data)} /> : status.loading && <span role="status" className="flex items-center gap-2 text-xs text-muted-foreground"><Loader2 className="h-3.5 w-3.5 animate-spin" />{t("common:loading")}</span>}</div><p className="break-all text-xs text-muted-foreground">{data?.image || "\u00a0"}</p></div>
      {data && <PowerButton name={name} phase={data.phase} desiredState={data.desiredState} failed={startFailure(data) !== null} playersOnline={data.playersOnline} playerCountUnknown={data.playerCountUnknown} onChanged={status.reload} />}
    </Card>
    <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
      <ExperienceSettings key={name} name={name} server={data} onDirtyChange={setDirty} onChanged={status.reload} />
      <div className="space-y-4">
        <Card className="overflow-hidden">
          <CardHeader><CardTitle>{t("content_tools")}</CardTitle></CardHeader>
          <CardContent className="divide-y divide-border/60 p-0">
            {links.map(({ icon: Icon, title, hint, to }) => (
              <Link key={title} to={to} className="group flex gap-3 p-4 transition-colors hover:bg-muted/40">
                <Icon className="mt-0.5 h-4 w-4 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="flex items-center justify-between gap-2 text-sm font-medium">{t(title)}<ChevronRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" /></p>
                  <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">{t(hint)}</p>
                </div>
              </Link>
            ))}
          </CardContent>
        </Card>
        {data && <EditServerDialog serverName={name} systemService currentDisplayName={data.displayName} currentPolicy={data.autostartPolicy} currentImage={data.image} currentMemory={data.memory} currentStorage={data.storageSize} currentCpu={data.cpu} currentIdleStopSeconds={0} onUpdated={status.reload} />}
        {name === "lobby" && <BuilderAccess ready={data?.ready ?? false} />}
      </div>
    </div>
    <ConfirmDialog open={nextSpace !== null} onOpenChange={(open) => { if (!open) setNextSpace(null); }} title={t("discard_title")} description={t("discard_hint")} confirmLabel={t("discard")} onConfirm={async () => { if (nextSpace) { setParams({ space: nextSpace }); setDirty(false); setNextSpace(null); } }} />
  </div>;
}
