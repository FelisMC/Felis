import type { ReactNode } from "react";

/** TabHeader opens a server tab with what the page is for and the page's own actions. */
export function TabHeader({ description, actions }: { description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <p className="min-w-0 text-sm text-muted-foreground">{description}</p>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
