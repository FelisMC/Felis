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
  className?: string;
}

export function PageHeader({ icon: Icon, title, subtitle, actions, tabs, className }: PageHeaderProps) {
  const host = React.useContext(PageHeaderHostContext);
  const row = (
    <div className={cn("mx-auto flex w-full max-w-8xl flex-wrap items-center justify-between gap-3", className, host && "my-0")}>
      <div className="flex min-w-0 max-w-full items-center gap-3">
        {Icon && (
          <div className="shrink-0 text-primary">
            {React.isValidElement(Icon)
              ? Icon
              : React.createElement(Icon as React.ComponentType<{ className?: string }>, {
                  className: "h-6 w-6",
                })}
          </div>
        )}
        <div className="min-w-0 text-left">
          <h1 className="text-2xl font-semibold tracking-tight text-foreground">{title}</h1>
          {subtitle && <div className="text-sm text-muted-foreground mt-0.5">{subtitle}</div>}
        </div>
      </div>
      {actions && <div className="flex min-w-0 max-w-full shrink-0 flex-wrap items-center gap-3">{actions}</div>}
    </div>
  );
  // In the header strip the tabs sit on its bottom border: the negative margin takes back the
  // strip's padding, so the chosen tab's underline lands on the line itself.
  const header = tabs ? (
    <div className="mx-auto w-full max-w-8xl">
      {row}
      <div className={cn("mt-4 md:mt-5", host ? "-mx-4 -mb-4 md:mx-0 md:-mb-6" : "border-b border-border")}>{tabs}</div>
    </div>
  ) : row;
  return host ? createPortal(header, host) : header;
}
