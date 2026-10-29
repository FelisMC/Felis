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
  // A header with tabs stays pinned over its sections, so it keeps to one line with the subtitle beside the title.
  const compact = !!(tabs || back);
  const row = (
    <div className={cn("mx-auto flex w-full max-w-8xl flex-wrap items-center justify-between gap-3", className, host && "my-0")}>
      <div className="flex min-w-0 max-w-full items-center gap-3">
        {Icon && (
          <div className="shrink-0 text-primary">
            {React.isValidElement(Icon)
              ? Icon
              : React.createElement(Icon as React.ComponentType<{ className?: string }>, {
                  className: compact ? "h-5 w-5" : "h-6 w-6",
                })}
          </div>
        )}
        <div className={cn("min-w-0 text-left", compact && "flex flex-wrap items-baseline gap-x-4 gap-y-0.5")}>
          <h1 className={cn("font-semibold tracking-tight text-foreground", compact ? "text-xl" : "text-2xl")}>{title}</h1>
          {subtitle && <div className={cn("text-sm text-muted-foreground", !compact && "mt-0.5")}>{subtitle}</div>}
        </div>
      </div>
      {actions && <div className="flex min-w-0 max-w-full shrink-0 flex-wrap items-center gap-3">{actions}</div>}
    </div>
  );
  // In the header strip the tabs sit on its bottom border: the negative margin takes back the
  // strip's padding, so the chosen tab's underline lands on the line itself.
  const header = tabs || back ? (
    <div className="mx-auto w-full max-w-8xl">
      {back && <div className="mb-1.5">{back}</div>}
      {row}
      {tabs && <div className={cn("mt-3", host ? "-mx-4 -mb-4 md:mx-0" : "border-b border-border")}>{tabs}</div>}
    </div>
  ) : row;
  return host ? createPortal(header, host) : header;
}
