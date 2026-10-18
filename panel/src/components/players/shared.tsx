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
import { humanizeError } from "@/lib/api";
import { cn } from "@/lib/utils";

// Mirror the backend's player-name charset for feedback; the backend still validates it.
export const MC_NAME = /^[A-Za-z0-9_]{1,16}$/;

export type Feedback = { kind: "ok" | "err"; msg: string } | null;

// Prefer raw RCON replies, which may contain refusals; use localized text only for empty replies.
export function rconReply(output: string | undefined, fallback: string): string {
  return (output ?? "").trim() || fallback;
}

export function FeedbackLine({ fb }: { fb: Feedback }) {
  if (!fb) return null;
  return (
    <p
      role={fb.kind === "ok" ? "status" : "alert"}
      className={fb.kind === "ok" ? "text-xs text-emerald-500" : "text-xs text-destructive"}
    >
      {fb.msg}
    </p>
  );
}

export function LoadError({ message, error }: { message: string; error: unknown }) {
  return (
    <p role="alert" className="text-xs text-destructive">
      {message} {humanizeError(error)}
    </p>
  );
}

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

// The count and refresh stay visible while collapsed, so the header works as an at-a-glance index; actions render in both states and only the body is hidden.
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

// Clamp pages after removals and reset to page one when the query changes.
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
        className="pl-8 text-sm"
      />
    </div>
  );
}

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
