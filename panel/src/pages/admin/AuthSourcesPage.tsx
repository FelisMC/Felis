import { useEffect, useState } from "react";
import { ArrowDown, ArrowUp, Check, ChevronRight, Loader2, Plus, RefreshCw, Save, ShieldCheck, Trash2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { PageHeader } from "@/components/PageHeader";
import { InlineError, MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useUnsavedGuard } from "@/lib/hooks";
import type { AuthSourceConfig, AuthSourcesSettings } from "@/lib/types";
import { cn } from "@/lib/utils";

const emptySource = (): AuthSourceConfig => ({ tag: "", prefix: "", url: "", api_url: "", enabled: true });

function validSource(source: AuthSourceConfig): boolean {
  const endpoint = (value: string) => {
    try {
      const url = new URL(value);
      return value.trim() === value && ["http:", "https:"].includes(url.protocol) && !!url.hostname && !/[?#]/.test(value);
    } catch { return false; }
  };
  return !!source.tag && source.tag.trim() === source.tag && !source.tag.includes(":") && source.tag.toLowerCase() !== "mojang" &&
    /^[a-z0-9]{1,4}$/i.test(source.prefix) && endpoint(source.url) && (!source.api_url || endpoint(source.api_url));
}

type SourceDraft = AuthSourceConfig & { locked?: boolean; expanded?: boolean };
type Draft = Omit<AuthSourcesSettings, "sources"> & { sources: SourceDraft[]; original: string };
const wireSources = (sources: SourceDraft[]): AuthSourceConfig[] => sources.map(({ locked: _locked, expanded: _expanded, ...source }) => source);
const loadedDraft = (settings: AuthSourcesSettings, previous: SourceDraft[] = []): Draft => ({ ...settings, sources: settings.sources.map((source) => ({ ...source, locked: true, expanded: previous.find((s) => s.tag === source.tag)?.expanded ?? false })), original: JSON.stringify(settings.sources) });

export function AuthSourcesPage() {
  const { t } = useTranslation("authSources");
  const query = useAsync(api.getAuthSources, []);
  const reauth = useReauth();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState<number | null>(null);
  const [checks, setChecks] = useState<Record<number, { kind: "success" | "error"; text: string }>>({});
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  useEffect(() => {
    if (query.data) setDraft((current) => current && JSON.stringify(wireSources(current.sources)) !== current.original ? current : loadedDraft(query.data!, current?.sources));
  }, [query.data]);
  const dirty = draft !== null && JSON.stringify(wireSources(draft.sources)) !== draft.original;
  useUnsavedGuard(dirty);
  const busy = saving || testing !== null || query.loading;
  const valid = draft !== null && draft.sources.every(validSource) &&
    new Set(draft.sources.map((s) => s.tag)).size === draft.sources.length &&
    new Set(draft.sources.map((s) => s.prefix.toLowerCase())).size === draft.sources.length;

  function edit(sources: SourceDraft[]) {
    setDraft((current) => current && { ...current, sources });
    setChecks({});
    setMessage(null);
  }

  function discard() {
    if (!draft) return;
    setDraft(loadedDraft({ ...draft, sources: JSON.parse(draft.original) as AuthSourceConfig[] }, draft.sources));
    setChecks({});
    setMessage(null);
  }

  async function save() {
    if (!draft || !dirty || !valid || busy) return;
    setSaving(true);
    setMessage(null);
    try {
      const saved = await reauth.guard(() => api.setAuthSources(wireSources(draft.sources), draft.revision));
      setDraft((current) => loadedDraft(saved, current?.sources));
      setMessage({ kind: "success", text: t("saved") });
    } catch (error) {
      if (!isReauthCancelled(error)) setMessage({ kind: "error", text: humanizeError(error) });
    } finally { setSaving(false); }
  }

  async function testSource(index: number) {
    if (!draft || busy || !validSource(draft.sources[index])) return;
    setTesting(index);
    try {
      const result = await api.testAuthSource(wireSources(draft.sources)[index]);
      setChecks((current) => ({ ...current, [index]: { kind: result.ok ? "success" : "error", text: result.ok ? t("test_ok", { ms: result.elapsed_ms }) : t("test_status", { status: result.status }) } }));
    } catch (error) {
      setChecks((current) => ({ ...current, [index]: { kind: "error", text: humanizeError(error) } }));
    } finally { setTesting(null); }
  }

  return <div className="space-y-6">
    <PageHeader icon={ShieldCheck} title={t("title")} subtitle={t("subtitle")} />
    <Card className="rounded-xl shadow-none"><CardContent className="flex items-start gap-3 pt-5">
      <ShieldCheck className="mt-0.5 h-5 w-5 shrink-0 text-primary" />
      <div className="space-y-1"><p className="text-sm font-semibold">{t("builtin_title")}</p><p className="text-xs leading-relaxed text-muted-foreground">{t("builtin_hint")}</p></div>
    </CardContent></Card>
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div><h2 className="text-sm font-semibold">{t("custom_title")}</h2><p className="mt-1 text-xs text-muted-foreground">{t("priority_hint")}</p></div>
      <div className="flex gap-2">
        <Button variant="outline" size="sm" disabled={!draft || dirty || busy || query.loading} onClick={query.reload}><RefreshCw className={cn(query.loading && "animate-spin")} />{t("reload")}</Button>
        <Button size="sm" disabled={!draft || busy || draft.sources.length >= 32} onClick={() => draft && edit([...draft.sources, { ...emptySource(), expanded: true }])}><Plus />{t("add")}</Button>
      </div>
    </div>
    {query.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("loading")}</p>}
    {query.error != null && <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-3"><InlineError message={humanizeError(query.error)} /><Button variant="outline" size="sm" disabled={dirty || busy} onClick={query.reload}>{t("common:try_again")}</Button></div>}
    <div aria-busy={query.loading} className="space-y-4">
      {(draft?.sources ?? [emptySource()]).map((source, index) => {
        const locked = "locked" in source && source.locked === true;
        const expanded = "expanded" in source ? source.expanded === true : true;
        const set = (patch: Partial<AuthSourceConfig>) => draft && edit(draft.sources.map((s, i) => i === index ? { ...s, ...patch } : s));
        const move = (offset: number) => {
          if (!draft) return;
          const sources = [...draft.sources];
          [sources[index], sources[index + offset]] = [sources[index + offset], sources[index]];
          edit(sources);
        };
        return <Card key={index} className="overflow-hidden rounded-xl shadow-none">
          <CardHeader className={cn("flex-row flex-wrap items-center justify-between gap-3 space-y-0 bg-muted/20 py-4", expanded && "border-b border-border/60")}>
            <CardTitle className="min-w-0 flex-1 text-sm">
              <Button variant="ghost" className="h-auto w-full justify-start whitespace-normal p-0 text-left hover:bg-transparent active:scale-100" aria-expanded={expanded} aria-controls={`source-fields-${index}`} aria-label={t(expanded ? "collapse" : "expand", { name: source.tag || t("new_source") })} disabled={!draft} onClick={() => setDraft((current) => current && { ...current, sources: current.sources.map((s, i) => i === index ? { ...s, expanded: !expanded } : s) })}>
                <ChevronRight className={cn("transition-transform motion-reduce:transition-none", expanded && "rotate-90")} />
                <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-xs font-semibold text-primary">{index + 2}</span>
                <span className="break-all">{source.tag || t(draft ? "new_source" : "loading")}</span>
              </Button>
            </CardTitle>
            <div className="flex items-center gap-2">
              <label className="flex cursor-pointer items-center gap-2 text-xs"><Switch checked={draft !== null && source.enabled} onCheckedChange={(enabled) => set({ enabled })} disabled={!draft || busy} />{t("enabled")}</label>
              <Button variant="outline" size="icon" className="h-8 w-8" aria-label={t("move_up")} disabled={!draft || busy || index === 0} onClick={() => move(-1)}><ArrowUp /></Button>
              <Button variant="outline" size="icon" className="h-8 w-8" aria-label={t("move_down")} disabled={!draft || busy || index === draft.sources.length - 1} onClick={() => move(1)}><ArrowDown /></Button>
              {!locked && <Button variant="outline" size="icon" className="h-8 w-8" aria-label={t("remove")} disabled={!draft || busy} onClick={() => draft && edit(draft.sources.filter((_, i) => i !== index))}><Trash2 /></Button>}
            </div>
          </CardHeader>
          <CardContent id={`source-fields-${index}`} hidden={!expanded} className="space-y-4 pt-5">
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2"><Label htmlFor={`source-tag-${index}`}>{t("tag")}</Label><Input id={`source-tag-${index}`} value={source.tag} readOnly={locked} disabled={!draft || busy} placeholder="littleskin" maxLength={128} onChange={(e) => set({ tag: e.target.value })} /><p className="text-xs text-muted-foreground">{t(locked ? "tag_locked" : "tag_hint")}</p></div>
              <div className="space-y-2"><Label htmlFor={`source-prefix-${index}`}>{t("prefix")}</Label><Input id={`source-prefix-${index}`} value={source.prefix} disabled={!draft || busy} placeholder="LS" maxLength={4} onChange={(e) => set({ prefix: e.target.value })} /><p className="text-xs text-muted-foreground">{t("prefix_hint")}</p></div>
            </div>
            <div className="space-y-2"><Label htmlFor={`source-url-${index}`}>{t("url")}</Label><Input id={`source-url-${index}`} type="url" value={source.url} disabled={!draft || busy} placeholder="https://example.org/api/yggdrasil/sessionserver/session/minecraft/hasJoined" onChange={(e) => set({ url: e.target.value })} /><p className="text-xs text-muted-foreground">{t("url_hint")}</p></div>
            <div className="space-y-2"><Label htmlFor={`source-api-${index}`}>{t("api_url")}</Label><Input id={`source-api-${index}`} type="url" value={source.api_url} disabled={!draft || busy} placeholder="https://example.org/api/yggdrasil" onChange={(e) => set({ api_url: e.target.value })} /><p className="text-xs text-muted-foreground">{t("api_hint")}</p></div>
            <div className="flex flex-wrap items-center gap-3"><Button variant="outline" size="sm" disabled={!draft || busy || !validSource(source)} onClick={() => void testSource(index)}>{testing === index ? <Loader2 className="animate-spin" /> : <Check />}{t(testing === index ? "testing" : "test")}</Button><p className="text-xs text-muted-foreground">{t("test_hint")}</p></div>
            {checks[index] && <MessageLine kind={checks[index].kind} message={checks[index].text} />}
          </CardContent>
        </Card>;
      })}
      {draft?.sources.length === 0 && <Card className="border-dashed shadow-none"><CardContent className="py-8 text-center text-sm text-muted-foreground">{t("empty")}</CardContent></Card>}
    </div>
    <p className="text-xs leading-relaxed text-muted-foreground">{t("disable_hint")}</p>
    {message && <MessageLine kind={message.kind} message={message.text} />}
    {draft && !valid && <InlineError message={t("invalid")} />}
    <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-card p-3">
      <p className="text-xs text-muted-foreground">{draft ? t(dirty ? "unsaved" : draft.managed ? "active" : "defaults") : query.loading ? t("loading") : ""}</p>
      <div className="flex gap-2">
        {dirty && <Button variant="outline" disabled={busy} onClick={discard}>{t("discard")}</Button>}
        <Button disabled={!dirty || !valid || busy} onClick={() => void save()}>{saving ? <Loader2 className="animate-spin" /> : <Save />}{t(saving ? "saving" : "save")}</Button>
      </div>
    </div>
    {reauth.dialog}
  </div>;
}
