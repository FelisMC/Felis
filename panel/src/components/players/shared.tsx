import {
  useId,
  useMemo,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { useTranslation } from "react-i18next";
import { ChevronLeft, ChevronRight, Search } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// MC_NAME mirrors the backend's mcNameRe (handlers_access.go): a Minecraft name is
// 1–16 chars of [A-Za-z0-9_]. Validating client-side gives instant feedback and
// matches exactly what the server accepts, so a well-formed name never round-trips
// just to learn the rule. The server re-validates regardless — this is UX, not
// trust (the structured field is the whole reason there is no injection surface).
export const MC_NAME = /^[A-Za-z0-9_]{1,16}$/;

export type Feedback = { kind: "ok" | "err"; msg: string } | null;

/** rconReply decides what a mutation reports: the server's own reply, verbatim,
 *  whenever it says anything — the reply is ground truth and names refusals too
 *  ("That player does not exist"), so a canned confirmation must never speak over
 *  it. The localized fallback only covers a silent server. */
export function rconReply(output: string | undefined, fallback: string): string {
  return (output ?? "").trim() || fallback;
}

/** FeedbackLine is the shared inline result line for a player action: emerald on
 *  success, destructive on failure. There is no toast library — every section
 *  reports here, in place, right under the control that fired. */
export function FeedbackLine({ fb }: { fb: Feedback }) {
  if (!fb) return null;
  return (
    <p
      role="status"
      className={fb.kind === "ok" ? "text-xs text-emerald-500" : "text-xs text-destructive"}
    >
      {fb.msg}
    </p>
  );
}

/** PlayerField is the shared player-name input: a controlled text box that enforces
 *  the access charset live (showing the rule only once the user has typed something
 *  wrong) and fires onEnter so the keyboard-only path works in every section. */
export function PlayerField({
  value,
  onChange,
  onEnter,
  invalid,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  onEnter: () => void;
  invalid: boolean;
  disabled?: boolean;
}) {
  const { t } = useTranslation("servers");
  return (
    <Input
      value={value}
      onChange={(e) => onChange(e.target.value)}
      onKeyDown={(e: KeyboardEvent<HTMLInputElement>) => {
        if (e.key === "Enter") {
          e.preventDefault();
          onEnter();
        }
      }}
      placeholder={t("access_player_placeholder")}
      autoComplete="off"
      autoCapitalize="none"
      spellCheck={false}
      maxLength={16}
      disabled={disabled}
      aria-invalid={invalid}
      className={invalid ? "border-destructive focus-visible:ring-destructive" : undefined}
    />
  );
}

/** CollapsibleSection is the shared shell for every player-management block. The
 *  page stacks several rosters (online, whitelist, bans); at a few hundred names
 *  each, showing them all expanded buries the one an admin actually wants. So each
 *  block collapses to a single index row — chevron, title, live count, and its
 *  refresh — and expands on click. The count and refresh stay visible while
 *  collapsed so the header doubles as an at-a-glance, refreshable index; `actions`
 *  therefore renders in both states, and only the body (`children`) is hidden. */
export function CollapsibleSection({
  icon,
  title,
  count,
  actions,
  defaultOpen = false,
  children,
}: {
  icon: ReactNode;
  title: string;
  /** Shown as a muted badge beside the title; omit while loading so no stale/empty
   *  badge flashes. This is the number the collapsed index is worth reading. */
  count?: ReactNode;
  /** Header controls kept visible in both states (e.g. refresh) — a sibling of the
   *  toggle, so clicking them never folds the section. */
  actions?: ReactNode;
  defaultOpen?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const contentId = useId();
  return (
    <Card>
      <div className="flex items-center gap-2 p-5">
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          aria-expanded={open}
          aria-controls={contentId}
          className="group flex min-w-0 flex-1 items-center gap-2 text-left"
        >
          <ChevronRight
            className={cn(
              "h-4 w-4 shrink-0 text-muted-foreground transition-transform duration-200 ease-out group-hover:text-foreground motion-reduce:transition-none",
              open && "rotate-90",
            )}
          />
          {icon}
          <span className="truncate text-base font-semibold tracking-tight">{title}</span>
          {count != null && (
            <Badge variant="muted" className="font-normal tabular-nums">
              {count}
            </Badge>
          )}
        </button>
        {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
      </div>
      {/* Expand / collapse animates the body's real height. The trick is the
          grid-rows 0fr↔1fr transition: it is the one pure-CSS way to ease to an
          UNKNOWN auto height (no JS measuring, no guessed max-height that would make
          the easing feel wrong). Three layers, each with one job:
            1. the grid — animates the track height 0fr↔1fr;
            2. overflow-hidden + min-h-0 — clips the body while it rolls up (min-h-0
               defeats a grid item's automatic minimum size so it truly reaches 0);
               `visibility` rides the SAME duration so, by the CSS visibility-
               transition rule, the body stays visible until the roll-up finishes and
               only THEN leaves the a11y tree — collapsed content is neither tabbable
               nor read by a screen reader, yet still animates;
            3. the padded body — fades opacity in step so it doesn't pop.
          motion-reduce collapses all of it to an instant toggle. */}
      <div
        className={cn(
          "grid transition-[grid-template-rows] duration-200 ease-out motion-reduce:transition-none",
          open ? "grid-rows-[1fr]" : "grid-rows-[0fr]",
        )}
      >
        <div
          className={cn(
            "min-h-0 overflow-hidden transition-[visibility] duration-200 motion-reduce:transition-none",
            open ? "visible" : "invisible",
          )}
        >
          <div
            id={contentId}
            className={cn(
              "p-5 pt-0 transition-opacity duration-200 ease-out motion-reduce:transition-none",
              open ? "opacity-100" : "opacity-0",
            )}
          >
            {children}
          </div>
        </div>
      </div>
    </Card>
  );
}

// Defaults shared by every roster list: page over 10 names at a time, and only
// show the filter once the list is long enough that the eye can't just scan it.
const PAGE_SIZE = 10;
const SEARCH_THRESHOLD = 8;

/** usePagedNames is the shared list engine for every player roster (whitelist,
 *  online, bans): a client-side filter plus paging over the filtered result. A
 *  whitelist or a full server can run to hundreds of names, and paging a screenful
 *  at a time — after search narrows — beats a cramped scroll box. clampedPage keeps
 *  a stale-high page valid after a removal shrinks the list, so the view never
 *  lands on an empty page. Changing the query resets to the first page. */
export function usePagedNames(names: string[]) {
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(0);

  const q = query.trim().toLowerCase();
  const shown = useMemo(
    () => (q ? names.filter((n) => n.toLowerCase().includes(q)) : names),
    [names, q],
  );
  const showSearch = names.length > SEARCH_THRESHOLD;
  const pageCount = Math.max(1, Math.ceil(shown.length / PAGE_SIZE));
  const clampedPage = Math.min(page, pageCount - 1);
  const pageItems = shown.slice(clampedPage * PAGE_SIZE, clampedPage * PAGE_SIZE + PAGE_SIZE);
  const needFooter = shown.length > PAGE_SIZE || (q !== "" && shown.length > 0);

  const onQuery = (v: string) => {
    setQuery(v);
    setPage(0);
  };

  return {
    query,
    onQuery,
    q,
    shown,
    showSearch,
    pageItems,
    pageCount,
    clampedPage,
    needFooter,
    setPage,
  };
}

/** SearchBox is the shared roster filter input — a search-icon-prefixed field. */
export function SearchBox({
  value,
  onChange,
}: {
  value: string;
  onChange: (v: string) => void;
}) {
  const { t } = useTranslation("servers");
  return (
    <div className="relative">
      <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
      <Input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={t("access_search_placeholder")}
        autoComplete="off"
        spellCheck={false}
        className="h-8 pl-8 text-sm"
      />
    </div>
  );
}

/** PagerFooter is the shared roster footer: a live count on the left (total, or
 *  filtered-of-total while searching) and prev / page-of / next controls on the
 *  right, shown only when there is more than one page. */
export function PagerFooter({
  q,
  shownCount,
  total,
  pageCount,
  clampedPage,
  onPage,
}: {
  q: string;
  shownCount: number;
  total: number;
  pageCount: number;
  clampedPage: number;
  onPage: (page: number) => void;
}) {
  const { t } = useTranslation("servers");
  return (
    <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
      <span className="tabular-nums">
        {q
          ? t("access_search_count", { shown: shownCount, total })
          : t("access_total_count", { total })}
      </span>
      {pageCount > 1 && (
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2"
            onClick={() => onPage(clampedPage - 1)}
            disabled={clampedPage === 0}
            aria-label={t("access_page_prev")}
          >
            <ChevronLeft className="h-4 w-4" />
            <span className="hidden sm:inline">{t("access_page_prev")}</span>
          </Button>
          <span className="min-w-[4.5rem] text-center tabular-nums">
            {t("access_page_indicator", { page: clampedPage + 1, pages: pageCount })}
          </span>
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2"
            onClick={() => onPage(clampedPage + 1)}
            disabled={clampedPage >= pageCount - 1}
            aria-label={t("access_page_next")}
          >
            <span className="hidden sm:inline">{t("access_page_next")}</span>
            <ChevronRight className="h-4 w-4" />
          </Button>
        </div>
      )}
    </div>
  );
}
