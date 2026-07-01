import { useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Ban, Loader2, RotateCw, Undo2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import {
  CollapsibleSection,
  FeedbackLine,
  MC_NAME,
  PagerFooter,
  PlayerField,
  SearchBox,
  usePagedNames,
  type Feedback,
} from "./shared";

/** BansSection is the ban roster: view who is blocked from the server, pardon any
 *  of them in one tap, and ban an arbitrary player by ID (the one player action
 *  whose target is NOT already on screen). Built to stay usable at a few hundred
 *  names — a live count, filter + paging (via usePagedNames), and a manual refresh.
 *
 *  Two deliberately DIFFERENT confirmations, weighted by consequence:
 *  - Banning a typed-in name is the most surprising/destructive action here (the
 *    target isn't in front of you), so it arms a FULL-SENTENCE confirm that names
 *    the consequence — and Enter only ARMS it, never fires the ban.
 *  - Pardoning is recoverable (re-ban is one tap) but still security-relevant (it
 *    lets someone back in), so it gets a lighter one-step inline confirm per row. */
export function BansSection({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  const { data, error, loading, reload } = useAsync(() => api.accessBanList(name), [name]);
  const [value, setValue] = useState("");
  const [touched, setTouched] = useState(false);
  const [armed, setArmed] = useState(false); // ban add-row: confirm shown, not yet fired
  const [banning, setBanning] = useState(false);
  const [pardoning, setPardoning] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<string | null>(null); // pardon row armed
  const [fb, setFb] = useState<Feedback>(null);

  const player = value.trim();
  const valid = MC_NAME.test(player);
  const invalid = touched && player.length > 0 && !valid;

  const players = useMemo(() => data?.players ?? [], [data]);
  // Raw RCON reply is ground truth: the vanilla-only parse can come back empty on a
  // plugin or localized "banlist" format while `output` still names the bans, so it
  // stays available as a low-key disclosure rather than showing a false "no bans".
  const raw = data?.output?.trim() ?? "";

  const { query, onQuery, q, shown, showSearch, pageItems, pageCount, clampedPage, needFooter, setPage } =
    usePagedNames(players);

  // Enter and the Ban button only ARM the confirm — the ban never fires without the
  // deliberate second click on the full-sentence confirmation below.
  const arm = useCallback(() => {
    if (!valid) {
      setTouched(true);
      return;
    }
    setFb(null);
    setArmed(true);
  }, [valid]);

  const ban = useCallback(async () => {
    if (!valid || banning) return;
    setFb(null);
    setBanning(true);
    try {
      await api.accessBan(name, "ban", player);
      setFb({ kind: "ok", msg: t("access_banned", { player }) });
      setValue("");
      setTouched(false);
      setArmed(false);
      reload();
    } catch (e) {
      setFb({ kind: "err", msg: humanizeError(e) });
    } finally {
      setBanning(false);
    }
  }, [name, player, valid, banning, reload, t]);

  const pardon = useCallback(
    async (p: string) => {
      setFb(null);
      setPardoning(p);
      try {
        await api.accessBan(name, "pardon", p);
        setFb({ kind: "ok", msg: t("access_pardoned", { player: p }) });
        reload();
      } catch (e) {
        setFb({ kind: "err", msg: humanizeError(e) });
      } finally {
        setPardoning(null);
        setConfirming(null);
      }
    },
    [name, reload, t],
  );

  return (
    <CollapsibleSection
      icon={<Ban className="h-4 w-4" />}
      title={t("access_ban_title")}
      count={!loading && !error ? players.length : undefined}
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
        <p className="text-sm text-muted-foreground">{t("access_ban_desc")}</p>

        {/* Ban-by-name — the one action whose target isn't already on screen, so it
            is the most guarded: the button arms a full-sentence confirm rather than
            firing, and Enter arms it too (PlayerField.onEnter={arm}). */}
        <div className="space-y-1.5">
          <div className="flex items-start gap-2">
            <div className="flex-1">
              <PlayerField
                value={value}
                onChange={(v) => {
                  setValue(v);
                  setArmed(false); // editing the name re-disarms; confirm what you see
                }}
                onEnter={arm}
                invalid={invalid}
                disabled={banning}
              />
            </div>
            <Button
              size="sm"
              variant="destructive"
              className="h-9"
              onClick={arm}
              disabled={!valid || armed || banning}
            >
              <Ban className="h-4 w-4" />
              {t("access_ban_btn")}
            </Button>
          </div>
          {invalid && <p className="text-xs text-destructive">{t("access_player_invalid")}</p>}

          {armed && valid && (
            <div className="flex flex-wrap items-center gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2">
              <p className="min-w-0 flex-1 text-xs text-foreground">
                {t("access_ban_confirm", { player })}
              </p>
              <Button
                variant="ghost"
                size="sm"
                className="h-7 px-2"
                onClick={() => setArmed(false)}
                disabled={banning}
              >
                {t("access_cancel")}
              </Button>
              <Button
                variant="destructive"
                size="sm"
                className="h-7 px-2"
                onClick={ban}
                disabled={banning}
              >
                {banning ? (
                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                ) : (
                  t("access_ban_confirm_yes")
                )}
              </Button>
            </div>
          )}
        </div>

        {loading ? (
          <div className="flex items-center gap-2 py-2 text-xs text-muted-foreground">
            <Loader2 className="h-3.5 w-3.5 animate-spin" /> {t("log_connecting")}
          </div>
        ) : error ? (
          <p className="text-xs text-destructive">{t("access_ban_load_error")}</p>
        ) : players.length === 0 ? (
          <div className="rounded-md border border-dashed border-border bg-muted/20 px-4 py-8 text-center">
            <p className="text-sm text-muted-foreground">{t("access_ban_empty")}</p>
            <p className="mt-1 text-xs text-muted-foreground/80">{t("access_ban_empty_hint")}</p>
          </div>
        ) : (
          <div className="space-y-2">
            {showSearch && <SearchBox value={query} onChange={onQuery} />}

            <ul className="divide-y divide-border rounded-md border border-border">
              {shown.length === 0 ? (
                <li className="px-3 py-6 text-center text-xs text-muted-foreground">
                  {t("access_search_no_match", { query: query.trim() })}
                </li>
              ) : (
                pageItems.map((p) => (
                  <li
                    key={p}
                    className="flex items-center justify-between gap-2 px-3 py-2 transition-colors hover:bg-muted/40"
                  >
                    <span className="flex min-w-0 items-center gap-2">
                      <Ban className="h-3.5 w-3.5 shrink-0 text-destructive/70" />
                      <span className="truncate font-mono text-sm">{p}</span>
                    </span>
                    {/* Pardon lets a player back in, so it asks once: one tap arms the
                        row (取消 / 解封), a second confirms. Lighter than the ban-by-name
                        confirm because a mistaken pardon is re-bannable in one tap. */}
                    {confirming === p ? (
                      <div className="flex shrink-0 items-center gap-1">
                        <span className="mr-1 hidden text-xs text-muted-foreground sm:inline">
                          {t("access_pardon_q")}
                        </span>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-6 px-2"
                          onClick={() => setConfirming(null)}
                          disabled={pardoning === p}
                        >
                          {t("access_cancel")}
                        </Button>
                        <Button
                          size="sm"
                          className="h-6 px-2"
                          onClick={() => pardon(p)}
                          disabled={pardoning !== null}
                        >
                          {pardoning === p ? (
                            <Loader2 className="h-3.5 w-3.5 animate-spin" />
                          ) : (
                            t("access_pardon_btn")
                          )}
                        </Button>
                      </div>
                    ) : (
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7 shrink-0 px-2"
                        onClick={() => setConfirming(p)}
                        disabled={pardoning !== null}
                      >
                        <Undo2 className="h-3.5 w-3.5" />
                        <span className="hidden sm:inline">{t("access_pardon_btn")}</span>
                      </Button>
                    )}
                  </li>
                ))
              )}
            </ul>

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

        {/* Verbatim server reply — the escape hatch when the vanilla-only parse can't
            tokenize a plugin or localized "banlist". Collapsed by default. */}
        {!loading && !error && raw && (
          <details className="text-xs">
            <summary className="cursor-pointer select-none text-muted-foreground transition-colors hover:text-foreground">
              {t("access_ban_raw")}
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
