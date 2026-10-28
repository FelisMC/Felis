import { type ReactNode, useEffect } from "react";
import { useTranslation } from "react-i18next";
import { ConfigBanner } from "@/components/RuntimeStatus";
import { BrandLogo } from "@/components/BrandLogo";
import { PoweredBy } from "@/components/PoweredBy";
import { useBranding } from "@/lib/branding";
import { cn } from "@/lib/utils";

// AuthLayout is the chrome for the pre-app auth surfaces (login, forced change). They live outside AppShell, with no nav or tier context yet, so they get their own centered frame.
export function AuthLayout({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children: ReactNode;
}) {
  const { i18n } = useTranslation("common");
  const { name, notice, background } = useBranding();

  useEffect(() => {
    document.documentElement.lang = i18n.language;
    document.title = title === name ? name : `${title} · ${name}`;
  }, [name, i18n.language, title]);
  return (
    <div className="relative flex min-h-screen flex-col bg-background px-4 py-12">
      {background && <img src={background} alt="" aria-hidden="true" className="pointer-events-none fixed inset-0 h-full w-full object-cover" />}
      <div className="relative flex flex-1 items-center justify-center">
        <div
          className={cn(
            "w-full max-w-md space-y-8",
            background && "rounded-xl border border-border/60 bg-background/85 p-6 shadow-lg backdrop-blur-sm sm:p-8",
          )}
        >
          <ConfigBanner className="rounded-md border" />
          <div className="flex flex-col items-center gap-3 text-center">
            <div className="flex items-center justify-center gap-3">
              <BrandLogo size={44} />
              <h1 className="text-3xl font-semibold tracking-tight sm:text-4xl">{title}</h1>
            </div>
            {subtitle && <p className="text-sm text-muted-foreground">{subtitle}</p>}
          </div>
          {notice && <p className="whitespace-pre-wrap break-words rounded-md border border-border bg-muted/40 px-4 py-3 text-sm">{notice}</p>}
          {children}
        </div>
      </div>
      <PoweredBy className="relative self-center rounded-full bg-background/70 px-3 py-1 text-xs backdrop-blur-sm" />
    </div>
  );
}
