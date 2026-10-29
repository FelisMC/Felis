import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/** TabHeader opens a server tab with what the page is for and the page's own actions; a phone shows only the actions. */
export function TabHeader({ description, actions }: { description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className={cn("flex-wrap items-center justify-between gap-3", actions ? "flex" : "hidden sm:flex")}>
      <p className="hidden min-w-0 text-sm text-muted-foreground sm:block">{description}</p>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
