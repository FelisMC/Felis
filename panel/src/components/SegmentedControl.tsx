import type { KeyboardEvent } from "react";
import { cn } from "@/lib/utils";

export interface Segment<K extends string> {
  key: K;
  label: string;
  count?: number;
}

export function SegmentedControl<K extends string>({ segments, value, onChange, label, className }: {
  segments: readonly Segment<K>[];
  value: K;
  onChange: (key: K) => void;
  label: string;
  className?: string;
}) {
  function move(event: KeyboardEvent<HTMLDivElement>) {
    const current = segments.findIndex((segment) => segment.key === value);
    const next = event.key === "Home" ? 0
      : event.key === "End" ? segments.length - 1
      : event.key === "ArrowLeft" || event.key === "ArrowUp" ? (current - 1 + segments.length) % segments.length
      : event.key === "ArrowRight" || event.key === "ArrowDown" ? (current + 1) % segments.length
      : -1;
    if (next < 0) return;
    event.preventDefault();
    event.currentTarget.querySelectorAll<HTMLElement>('[role="radio"]')[next]?.focus();
    onChange(segments[next].key);
  }

  return (
    <div role="radiogroup" aria-label={label} onKeyDown={move}
      className={cn(
        "inline-flex h-10 max-w-full shrink-0 items-center gap-0.5 self-start overflow-x-auto rounded-lg border border-border bg-muted/50 p-1",
        "[scrollbar-width:none] [&::-webkit-scrollbar]:hidden",
        className,
      )}>
      {segments.map(({ key, label: text, count }) => {
        const checked = key === value;
        return (
          <button key={key} type="button" role="radio" aria-checked={checked} tabIndex={checked ? 0 : -1}
            aria-label={count === undefined ? undefined : `${text} (${count})`}
            onClick={() => { if (!checked) onChange(key); }}
            className={cn(
              "inline-flex h-full shrink-0 items-center gap-1.5 whitespace-nowrap rounded-md px-3 text-sm font-medium transition-colors duration-150",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              checked ? "bg-card text-foreground shadow-sm ring-1 ring-border" : "text-muted-foreground hover:text-foreground",
            )}>
            {text}
            {count !== undefined && (
              <span className={cn(
                "min-w-[1.25rem] rounded-full px-1.5 text-center text-[11px] font-semibold leading-[1.125rem] tabular-nums",
                checked ? "bg-primary/10 text-primary" : "bg-muted text-muted-foreground",
              )}>{count}</span>
            )}
          </button>
        );
      })}
    </div>
  );
}
