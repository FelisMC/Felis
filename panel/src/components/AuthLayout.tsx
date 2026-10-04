import { type ReactNode, useEffect } from "react";
import { useTranslation } from "react-i18next";
import { ConfigBanner } from "@/components/RuntimeStatus";
import { FelisLogo } from "@/components/FelisLogo";

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
  const { t, i18n } = useTranslation("common");
  const brand = t("brand_name");

  useEffect(() => {
    document.documentElement.lang = i18n.language;
    document.title = title === brand ? brand : `${title} · ${brand}`;
  }, [brand, i18n.language, title]);
  return (
    <div className="flex min-h-screen flex-col items-center justify-center bg-background px-4 py-12">
      <div className="w-full max-w-md space-y-8">
        <ConfigBanner className="rounded-md border" />
        <div className="flex flex-col items-center gap-3 text-center">
          <div className="flex items-center justify-center gap-3">
            <FelisLogo size={44} />
            <h1 className="text-3xl font-semibold tracking-tight sm:text-4xl">{title}</h1>
          </div>
          {subtitle && <p className="text-sm text-muted-foreground">{subtitle}</p>}
        </div>
        {children}
      </div>
    </div>
  );
}
