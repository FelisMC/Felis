import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { BookOpen, ChevronRight, DoorOpen, FolderOpen, Globe, Map, MessageSquare, Package, Save, Shield, Sun, Terminal, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { PageHeader } from "@/components/PageHeader";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { PhaseBadge, shownPhase, startFailure } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
import { CopyAddress } from "@/components/CopyAddress";
import { EditServerDialog } from "@/components/EditServerDialog";
import { ErrorState, Loading } from "@/components/States";
import { InlineError, MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { joinAddress } from "@/lib/config";
import { cn } from "@/lib/utils";
import { STATUS_POLL_FAST_MS, useAsync, useConfig, usePolling, useUnsavedGuard } from "@/lib/hooks";
import { experienceValid, LOGIN_GROUPS, LOBBY_GROUPS, readExperience, writeExperience, type Experience } from "@/lib/experience";

const GROUP_ICONS: Record<string, LucideIcon> = {
  appearance: MessageSquare,
  players: Shield,
  world: Sun,
  loginBook: BookOpen,
};

function ExperienceSettings({ name, stopped, onDirtyChange }: { name: string; stopped: boolean; onDirtyChange: (dirty: boolean) => void }) {
  const { t } = useTranslation("lobby");
  const groups = name === "login" ? LOGIN_GROUPS : LOBBY_GROUPS;
  const query = useAsync(() => stopped ? readExperience(name, groups) : Promise.resolve(null), [name, stopped]);
  const [draft, setDraft] = useState<{ values: Experience; original: string; sha256: string } | null>(null);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ kind: "error" | "success"; text: string } | null>(null);
  useEffect(() => {
    if (query.data && !draft) {
      setDraft({ ...query.data, original: JSON.stringify(query.data.values) });
    }
  }, [query.data, draft]);
  const dirty = draft !== null && JSON.stringify(draft.values) !== draft.original;
  useUnsavedGuard(dirty);
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  async function save() {
    if (!draft || !stopped || saving || !experienceValid(draft.values, groups)) return;
    setSaving(true);
    setMessage(null);
    try {
      const sha256 = await writeExperience(name, draft.values, draft.sha256);
      setDraft({ ...draft, original: JSON.stringify(draft.values), sha256 });
      setMessage({ kind: "success", text: t("saved") });
    } catch (error) {
      setMessage({ kind: "error", text: humanizeError(error) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-4">
      {!stopped && <p className="rounded-md border border-amber-500/30 bg-amber-500/10 p-4 text-sm">{t(name === "login" ? "stop_login" : "stop_lobby")}</p>}
      {stopped && query.loading && !draft && <Loading />}
      {query.error != null && <ErrorState error={t("read_failed", { reason: humanizeError(query.error) })} onRetry={query.reload} />}
      {draft && groups.map((group) => {
        const Icon = GROUP_ICONS[group.key];
        const fields = group.fields.filter((field) => typeof field.value !== "boolean");
        const switches = group.fields.filter((field) => typeof field.value === "boolean");
        return (
          <Card key={group.key} className="overflow-hidden rounded-xl shadow-none">
            <CardHeader className="flex-row items-center gap-3 space-y-0 border-b border-border/60 bg-muted/20 py-4">
              <span className="rounded-lg bg-primary/10 p-2 text-primary"><Icon className="h-4 w-4" /></span>
              <div className="space-y-1.5">
                <CardTitle className="text-sm">{t(group.key)}</CardTitle>
                <p className="text-xs text-muted-foreground">{t(`${group.key}_description`)}</p>
              </div>
            </CardHeader>
            <CardContent className="space-y-5 pt-5">
              <div className="grid items-start gap-x-5 gap-y-4 sm:grid-cols-2">
                {fields.map((field) => {
                  const value = draft.values[field.key] ?? field.value;
                  const id = `experience-${field.key}`;
                  const set = (next: unknown) => setDraft({ ...draft, values: { ...draft.values, [field.key]: next } });
                  return (
                    <div key={id} className="min-w-0 space-y-2">
                      <Label htmlFor={id} className="block text-xs font-medium text-muted-foreground">{t(field.key)}</Label>
                      {field.choices ? (
                        <select id={id} value={value as string} onChange={(e) => set(e.target.value)} disabled={!stopped || saving} className="h-10 w-full rounded-lg border border-input bg-background px-3 text-sm outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50">
                          {field.choices.map((choice) => <option key={choice} value={choice}>{t(choice)}</option>)}
                        </select>
                      ) : field.multiline ? (
                        <textarea id={id} value={value as string} maxLength={512} onChange={(e) => set(e.target.value)} disabled={!stopped || saving} className="block h-32 w-full resize-y rounded-lg border border-input bg-background px-3 py-2.5 text-sm leading-6 outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50" />
                      ) : (
                        <Input id={id} type={typeof field.value === "number" ? "number" : "text"} value={value as string | number} min={field.min} max={field.max} step={1} maxLength={512} onChange={(e) => set(typeof field.value === "number" ? (e.target.value === "" ? "" : Number(e.target.value)) : e.target.value)} disabled={!stopped || saving} className="h-10 rounded-lg bg-background shadow-none" />
                      )}
                    </div>
                  );
                })}
              </div>
              <div className={cn("grid gap-x-6 border-t border-border/60", switches.length > 1 && "sm:grid-cols-2")}>
                {switches.map((field) => (
                  <label key={field.key} htmlFor={`experience-${field.key}`} className="flex min-h-12 cursor-pointer items-center justify-between gap-4 py-3 text-sm">
                    <span>{t(field.key)}</span>
                    <span className="relative shrink-0">
                      <input id={`experience-${field.key}`} type="checkbox" role="switch" checked={(draft.values[field.key] ?? field.value) as boolean} onChange={(e) => setDraft({ ...draft, values: { ...draft.values, [field.key]: e.target.checked } })} disabled={!stopped || saving} className="peer sr-only" />
                      <span aria-hidden="true" className="block h-5 w-9 rounded-full bg-muted-foreground/25 p-0.5 transition-colors peer-checked:bg-primary peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-focus-visible:ring-offset-2 peer-disabled:opacity-50 peer-checked:[&>span]:translate-x-4">
                        <span className="block h-4 w-4 rounded-full bg-white shadow-sm transition-transform motion-reduce:transition-none" />
                      </span>
                    </span>
                  </label>
                ))}
              </div>
            </CardContent>
          </Card>
        );
      })}
      {draft && <>
        <p className="text-sm text-muted-foreground">{t(name === "login" ? "login_settings_hint" : "lobby_settings_hint")}</p>
        {message && <MessageLine kind={message.kind} message={message.text} />}
        {!experienceValid(draft.values, groups) && <InlineError message={t("invalid_values")} />}
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-card p-3">
          <p className="text-xs text-muted-foreground">{t(dirty ? "unsaved" : "apply_on_start")}</p>
          <Button onClick={() => void save()} disabled={!stopped || !dirty || saving || !experienceValid(draft.values, groups)}><Save className="h-4 w-4" />{t(saving ? "saving" : "save")}</Button>
        </div>
      </>}
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
  return <Card className="overflow-hidden rounded-xl shadow-none">
    <CardHeader className="border-b border-border/60 bg-muted/20 py-4"><CardTitle className="text-sm">{t("builder_title")}</CardTitle></CardHeader>
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
  const name = params.get("space") === "login" ? "login" : "lobby";
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
  return <div className="mx-auto max-w-7xl space-y-5">
    <PageHeader icon={DoorOpen} title={t("title")} subtitle={t("subtitle")} />
    <Card className="rounded-xl bg-muted/20 shadow-none"><CardContent className="flex flex-wrap items-start justify-between gap-4 pt-5">
      <div className="flex min-w-0 gap-3">
        <Globe className="mt-0.5 h-5 w-5 shrink-0 text-primary" />
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
        <Button key={space} variant="ghost" aria-pressed={name === space} onClick={() => selectSpace(space)} className={cn("px-4", name === space ? "bg-card text-primary shadow-sm hover:bg-card" : "text-muted-foreground")}>
          {space === "lobby" ? <DoorOpen /> : <BookOpen />}{t(space)}
        </Button>
      ))}
    </div>
    <p className="text-sm text-muted-foreground">{t(`${name}_description`)}</p>
    {status.loading && !data && <Loading />}
    {status.error != null && <ErrorState error={humanizeError(status.error)} onRetry={status.reload} />}
    {data && <>
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-card p-4">
        <div className="min-w-0 space-y-2"><div className="flex flex-wrap items-center gap-3"><h2 className="font-semibold">{data.displayName || t(name)}</h2><PhaseBadge phase={shownPhase(data)} /></div><p className="break-all text-xs text-muted-foreground">{data.image}</p></div>
        <PowerButton name={name} phase={data.phase} desiredState={data.desiredState} failed={startFailure(data) !== null} playersOnline={data.playersOnline} playerCountUnknown={data.playerCountUnknown} onChanged={status.reload} />
      </div>
      <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
        <ExperienceSettings key={name} name={name} onDirtyChange={setDirty} stopped={data.phase === "Stopped" && data.desiredState === "Stopped"} />
        <div className="space-y-4">
          <Card className="overflow-hidden rounded-xl shadow-none">
            <CardHeader className="border-b border-border/60 bg-muted/20 py-4"><CardTitle className="text-sm">{t("content_tools")}</CardTitle></CardHeader>
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
          <EditServerDialog serverName={name} systemService currentDisplayName={data.displayName} currentPolicy={data.autostartPolicy} currentImage={data.image} currentMemory={data.memory} currentStorage={data.storageSize} currentCpu={data.cpu} currentIdleStopSeconds={0} onUpdated={status.reload} />
          {name === "lobby" && <BuilderAccess ready={data.ready} />}
        </div>
      </div>
    </>}
    <ConfirmDialog open={nextSpace !== null} onOpenChange={(open) => { if (!open) setNextSpace(null); }} title={t("discard_title")} description={t("discard_hint")} confirmLabel={t("discard")} onConfirm={async () => { if (nextSpace) { setParams({ space: nextSpace }); setDirty(false); setNextSpace(null); } }} />
  </div>;
}
