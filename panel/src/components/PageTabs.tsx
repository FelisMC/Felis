import { useEffect, useRef, type KeyboardEvent } from "react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { useScrollToTop } from "@/components/MainScroll";

export interface PageTab<K extends string> {
  key: K;
  label: string;
  icon?: LucideIcon;
}

/** PageTabs switches between the sections of one page, drawn as an underlined row along the
 *  bottom edge of the page header (PageHeader's `tabs`). It follows the WAI-ARIA tabs pattern:
 *  the chosen tab is the one in the tab order, the arrow keys, Home and End move between tabs
 *  and choose as they go, and the chosen tab controls the panel `${idPrefix}-panel-${key}` (a
 *  page may mount only the chosen one). On a narrow screen the row scrolls sideways and keeps
 *  the chosen tab in view. */
export function PageTabs<K extends string>({ tabs, value, onChange, label, idPrefix }: {
  tabs: readonly PageTab<K>[];
  value: K;
  onChange: (key: K) => void;
  label: string;
  idPrefix: string;
}) {
  const list = useRef<HTMLDivElement>(null);
  const toTop = useScrollToTop();
  const shown = useRef(value);
  useEffect(() => {
    list.current?.querySelector<HTMLElement>('[aria-selected="true"]')?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
    // Another section starts at its top, once it is the one shown: a switch the page holds back
    // (unsaved changes to confirm first) leaves the scroll position alone.
    if (shown.current === value) return;
    shown.current = value;
    toTop();
  }, [value, toTop]);

  function move(event: KeyboardEvent<HTMLDivElement>) {
    const current = tabs.findIndex((tab) => tab.key === value);
    const next = event.key === "Home" ? 0
      : event.key === "End" ? tabs.length - 1
      : event.key === "ArrowLeft" ? (current - 1 + tabs.length) % tabs.length
      : event.key === "ArrowRight" ? (current + 1) % tabs.length
      : -1;
    if (next < 0) return;
    event.preventDefault();
    event.currentTarget.querySelectorAll<HTMLElement>('[role="tab"]')[next]?.focus();
    onChange(tabs[next].key);
  }

  return (
    <div ref={list} role="tablist" aria-label={label} onKeyDown={move}
      className={cn(
        "flex min-w-0 max-w-full gap-1 overflow-x-auto [scrollbar-width:none] [&::-webkit-scrollbar]:hidden",
        // Below md the row runs to the screen edges and fades there, so a tab cut off reads as more to scroll to.
        "scroll-px-4 px-4 [-webkit-mask-image:linear-gradient(to_right,transparent,black_1rem,black_calc(100%-1rem),transparent)] [mask-image:linear-gradient(to_right,transparent,black_1rem,black_calc(100%-1rem),transparent)]",
        "md:scroll-px-0 md:px-0 md:[-webkit-mask-image:none] md:[mask-image:none]",
      )}>
      {tabs.map(({ key, label: text, icon: Icon }) => {
        const selected = key === value;
        return (
          <button key={key} type="button" role="tab" id={`${idPrefix}-tab-${key}`} aria-controls={selected ? `${idPrefix}-panel-${key}` : undefined}
            aria-selected={selected} tabIndex={selected ? 0 : -1} onClick={() => { if (!selected) onChange(key); }}
            className={cn(
              "group relative inline-flex shrink-0 whitespace-nowrap px-1.5 pb-2 pt-1 text-sm font-medium transition-colors duration-150 focus-visible:outline-none",
              selected ? "text-foreground" : "text-muted-foreground hover:text-foreground",
            )}>
            {/* The hover tint and the focus ring go round the label, clear of the underline. */}
            <span className="inline-flex items-center gap-2 rounded-md px-1.5 py-1 transition-colors duration-150 group-hover:bg-muted/70 group-focus-visible:ring-2 group-focus-visible:ring-ring">
              {Icon && <Icon className={cn("h-4 w-4 shrink-0 transition-colors", selected ? "text-primary" : "text-muted-foreground group-hover:text-foreground")} aria-hidden="true" />}
              {text}
            </span>
            <span aria-hidden="true" className={cn("absolute inset-x-1.5 bottom-0 h-0.5 rounded-full transition-colors duration-150", selected ? "bg-primary" : "bg-transparent")} />
          </button>
        );
      })}
    </div>
  );
}
