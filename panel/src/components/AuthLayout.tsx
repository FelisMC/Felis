import { type ReactNode, useEffect } from "react";
import { useTranslation } from "react-i18next";
import { ConfigBanner } from "@/components/RuntimeStatus";
import { BrandLogo } from "@/components/BrandLogo";
import { PoweredBy } from "@/components/PoweredBy";
import { useBranding } from "@/lib/branding";
import { cn } from "@/lib/utils";

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
            "w-full max-w-[400px] space-y-6",
            background && "rounded-xl border border-border/75 bg-background/90 p-6 shadow-xl backdrop-blur-md sm:p-7",
          )}
        >
          <ConfigBanner className="rounded-lg border" />
          <div className="flex flex-col items-center gap-2 text-center">
            <div className="flex items-center justify-center gap-2.5">
              <span className="inline-flex items-center justify-center overflow-hidden rounded-lg border border-border/80 bg-white p-0.5 shadow-sm">
                <BrandLogo size={34} />
              </span>
              <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
            </div>
            {subtitle && <p className="text-xs text-muted-foreground">{subtitle}</p>}
          </div>
          {notice && <p className="whitespace-pre-wrap break-words rounded-lg border border-border bg-card px-3.5 py-2.5 text-xs leading-relaxed text-muted-foreground">{notice}</p>}
          {children}
        </div>
      </div>
      <PoweredBy className="relative self-center rounded-full bg-background/75 px-3 py-1 text-[11px] backdrop-blur-sm" />
    </div>
  );
}
