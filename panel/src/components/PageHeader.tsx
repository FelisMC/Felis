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

export function PageHeader({ icon: Icon, title, subtitle, actions, tabs, back, className }: PageHeaderProps) {
  const host = React.useContext(PageHeaderHostContext);
  const compact = !!back;
  const row = (
    <div className={cn("flex w-full flex-wrap items-end justify-between gap-3", className, host && "my-0")}>
      <div className="flex min-w-0 max-w-full items-center gap-2.5">
        {Icon && (
          <div className="shrink-0 text-muted-foreground">
            {React.isValidElement(Icon)
              ? Icon
              : React.createElement(Icon as React.ComponentType<{ className?: string }>, {
                  className: compact ? "h-4 w-4" : "h-[17px] w-[17px]",
                })}
          </div>
        )}
        <div className={cn("min-w-0 text-left", compact && "flex flex-wrap items-baseline gap-x-3 gap-y-0.5")}>
          <h1 className="text-[17px] font-semibold tracking-tight text-foreground">{title}</h1>
          {subtitle && <div className={cn("text-xs text-muted-foreground", !compact && "mt-0.5")}>{subtitle}</div>}
        </div>
      </div>
      {actions && <div className="flex min-w-0 max-w-full shrink-0 flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
  const header = (
    <div className={cn("mx-auto w-full max-w-[1220px]", tabs ? "pb-0" : "border-b border-border/60 pb-3.5")}>
      {back && <div className="mb-2">{back}</div>}
      {row}
      {tabs && <div className="mt-3 border-b border-border/70">{tabs}</div>}
    </div>
  );
  return host ? createPortal(header, host) : header;
}
