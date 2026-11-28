import React from "react";
import { createPortal } from "react-dom";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

export const PageHeaderHostContext = React.createContext<HTMLElement | null>(null);

export interface PageHeaderProps {
  icon?: LucideIcon | React.ReactNode;
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  /** tabs is a PageTabs row drawn along the header's bottom edge. */
  tabs?: React.ReactNode;
  /** back is a link to the page one level up, set above the title. */
  back?: React.ReactNode;
  className?: string;
}

export function PageHeader({ title, subtitle, actions, tabs, back, className }: PageHeaderProps) {
  const host = React.useContext(PageHeaderHostContext);
  const row = (
    <div className={cn("flex w-full flex-wrap items-center justify-between gap-3", className, host && "my-0")}>
      <div className="min-w-0 text-left">
        <h1 className="text-[17px] font-semibold tracking-tight text-foreground">{title}</h1>
        {subtitle && <div className="mt-1 text-xs text-muted-foreground">{subtitle}</div>}
      </div>
      {actions && <div className="flex min-w-0 max-w-full shrink-0 flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
  const header = (
    <div className={cn("w-full", tabs ? "pb-0" : "border-b border-border/60 pb-3.5")}>
      {back && <div className="mb-2">{back}</div>}
      {row}
      {tabs && <div className="mt-3 border-b border-border/70">{tabs}</div>}
    </div>
  );
  return host ? createPortal(header, host) : header;
}
