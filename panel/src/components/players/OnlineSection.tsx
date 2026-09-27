import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Ban, Loader2, LogOut, RotateCw, Users } from "lucide-react";
import { Button } from "@/components/ui/button";
import { InlineConfirm } from "@/components/InlineConfirm";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import {
  CollapsibleSection,
  FeedbackLine,
  LoadError,
  PagerFooter,
  rconReply,
  SearchBox,
  usePagedNames,
  type Feedback,
} from "./shared";

type RowAction = "kick" | "ban";

/** OnlineSection is the live roster: who is on the server right now, each with a
 *  one-click kick or ban (both two-step confirmed, since both are disruptive). It
 *  is the ONLY place the panel learns WHO is online — status carries the count
 *  alone — so it reads the RCON "list" reply on demand. Refresh is MANUAL (a button
 *  + a last-updated stamp), never a timer: auto-polling would fire an RCON command
 *  per viewer forever, and the roster does not move fast enough to justify it. */
export function OnlineSection({ name, defaultOpen = true }: { name: string; defaultOpen?: boolean }) {
  const { t } = useTranslation("servers");
  const { data, error, loading, reload } = useAsync(() => api.accessPlayers(name), [name]);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const [confirming, setConfirming] = useState<{ player: string; action: RowAction } | null>(null);
  const [pending, setPending] = useState<{ player: string; action: RowAction } | null>(null);
  const [fb, setFb] = useState<Feedback>(null);

  // Stamp the last successful read so the roster's freshness is always visible —
  // the honest counterpart to not auto-refreshing.
  useEffect(() => {
    if (data) setUpdatedAt(new Date());
  }, [data]);

  const online = data?.online ?? 0;
  const max = data?.max ?? 0;
  const players = useMemo(() => data?.players ?? [], [data]);
  const raw = data?.output?.trim() ?? "";
  // The tally says someone is on but no names parsed (a non-vanilla "list" format):
  // report the count honestly and point at the raw reply rather than a false empty.
  const namesUnavailable = online > 0 && players.length === 0;

  const { query, onQuery, q, shown, showSearch, pageItems, pageCount, clampedPage, needFooter, setPage } =
    usePagedNames(players);

  const run = useCallback(
    async (playerName: string, action: RowAction) => {
      setFb(null);
      setPending({ player: playerName, action });
      try {
        const res =
          action === "kick"
            ? await api.accessKick(name, playerName)
            : await api.accessBan(name, "ban", playerName);
        setFb({
          kind: "ok",
          msg: rconReply(
            res.output,
            t(action === "kick" ? "access_kicked" : "access_banned", { player: playerName }),
          ),
        });
        reload(); // the player just left — refresh so the roster reflects it
      } catch (e) {
        setFb({ kind: "err", msg: humanizeError(e) });
      } finally {
        setPending(null);
        setConfirming(null);
      }
    },
    [name, reload, t],
  );

  return (
    <CollapsibleSection
      icon={<Users className="h-4 w-4" />}
      title={t("access_online_title")}
      count={!loading && !error ? (max > 0 ? `${online} / ${max}` : online) : undefined}
      defaultOpen={defaultOpen}
      actions={
        <Button
          variant="ghost"
          size="icon"
          className="h-8 w-8 text-muted-foreground"
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
        {/* Freshness stamp — the honest counterpart to manual refresh — sits with the
            section's description now that the card header is just the collapsed index. */}
        <div className="flex items-center justify-between gap-2">
          <p className="text-sm text-muted-foreground">{t("access_online_desc")}</p>
          {updatedAt && !loading && (
            <span className="shrink-0 text-xs text-muted-foreground">
              {t("access_updated_at", { time: updatedAt.toLocaleTimeString() })}
            </span>
          )}
        </div>

        {loading && !data ? (
          <div className="flex items-center gap-2 py-2 text-xs text-muted-foreground">
            <Loader2 className="h-3.5 w-3.5 animate-spin" /> {t("log_connecting")}
          </div>
        ) : error ? (
          <LoadError message={t("access_online_load_error")} error={error} />
        ) : players.length === 0 ? (
          <div className="rounded-md border border-dashed border-border bg-muted/20 px-4 py-8 text-center">
            {namesUnavailable ? (
              <>
                <p className="text-sm text-muted-foreground">
                  {t("access_online_names_unavailable", { count: online })}
                </p>
                <p className="mt-1 text-xs text-muted-foreground/80">
                  {t("access_online_names_unavailable_hint")}
                </p>
              </>
            ) : (
              <p className="text-sm text-muted-foreground">{t("access_online_empty")}</p>
            )}
          </div>
        ) : (
          <div className="space-y-2">
            {showSearch && <SearchBox value={query} onChange={onQuery} />}

            <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2 md:grid-cols-3 xl:grid-cols-4">
              {shown.length === 0 ? (
                <div className="col-span-full py-6 text-center text-xs text-muted-foreground">
                  {t("access_search_no_match", { query: query.trim() })}
                </div>
              ) : (
                pageItems.map((p) => {
                  // Narrow here so confirming.action is non-null inside the branch.
                  const c = confirming && confirming.player === p ? confirming : null;
                  return (
                    <div
                      key={p}
                      className="flex items-center justify-between gap-3 rounded-lg border border-border bg-card/25 p-2.5 transition-all hover:border-primary/20 hover:bg-accent/40"
                    >
                      <span className="flex min-w-0 items-center gap-2">
                        <span className="relative flex h-2 w-2 shrink-0">
                          <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75"></span>
                          <span className="relative inline-flex h-2 w-2 rounded-full bg-emerald-500"></span>
                        </span>
                        <span className="truncate font-mono text-sm font-medium">{p}</span>
                      </span>
                      {/* Both kick and ban are disruptive, so each arms a one-step
                          inline confirm before it fires (no native confirm()). */}
                      {c ? (
                        <InlineConfirm
                          open={true}
                          confirming={pending !== null}
                          onConfirm={() => run(p, c.action)}
                          onCancel={() => setConfirming(null)}
                          confirmLabel={c.action === "kick" ? t("access_kick_btn") : t("access_ban_btn")}
                          cancelLabel={t("access_cancel")}
                          size="sm"
                          className="flex shrink-0 items-center gap-1"
                        />
                      ) : (
                        <div className="flex shrink-0 items-center gap-1">
                          <Button
                            variant="outline"
                            size="sm"
                            className="h-7 px-2 text-xs gap-1"
                            onClick={() => setConfirming({ player: p, action: "kick" })}
                            disabled={pending !== null}
                            title={t("access_kick_btn")}
                          >
                            <LogOut className="h-3.5 w-3.5 shrink-0" />
                            <span>{t("access_kick_btn")}</span>
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            className="h-7 px-2 text-xs text-destructive hover:bg-destructive/10 hover:text-destructive gap-1"
                            onClick={() => setConfirming({ player: p, action: "ban" })}
                            disabled={pending !== null}
                            title={t("access_ban_btn")}
                          >
                            <Ban className="h-3.5 w-3.5 shrink-0" />
                            <span>{t("access_ban_btn")}</span>
                          </Button>
                        </div>
                      )}
                    </div>
                  );
                })
              )}
            </div>

            {needFooter && (
              <PagerFooter
                q={q}
                shownCount={shown.length}
                total={players.length}
                pageCount={pageCount}
                clampedPage={clampedPage}
                onPage={setPage}
              />
            )}
          </div>
        )}

        {/* Raw RCON reply — the ground truth for names when the parse can't tokenize
            a non-vanilla "list" format. Collapsed by default. */}
        {!loading && !error && raw && (
          <details className="text-xs">
            <summary className="cursor-pointer select-none text-muted-foreground transition-colors hover:text-foreground">
              {t("access_online_raw")}
            </summary>
            <pre className="mt-1.5 whitespace-pre-wrap break-words rounded-md border border-border bg-muted/30 p-2.5 font-mono text-foreground">
              {raw}
            </pre>
          </details>
        )}

        <FeedbackLine fb={fb} />
      </div>
    </CollapsibleSection>
  );
}
