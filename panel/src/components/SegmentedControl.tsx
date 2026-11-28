import { useRef, type KeyboardEvent } from "react";
import { cn } from "@/lib/utils";
import { useSlidingIndicator } from "@/lib/motion";

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
  const groupRef = useRef<HTMLDivElement>(null);
  const indicator = useSlidingIndicator(groupRef, '[aria-checked="true"]', `${value}:${segments.length}`, "x");

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
    <div
      ref={groupRef}
      role="radiogroup"
      aria-label={label}
      onKeyDown={move}
      className={cn(
        "relative inline-flex h-8 max-w-full shrink-0 items-center gap-0.5 self-start overflow-x-auto rounded-lg border border-border/80 bg-muted/50 p-0.5",
        "[scrollbar-width:none] [&::-webkit-scrollbar]:hidden",
        className,
      )}
    >
      {indicator.ready && (
        <span
          aria-hidden="true"
          className={cn(
            "pointer-events-none absolute inset-y-0.5 left-0 rounded-md bg-card shadow-sm ring-1 ring-border/70 motion-reduce:transition-none",
            indicator.animate && "transition-glide-x",
          )}
          style={{
            transform: `translateX(${indicator.start}px)`,
            width: `${indicator.size}px`,
          }}
        />
      )}
      {segments.map(({ key, label: text, count }) => {
        const checked = key === value;
        return (
          <button
            key={key}
            type="button"
            role="radio"
            aria-checked={checked}
            tabIndex={checked ? 0 : -1}
            aria-label={count === undefined ? undefined : `${text} (${count})`}
            onClick={() => { if (!checked) onChange(key); }}
            className={cn(
              "relative z-10 inline-flex h-full shrink-0 items-center gap-1.5 whitespace-nowrap rounded-md px-2.5 text-xs font-medium transition-[color,transform] duration-150 ease-out active:scale-[0.98]",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40",
              checked
                ? cn("text-foreground", !indicator.ready && "bg-card shadow-sm ring-1 ring-border/70")
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {text}
            {count !== undefined && (
              <span
                className={cn(
                  "font-mono text-[11px] tabular-nums transition-colors duration-150",
                  checked ? "text-foreground/75" : "text-muted-foreground/70",
                )}
              >
                {count}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
