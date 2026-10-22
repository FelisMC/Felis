import { useEffect, useRef } from "react";
import { createPortal } from "react-dom";
import { MoreHorizontal } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";

export interface FileAction { label: string; run: () => void; disabled?: boolean }

export function FileMenu({ name, open, onOpenChange, actions }: {
  name: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  actions: FileAction[];
}) {
  const { t } = useTranslation("files");
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (open) menu.current?.querySelector<HTMLButtonElement>("button:not(:disabled)")?.focus();
  }, [open]);
  useEffect(() => {
    if (!open) return;
    const outside = (e: PointerEvent) => {
      if (e.target instanceof Node && !menu.current?.contains(e.target) && !trigger.current?.contains(e.target)) onOpenChange(false);
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open, onOpenChange]);
  const anchor = trigger.current?.getBoundingClientRect();
  const top = Math.max(8, Math.min(anchor?.bottom ?? 8, window.innerHeight - 220));
  return <div className="relative" onBlur={(e) => {
    if (!e.currentTarget.contains(e.relatedTarget) && !menu.current?.contains(e.relatedTarget)) onOpenChange(false);
  }}>
    <Button ref={trigger} size="sm" variant="ghost" className="h-8 w-8 p-0" aria-label={t("more_item", { name })}
      aria-haspopup="menu" aria-expanded={open} onClick={() => onOpenChange(!open)}>
      <MoreHorizontal className="h-4 w-4" />
    </Button>
    {open && createPortal(<div ref={menu} role="menu" aria-label={t("more_item", { name })}
      style={{ right: Math.max(8, window.innerWidth - (anchor?.right ?? window.innerWidth - 8)), top, maxHeight: window.innerHeight - top - 8 }}
      className="fixed z-50 grid min-w-44 overflow-y-auto rounded-md border border-border bg-background p-1 shadow-lg"
      onBlur={(e) => { if (!e.currentTarget.contains(e.relatedTarget) && e.relatedTarget !== trigger.current) onOpenChange(false); }}
      onKeyDown={(e) => {
        if (e.key === "Escape") { e.stopPropagation(); onOpenChange(false); trigger.current?.focus(); }
        if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(e.key)) return;
        e.preventDefault();
        const buttons = [...e.currentTarget.querySelectorAll<HTMLButtonElement>("button:not(:disabled)")];
        const index = buttons.indexOf(document.activeElement as HTMLButtonElement);
        const next = e.key === "Home" ? 0 : e.key === "End" ? buttons.length - 1 : (index + (e.key === "ArrowDown" ? 1 : -1) + buttons.length) % buttons.length;
        buttons[next]?.focus();
      }}>
      {actions.map((action) => <button key={action.label} role="menuitem" type="button" disabled={action.disabled}
        className="rounded px-3 py-2 text-left text-xs hover:bg-muted focus:bg-muted focus:outline-none disabled:opacity-40"
        onClick={() => { onOpenChange(false); action.run(); }}>{action.label}</button>)}
    </div>, document.body)}
  </div>;
}
