import { useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { AlarmClock, Loader2, RotateCw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api, humanizeError } from "@/lib/api";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import type { AllowlistEntry, AutostartPolicy } from "@/lib/types";
import { cn } from "@/lib/utils";
import { CollapsibleSection, FeedbackLine, PagerFooter, SearchBox, usePagedNames, type Feedback } from "./shared";

/** WakeListSection shows who may wake the server while it sleeps under the "wake
 *  list" autostart policy, and lets the owner take that right away or give it back.
 *  Felis keeps this list itself, so unlike the other player blocks it works whether
 *  the server is running or not — which is when it matters most. A player lands on
 *  it by joining once; a revoked row stays (marked) so a rejoin cannot undo the
 *  owner's choice, which is also why the action is a reversible toggle and needs
 *  no confirm step. */
export function WakeListSection({
  name,
  policy,
  defaultOpen = false,
}: {
  name: string;
  policy?: AutostartPolicy;
  defaultOpen?: boolean;
}) {
  const { t, i18n } = useTranslation("servers");
  const { data, error, loading, reload } = useAsync(() => api.serverAllowlist(name), [name]);
  const [busy, setBusy] = useState<string | null>(null);
  const [fb, setFb] = useState<Feedback>(null);

  const entries = useMemo(() => data ?? [], [data]);
  const label = useCallback((e: AllowlistEntry) => e.username || t("wake_list_unlinked"), [t]);
  // The shared list engine filters strings; each entry is searchable by its account
  // name and its UUID, and the key maps a page back to its rows.
  const byKey = useMemo(
    () => new Map(entries.map((e) => [`${e.username ?? ""}\u0000${e.mc_uuid}`, e])),
    [entries],
  );
  const keys = useMemo(() => [...byKey.keys()], [byKey]);
  const { query, onQuery, q, shown, showSearch, pageItems, pageCount, clampedPage, needFooter, setPage } =
    usePagedNames(keys);

  const toggle = useCallback(
    async (e: AllowlistEntry) => {
      const player = label(e);
      setFb(null);
      setBusy(e.mc_uuid);
      try {
        await api.setAllowlistWake(name, e.mc_uuid, !e.can_wake);
        setFb({
          kind: "ok",
          msg: t(e.can_wake ? "wake_list_revoked" : "wake_list_restored", { player }),
        });
        reload();
      } catch (err) {
        setFb({ kind: "err", msg: humanizeError(err) });
      } finally {
        setBusy(null);
      }
    },
    [name, label, reload, t],
  );

  const now = Date.now();
  const inactive = policy === "ownerOnly" || policy === "public";

  return (
    <CollapsibleSection
      icon={<AlarmClock className="h-4 w-4" />}
      title={t("wake_list_title")}
      count={!loading && !error ? entries.length : undefined}
      defaultOpen={defaultOpen}
      actions={
        <Button
          variant="ghost"
          size="icon"
          className="h-8 w-8 shrink-0 text-muted-foreground"
          onClick={reload}
          disabled={loading}
          title={t("access_refresh")}
          aria-label={t("access_refresh")}
        >
          <RotateCw className={loading ? "h-4 w-4 animate-spin" : "h-4 w-4"} />
        </Button>
      }
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">{t("wake_list_desc")}</p>
        {inactive && (
          <p className="rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
            {t(policy === "public" ? "wake_list_inactive_public" : "wake_list_inactive_owner")}
          </p>
        )}

        {loading && !data ? (
          <div className="flex items-center gap-2 py-2 text-xs text-muted-foreground">
            <Loader2 className="h-3.5 w-3.5 animate-spin" /> {t("log_connecting")}
          </div>
        ) : error ? (
          <p role="alert" className="text-xs text-destructive">
            {t("wake_list_load_error")}
          </p>
        ) : entries.length === 0 ? (
          <div className="rounded-md border border-dashed border-border bg-muted/20 px-4 py-8 text-center">
            <p className="text-sm text-muted-foreground">{t("wake_list_empty")}</p>
            <p className="mt-1 text-xs text-muted-foreground/80">{t("wake_list_empty_hint")}</p>
          </div>
        ) : (
          <div className={cn("space-y-2 transition-opacity", loading && "pointer-events-none opacity-60")}>
            {showSearch && <SearchBox value={query} onChange={onQuery} />}

            <ul className="grid grid-cols-1 gap-2.5 md:grid-cols-2">
              {shown.length === 0 ? (
                <li className="col-span-full py-6 text-center text-xs text-muted-foreground">
                  {t("access_search_no_match", { query: query.trim() })}
                </li>
              ) : (
                pageItems.map((key) => {
                  const e = byKey.get(key)!;
                  const player = label(e);
                  return (
                    <li
                      key={e.mc_uuid}
                      className={cn(
                        "flex items-center justify-between gap-3 rounded-lg border border-border p-2.5 transition-colors",
                        e.can_wake ? "bg-card/25 hover:bg-accent/40" : "bg-muted/30",
                      )}
                    >
                      <div className="min-w-0">
                        <div className="flex min-w-0 items-center gap-2">
                          <span
                            className={cn(
                              "truncate text-sm font-medium",
                              (!e.username || !e.can_wake) && "text-muted-foreground",
                            )}
                          >
                            {player}
                          </span>
                          {!e.can_wake && (
                            <Badge variant="muted" className="shrink-0 font-normal">
                              {t("wake_list_revoked_badge")}
                            </Badge>
                          )}
                        </div>
                        <p className="truncate text-[11px] text-muted-foreground">
                          <span className="font-mono" title={e.mc_uuid}>
                            {e.mc_uuid.slice(0, 8)}
                          </span>
                          {" · "}
                          <span title={formatAbsolute(e.added_at, i18n.language)}>
                            {t("wake_list_joined", { when: formatRelative(e.added_at, now, i18n.language) })}
                          </span>
                        </p>
                      </div>
                      <Button
                        variant={e.can_wake ? "ghost" : "outline"}
                        size="sm"
                        className={cn("h-8 shrink-0", e.can_wake && "text-muted-foreground hover:text-destructive")}
                        onClick={() => toggle(e)}
                        disabled={busy !== null}
                        aria-label={t(e.can_wake ? "wake_list_revoke_aria" : "wake_list_restore_aria", { player })}
                      >
                        {busy === e.mc_uuid && <Loader2 className="h-3.5 w-3.5 animate-spin" />}
                        {t(e.can_wake ? "wake_list_revoke" : "wake_list_restore")}
                      </Button>
                    </li>
                  );
                })
              )}
            </ul>

            {needFooter && (
              <PagerFooter
                q={q}
                shownCount={shown.length}
                total={entries.length}
                pageCount={pageCount}
                clampedPage={clampedPage}
                onPage={setPage}
              />
            )}
          </div>
        )}

        <FeedbackLine fb={fb} />
      </div>
    </CollapsibleSection>
  );
}
