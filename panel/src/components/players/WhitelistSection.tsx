import { useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { ListChecks, Loader2, Plus, RotateCw, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { InlineConfirm } from "@/components/InlineConfirm";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { cn } from "@/lib/utils";
import {
  CollapsibleSection,
  FeedbackLine,
  LoadError,
  MC_NAME,
  PagerFooter,
  PlayerField,
  rconReply,
  SearchBox,
  usePagedNames,
  type Feedback,
} from "./shared";

/** WhitelistSection manages the server's join whitelist: add a name, remove any
 *  entry, and read the current list. Built to stay usable at a few hundred names —
 *  a live count, a filter and paging (via usePagedNames) once the list is long
 *  enough to need them, and a two-step remove so a name never vanishes on one tap. */
export function WhitelistSection({ name, defaultOpen = false }: { name: string; defaultOpen?: boolean }) {
  const { t } = useTranslation("servers");
  const { data, error, loading, reload } = useAsync(
    () => api.accessWhitelistList(name),
    [name],
  );
  const [value, setValue] = useState("");
  const [touched, setTouched] = useState(false);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<string | null>(null);
  const [fb, setFb] = useState<Feedback>(null);

  const player = value.trim();
  const valid = MC_NAME.test(player);
  const invalid = touched && player.length > 0 && !valid;

  const players = useMemo(() => data?.players ?? [], [data]);
  // The server always echoes the raw RCON text. Parsed `players` drives the list /
  // empty state; `raw` is kept as an always-available, low-key disclosure — it is
  // ground truth when the vanilla-only parse can't tokenize a plugin or localized
  // whitelist (the names are in `output` even when `players` came back empty).
  const raw = data?.output?.trim() ?? "";

  const { query, onQuery, q, shown, showSearch, pageItems, pageCount, clampedPage, needFooter, setPage } =
    usePagedNames(players);

  const add = useCallback(async () => {
    if (!valid || adding) {
      setTouched(true);
      return;
    }
    setFb(null);
    setAdding(true);
    try {
      const res = await api.accessWhitelist(name, "add", player);
      setFb({
        kind: "ok",
        msg: rconReply(res.output, t("access_whitelist_added", { player })),
      });
      setValue("");
      setTouched(false);
      reload();
    } catch (e) {
      setFb({ kind: "err", msg: humanizeError(e) });
    } finally {
      setAdding(false);
    }
  }, [name, player, valid, adding, reload, t]);

  const remove = useCallback(
    async (p: string) => {
      setFb(null);
      setRemoving(p);
      try {
        const res = await api.accessWhitelist(name, "remove", p);
        setFb({
          kind: "ok",
          msg: rconReply(res.output, t("access_whitelist_removed", { player: p })),
        });
        reload();
      } catch (e) {
        setFb({ kind: "err", msg: humanizeError(e) });
      } finally {
        setRemoving(null);
        setConfirming(null);
      }
    },
    [name, reload, t],
  );

  return (
    <CollapsibleSection
      icon={<ListChecks className="h-4 w-4" />}
      title={t("access_whitelist_title")}
      count={!loading && !error ? players.length : undefined}
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
        <p className="text-sm text-muted-foreground">{t("access_whitelist_desc")}</p>

        {/* Add row — the primary action, kept at the top so it is always in reach. */}
        <div className="space-y-1.5">
          <div className="flex items-start gap-2">
            <div className="flex-1">
              <PlayerField
                value={value}
                onChange={setValue}
                onEnter={add}
                invalid={invalid}
                disabled={adding}
              />
            </div>
            <Button size="sm" className="h-9" onClick={add} disabled={!valid || adding}>
              {adding ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Plus className="h-4 w-4" />
              )}
              {t("access_whitelist_add")}
            </Button>
          </div>
          {invalid && <p className="text-xs text-destructive">{t("access_player_invalid")}</p>}
        </div>

        {loading && !data ? (
          <div className="flex items-center gap-2 py-2 text-xs text-muted-foreground">
            <Loader2 className="h-3.5 w-3.5 animate-spin" /> {t("log_connecting")}
          </div>
        ) : error ? (
          <LoadError message={t("access_whitelist_load_error")} error={error} />
        ) : players.length === 0 ? (
          <div className="rounded-md border border-dashed border-border bg-muted/20 px-4 py-8 text-center">
            <p className="text-sm text-muted-foreground">{t("access_whitelist_empty")}</p>
            <p className="mt-1 text-xs text-muted-foreground/80">
              {t("access_whitelist_empty_hint")}
            </p>
          </div>
        ) : (
          <div className={cn("space-y-2 transition-opacity", loading && "opacity-60 pointer-events-none")}>
            {showSearch && <SearchBox value={query} onChange={onQuery} />}

            <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2 md:grid-cols-3 xl:grid-cols-4">
              {shown.length === 0 ? (
                <div className="col-span-full py-6 text-center text-xs text-muted-foreground">
                  {t("access_search_no_match", { query: query.trim() })}
                </div>
              ) : (
                pageItems.map((p) => (
                  <div
                    key={p}
                    className="flex items-center justify-between gap-3 rounded-lg border border-border bg-card/25 p-2.5 transition-all hover:border-primary/20 hover:bg-accent/40"
                  >
                    <span className="truncate font-mono text-sm font-medium">{p}</span>
                    {/* Removal asks once before it fires: one click arms the row (X →
                        取消 / 移除), a second confirms. Recoverable, but a name gone on
                        a single stray tap is exactly the surprise to avoid. */}
                    {confirming === p ? (
                      <InlineConfirm
                        open={true}
                        confirming={removing !== null}
                        onConfirm={() => remove(p)}
                        onCancel={() => setConfirming(null)}
                        confirmLabel={t("access_remove")}
                        cancelLabel={t("access_cancel")}
                        size="sm"
                        className="flex shrink-0 items-center gap-1"
                      />
                    ) : (
                      <button
                        type="button"
                        onClick={() => setConfirming(p)}
                        disabled={removing !== null}
                        aria-label={t("access_whitelist_remove", { player: p })}
                        title={t("access_whitelist_remove", { player: p })}
                        className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-destructive/10 hover:text-destructive disabled:opacity-40"
                      >
                        <X className="h-3.5 w-3.5" />
                      </button>
                    )}
                  </div>
                ))
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

        {/* The server's verbatim reply, kept as an opt-in disclosure — the escape
            hatch when the vanilla-only parse can't tokenize a plugin or localized
            whitelist. Collapsed by default so it never clutters the common case. */}
        {!loading && !error && raw && (
          <details className="text-xs">
            <summary className="cursor-pointer select-none text-muted-foreground transition-colors hover:text-foreground">
              {t("access_whitelist_raw")}
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
