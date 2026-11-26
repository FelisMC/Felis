import { useEffect, useRef, type KeyboardEvent } from "react";
import { NavLink } from "react-router-dom";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { useScrollToTop } from "@/components/MainScroll";

export interface PageTab<K extends string> {
  key: K;
  label: string;
  icon?: LucideIcon;
}

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
    <div ref={list} role="tablist" aria-label={label} onKeyDown={move} className={ROW}>
      {tabs.map(({ key, label: text, icon }) => {
        const selected = key === value;
        return (
          <button
            key={key}
            type="button"
            role="tab"
            id={`${idPrefix}-tab-${key}`}
            aria-controls={selected ? `${idPrefix}-panel-${key}` : undefined}
            aria-selected={selected}
            tabIndex={selected ? 0 : -1}
            onClick={() => { if (!selected) onChange(key); }}
            className={tabClass(selected)}
          >
            <TabFace icon={icon} text={text} selected={selected} />
          </button>
        );
      })}
    </div>
  );
}

export interface LinkTab {
  to: string;
  label: string;
  icon?: LucideIcon;
  end?: boolean;
}

export function LinkTabs({ tabs, label }: { tabs: readonly LinkTab[]; label: string }) {
  return (
    <nav aria-label={label} className={ROW}>
      {tabs.map(({ to, label: text, icon, end }) => (
        <NavLink key={to} to={to} end={end} className={({ isActive }) => tabClass(isActive)}>
          {({ isActive }) => <TabFace icon={icon} text={text} selected={isActive} />}
        </NavLink>
      ))}
    </nav>
  );
}

const ROW = cn(
  "flex min-w-0 max-w-full gap-1 overflow-x-auto [scrollbar-width:none] [&::-webkit-scrollbar]:hidden",
  "scroll-px-4 px-1 md:scroll-px-0 md:px-0",
);

function tabClass(selected: boolean) {
  return cn(
    "group relative inline-flex shrink-0 whitespace-nowrap pb-2 pt-0.5 text-[13px] transition-colors duration-150 focus-visible:outline-none",
    selected ? "font-medium text-foreground" : "text-muted-foreground hover:text-foreground",
  );
}

function TabFace({ icon: Icon, text, selected }: { icon?: LucideIcon; text: string; selected: boolean }) {
  return (
    <>
      <span className="inline-flex items-center gap-1.5 rounded-md px-2 py-1 transition-colors duration-150 group-hover:bg-accent/60 group-focus-visible:ring-2 group-focus-visible:ring-ring/40">
        {Icon && <Icon className={cn("h-3.5 w-3.5 shrink-0 transition-colors", selected ? "text-foreground" : "text-muted-foreground group-hover:text-foreground")} aria-hidden="true" />}
        {text}
      </span>
      <span aria-hidden="true" className={cn("absolute inset-x-1.5 -bottom-px h-[1.5px] rounded-full transition-colors duration-150", selected ? "bg-primary" : "bg-transparent")} />
    </>
  );
}
