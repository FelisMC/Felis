import { useMemo, useState } from "react";
import { Loader2, Play, Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { HostTaskNotice, HostTaskStatus, useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";
import { PhaseBadge, shownPhase } from "@/components/PhaseBadge";
import { SearchInput } from "@/components/SearchInput";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useRefreshSignal } from "@/lib/hooks";
import type { FleetServer } from "@/lib/types";
import { cn } from "@/lib/utils";

const byLabel = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });
const label = (s: FleetServer) => s.displayName || s.name;

// matches looks for every word of the search in the server's names, owner and tags.
function matches(s: FleetServer, query: string): boolean {
  const haystack = [s.name, s.displayName, s.owner, ...(s.tags ?? [])].filter(Boolean).join(" ").toLowerCase();
  return query.toLowerCase().split(/\s+/).every((word) => haystack.includes(word));
}

// Live backup disables saving only during world copying.
export function WorldBackupCard() {
  const { t } = useTranslation("admin");
  const host = useHostTask(["backup-now"]);
  const fleet = useAsync(api.fleet, []);
  useRefreshSignal(fleet.reload);
  const servers = useMemo(
    () => (fleet.data ?? []).filter((s) => !s.system).sort((a, b) => byLabel.compare(label(a), label(b))),
    [fleet.data],
  );
  const tags = useMemo(() => [...new Set(servers.flatMap((s) => s.tags ?? []))].sort(byLabel.compare), [servers]);
  const [all, setAll] = useState(true);
  const [chosen, setChosen] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [tag, setTag] = useState<string | null>(null);
  const shown = servers.filter((s) => (!tag || (s.tags ?? []).includes(tag)) && (!query.trim() || matches(s, query.trim())));
  const [stop, setStop] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const ready = all || chosen.length > 0;
  function request() {
    const params: Record<string, string> = {};
    if (stop) params.stop = "true";
    if (!all) params.servers = chosen.join(",");
    return host.start({ action: "backup-now", params, confirmMaintenance: stop });
  }
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><Save className="h-4 w-4 text-primary" aria-hidden="true" />{t("world_backup_title")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("world_backup_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <div className="flex flex-wrap gap-2" role="radiogroup" aria-label={t("world_backup_scope")}>
          {[true, false].map((value) => (
            <Button key={String(value)} type="button" role="radio" aria-checked={all === value} variant="outline" size="sm" disabled={host.busy}
              className={all === value ? "border-primary/40 bg-primary/5 text-primary" : ""} onClick={() => setAll(value)}>
              {t(value ? "world_backup_all" : "world_backup_some")}
            </Button>
          ))}
        </div>
        {!all && (
          <fieldset className="space-y-2">
            <legend className="sr-only">{t("world_backup_some")}</legend>
            {fleet.error != null && <MessageLine kind="error" message={humanizeError(fleet.error)} />}
            {servers.length === 0 && !fleet.loading && <p className="text-sm text-muted-foreground">{t("world_backup_no_servers")}</p>}
            {servers.length > 0 && <>
              <div className="flex flex-wrap items-center gap-2">
                <SearchInput value={query} onChange={setQuery} placeholder={t("world_backup_search")} className="max-w-sm" />
                <Button type="button" variant="outline" size="sm" disabled={host.busy || shown.length === 0}
                  onClick={() => setChosen((list) => [...new Set([...list, ...shown.map((s) => s.name)])])}>
                  {t("world_backup_select_shown")}
                </Button>
                <Button type="button" variant="ghost" size="sm" disabled={host.busy || chosen.length === 0} onClick={() => setChosen([])}>
                  {t("world_backup_clear")}
                </Button>
                <span role="status" className="ml-auto text-xs text-muted-foreground">{t("world_backup_selected", { count: chosen.length, total: servers.length })}</span>
              </div>
              {tags.length > 0 && (
                <div role="group" aria-label={t("world_backup_tags")} className="flex flex-wrap items-center gap-1.5">
                  <span className="mr-1 text-xs text-muted-foreground">{t("world_backup_tags")}</span>
                  {tags.map((name) => (
                    <button key={name} type="button" aria-pressed={tag === name} onClick={() => setTag(tag === name ? null : name)}
                      className={cn("rounded-full border px-2.5 py-0.5 text-xs transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                        tag === name ? "border-primary bg-primary text-primary-foreground" : "border-border bg-card text-muted-foreground hover:bg-accent hover:text-foreground")}>
                      {name}
                    </button>
                  ))}
                </div>
              )}
              <ul className="max-h-72 divide-y divide-border overflow-y-auto rounded-lg border border-border">
                {shown.map((s) => (
                  <li key={s.name}>
                    <label className="flex cursor-pointer items-center gap-3 px-3 py-2 text-sm hover:bg-accent/40">
                      <Checkbox checked={chosen.includes(s.name)} disabled={host.busy}
                        onChange={(e) => { const on = e.target.checked; setChosen((list) => on ? [...list, s.name] : list.filter((n) => n !== s.name)); }} />
                      <span className="min-w-0 flex-1">
                        <span className="block truncate">{label(s)}</span>
                        <span className="block truncate text-xs text-muted-foreground"><span className="font-mono">{s.name}</span>{s.owner && ` · ${s.owner}`}</span>
                      </span>
                      <PhaseBadge phase={shownPhase(s)} />
                    </label>
                  </li>
                ))}
                {shown.length === 0 && <li className="px-3 py-6 text-center text-sm text-muted-foreground">{t("world_backup_no_match")}</li>}
              </ul>
            </>}
          </fieldset>
        )}
        <div className="flex items-start justify-between gap-4 rounded-lg border border-border p-4">
          <div className="space-y-1">
            <Label htmlFor="world-backup-stop">{t("world_backup_stop")}</Label>
            <p className="text-xs leading-relaxed text-muted-foreground">{t("world_backup_stop_hint")}</p>
          </div>
          <Switch id="world-backup-stop" checked={stop} disabled={host.busy} onCheckedChange={setStop} />
        </div>
        <Button disabled={!host.available || host.busy || !ready} onClick={() => (stop ? setConfirming(true) : void request())}>
          {host.starting ? <Loader2 className="animate-spin" /> : <Play />}{all || chosen.length === 0 ? t("world_backup_start") : t("world_backup_start_some", { count: chosen.length })}
        </Button>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={t("world_backup_stop_title")}
        description={t("world_backup_stop_confirm")}
        confirmLabel={t("world_backup_start")}
        onConfirm={async () => { await request(); }}
      />
      {host.reauth.dialog}
    </Card>
  );
}
