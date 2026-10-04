import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { DoorOpen, FolderOpen, Map, Package, Save, Shield, Terminal } from "lucide-react";
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
import { STATUS_POLL_FAST_MS, useAsync, useConfig, usePolling, useUnsavedGuard } from "@/lib/hooks";
import { experienceValid, LOGIN_GROUPS, LOBBY_GROUPS, readExperience, writeExperience, type Experience } from "@/lib/experience";

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
      {draft && groups.map((group) => (
        <Card key={group.key}>
          <CardHeader><CardTitle>{t(group.key)}</CardTitle></CardHeader>
          <CardContent className="grid gap-4 sm:grid-cols-2">
            {group.fields.map((field) => {
              const value = draft.values[field.key] ?? field.value;
              const id = `experience-${field.key}`;
              const set = (next: unknown) => setDraft({ ...draft, values: { ...draft.values, [field.key]: next } });
              if (typeof field.value === "boolean") return (
                <label key={id} className="flex min-h-11 items-center gap-3 rounded-md border border-border px-3 text-sm" htmlFor={id}>
                  <input id={id} type="checkbox" checked={value as boolean} onChange={(e) => set(e.target.checked)} disabled={!stopped || saving} className="h-4 w-4 accent-primary" />
                  {t(field.key)}
                </label>
              );
              return (
                <div key={id} className="grid min-w-0 gap-2">
                  <Label htmlFor={id}>{t(field.key)}</Label>
                  {field.choices ? (
                    <select id={id} value={value as string} onChange={(e) => set(e.target.value)} disabled={!stopped || saving} className="h-10 w-full rounded-md border border-input bg-background px-3 text-sm">
                      {field.choices.map((choice) => <option key={choice} value={choice}>{t(choice)}</option>)}
                    </select>
                  ) : field.multiline ? (
                    <textarea id={id} value={value as string} maxLength={512} onChange={(e) => set(e.target.value)} disabled={!stopped || saving} className="min-h-24 w-full rounded-md border border-input bg-background p-3 text-sm" />
                  ) : (
                    <Input id={id} type={typeof field.value === "number" ? "number" : "text"} value={value as string | number} min={field.min} max={field.max} step={1} maxLength={512} onChange={(e) => set(typeof field.value === "number" ? (e.target.value === "" ? "" : Number(e.target.value)) : e.target.value)} disabled={!stopped || saving} />
                  )}
                </div>
              );
            })}
          </CardContent>
        </Card>
      ))}
      {draft && <>
        <p className="text-sm text-muted-foreground">{t(name === "login" ? "login_settings_hint" : "lobby_settings_hint")}</p>
        {message && <MessageLine kind={message.kind} message={message.text} />}
        {!experienceValid(draft.values, groups) && <InlineError message={t("invalid_values")} />}
        <Button onClick={() => void save()} disabled={!stopped || !dirty || saving || !experienceValid(draft.values, groups)}><Save className="h-4 w-4" />{t(saving ? "saving" : "save")}</Button>
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
  return <Card>
    <CardHeader><CardTitle>{t("builder_title")}</CardTitle></CardHeader>
    <CardContent className="space-y-3">
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
  const links = [
    { icon: Map, title: "map_title", hint: name === "login" ? "login_map_hint" : "lobby_map_hint", to: `/servers/${name}/files` },
    { icon: Package, title: "plugins_title", hint: "plugins_hint", to: `/servers/${name}/files?path=plugins` },
    { icon: FolderOpen, title: "files_title", hint: "files_hint", to: `/servers/${name}/files` },
    { icon: Terminal, title: "console_title", hint: name === "login" ? "login_console_hint" : "console_hint", to: `/servers/${name}` },
    { icon: Shield, title: "backups_title", hint: "backups_hint", to: `/servers/${name}/backups` },
  ];
  return <div className="space-y-6">
    <PageHeader icon={DoorOpen} title={t("title")} subtitle={t("subtitle")} />
    <Card><CardContent className="flex flex-wrap items-center justify-between gap-4 pt-5">
      <div className="space-y-1">
        <p className="text-sm font-medium">{t("connection_version", { version: cfg?.gameVersion ?? t("unknown_version") })}</p>
        {cfg && <CopyAddress address={joinAddress("login", cfg)} />}
        <p className="max-w-2xl text-xs text-muted-foreground">{t("version_hint")}</p>
      </div>
      <Link to="/admin/images" className="text-sm text-primary hover:underline">{t("images_link")}</Link>
    </CardContent></Card>
    <div className="flex flex-wrap gap-2" aria-label={t("space_label")}>
      {(["lobby", "login"] as const).map((space) => <Button key={space} variant={name === space ? "default" : "outline"} aria-pressed={name === space} onClick={() => { if (space !== name) { if (dirty) setNextSpace(space); else setParams({ space }); } }}>{t(space)}</Button>)}
    </div>
    <p className="text-sm text-muted-foreground">{t(`${name}_description`)}</p>
    {status.loading && !data && <Loading />}
    {status.error != null && <ErrorState error={humanizeError(status.error)} onRetry={status.reload} />}
    {data && <>
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-card p-4">
        <div className="min-w-0 space-y-2"><div className="flex flex-wrap items-center gap-3"><h2 className="font-semibold">{data.displayName || t(name)}</h2><PhaseBadge phase={shownPhase(data)} /></div><p className="break-all text-xs text-muted-foreground">{data.image}</p></div>
        <PowerButton name={name} phase={data.phase} desiredState={data.desiredState} failed={startFailure(data) !== null} playersOnline={data.playersOnline} playerCountUnknown={data.playerCountUnknown} onChanged={status.reload} />
      </div>
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
        <ExperienceSettings key={name} name={name} onDirtyChange={setDirty} stopped={data.phase === "Stopped" && data.desiredState === "Stopped"} />
        <div className="space-y-4">
          {links.map(({ icon: Icon, title, hint, to }) => <Link key={title} to={to} className="flex gap-3 rounded-lg border border-border bg-card p-4 hover:border-primary/40"><Icon className="mt-0.5 h-5 w-5 shrink-0 text-primary" /><div className="min-w-0"><p className="text-sm font-medium">{t(title)}</p><p className="mt-1 text-xs text-muted-foreground">{t(hint)}</p></div></Link>)}
          <EditServerDialog serverName={name} systemService currentDisplayName={data.displayName} currentPolicy={data.autostartPolicy} currentImage={data.image} currentMemory={data.memory} currentStorage={data.storageSize} currentCpu={data.cpu} currentIdleStopSeconds={0} onUpdated={status.reload} />
          {name === "lobby" && <BuilderAccess ready={data.ready} />}
        </div>
      </div>
    </>}
    <ConfirmDialog open={nextSpace !== null} onOpenChange={(open) => { if (!open) setNextSpace(null); }} title={t("discard_title")} description={t("discard_hint")} confirmLabel={t("discard")} onConfirm={async () => { if (nextSpace) { setParams({ space: nextSpace }); setDirty(false); setNextSpace(null); } }} />
  </div>;
}
