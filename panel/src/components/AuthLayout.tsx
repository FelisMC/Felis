import type { ReactNode } from "react";
import { Cat } from "lucide-react";

// AuthLayout is the chrome for the pre-app auth surfaces (login, forced change).
// These live OUTSIDE AppShell — there is no nav, no tier context to honor yet —
// so they get their own centered, branded frame rather than the sidebar layout.
export function AuthLayout({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children: ReactNode;
}) {
  return (
    <div className="flex min-h-screen flex-col items-center justify-center bg-background px-4 py-12">
      <div className="w-full max-w-sm space-y-6">
        <div className="flex flex-col items-center gap-2 text-center">
          <Cat className="h-9 w-9 text-primary" />
          <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
          {subtitle && <p className="text-sm text-muted-foreground">{subtitle}</p>}
        </div>
        {children}
      </div>
    </div>
  );
}
