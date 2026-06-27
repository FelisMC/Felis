import { useEffect, useState } from "react";
import { NavLink, Outlet } from "react-router-dom";
import { Cat, Globe, Sun, Moon, LogOut } from "lucide-react";
import { useTranslation } from "react-i18next";
import * as SelectPrimitive from "@radix-ui/react-select";
import { cn } from "@/lib/utils";
import { useTier } from "@/lib/tier";
import { visibleSections, type NavSection } from "@/lib/nav";
import { Select, SelectContent, SelectItem } from "@/components/ui/select";
import { useTheme } from "@/lib/theme";
import { api } from "@/lib/api";

function SectionGroup({ section, isFirst }: { section: NavSection; isFirst: boolean }) {
  const { t } = useTranslation("navigation");

  return (
    <div
      className={cn(
        "flex flex-col gap-0.5",
        // Sections after the first get a thin divider with breathing room above,
        // which reads as a structural break without needing a loud heading.
        !isFirst && "mt-3 border-t border-border/50 pt-3",
      )}
    >
      {section.titleKey && (
        <div className="mb-1 px-3 text-[11px] font-medium text-muted-foreground/70">
          {t(section.titleKey)}
        </div>
      )}
      {section.items.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end}
          className={({ isActive }) =>
            cn(
              "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
              isActive
                ? "bg-primary/15 text-primary"
                : "text-muted-foreground hover:bg-accent hover:text-foreground",
            )
          }
        >
          <item.icon className="h-4 w-4" />
          {t(item.key)}
        </NavLink>
      ))}
    </div>
  );
}

// Shared visual baseline for every clickable icon button in the sidebar foot.
// Styled with a subtle border and background to make them feel like tangible widgets,
// resolving the flat "floating icons in empty space" visual issue.
const FOOT_ICON_BTN =
  "flex h-7 w-7 shrink-0 items-center justify-center rounded-md border border-border/40 bg-muted/20 text-muted-foreground hover:bg-accent hover:text-foreground focus:outline-none transition-all duration-150 active:scale-95 disabled:opacity-50 disabled:cursor-not-allowed";

function UserStrip() {
  // The sidebar foot identifies the principal and exposes one action — sign out.
  // identity?.email is the only display-safe field (user_id is a UUID, role is
  // server-truth not display). While /me is loading or has failed we render a
  // muted placeholder rather than a broken row, so the strip never flashes empty.
  const { identity, refresh } = useTier();
  const { t } = useTranslation("account");
  const [signingOut, setSigningOut] = useState(false);

  // Mirrors Account.tsx#signOut: idempotent on the server; refresh() flips
  // unauthenticated → RequireAuth bounces to /login. No navigate() needed.
  async function signOut() {
    if (signingOut) return;
    setSigningOut(true);
    try {
      await api.logout();
    } finally {
      await refresh();
    }
  }

  return (
    <div className="flex items-center gap-2 rounded-lg border border-border/60 bg-muted/30 px-3 py-2 min-w-0 shadow-sm">
      <div className="min-w-0 flex-1 leading-tight">
        <div
          className="truncate text-[13px] font-semibold text-foreground"
          title={identity?.email ?? ""}
        >
          {identity?.email ?? <span className="text-muted-foreground/40">—</span>}
        </div>
        <div className="text-[10px] text-muted-foreground/70 capitalize font-medium">
          {identity?.role ?? "user"}
        </div>
      </div>
      <button
        type="button"
        onClick={signOut}
        disabled={signingOut || !identity}
        className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-background hover:text-destructive border border-transparent hover:border-border/50 focus:outline-none transition-all active:scale-95 disabled:opacity-50"
        aria-label={t("sign_out")}
        title={t("sign_out")}
      >
        <LogOut className="h-3.5 w-3.5" />
      </button>
    </div>
  );
}

function LangToggle() {
  const { i18n } = useTranslation();

  return (
    <Select value={i18n.language} onValueChange={(v) => i18n.changeLanguage(v)}>
      <SelectPrimitive.Trigger className={FOOT_ICON_BTN}>
        <Globe className="h-4 w-4" />
      </SelectPrimitive.Trigger>
      <SelectContent align="start" className="min-w-[6rem]">
        <SelectItem value="en-US">English</SelectItem>
        <SelectItem value="zh-CN">中文</SelectItem>
      </SelectContent>
    </Select>
  );
}

function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  const { t } = useTranslation("common");

  return (
    <button
      onClick={toggleTheme}
      className={FOOT_ICON_BTN}
      aria-label={t("toggle_theme")}
      title={t("toggle_theme")}
    >
      {theme === "light" && <Sun className="h-4 w-4" />}
      {theme === "dark" && <Moon className="h-4 w-4" />}
    </button>
  );
}

export function AppShell() {
  const { isAdmin } = useTier();
  const { t, i18n } = useTranslation("navigation");
  // Sections are derived purely from is_admin: User-Side always, Admin/SysAdmin
  // only for admins. isAdmin is fail-closed (false while /me loads or on failure),
  // so admin sections appear only once identity is confirmed.
  const sections = visibleSections(isAdmin);

  // Sync document metadata with the active language.
  useEffect(() => {
    document.documentElement.lang = i18n.language;
    document.title = t("common:page_title");
  }, [i18n.language, t]);

  return (
    <div className="flex min-h-screen">
      <aside className="hidden w-60 shrink-0 flex-col border-r border-border bg-card/40 p-4 md:flex">
        <div className="mb-6 flex items-center gap-2 px-2">
          <Cat className="h-6 w-6 text-primary" />
          <span className="text-lg font-semibold tracking-tight">{t("common:brand_name")}</span>
        </div>

        <nav className="flex flex-col">
          {sections.map((section, i) => (
            <SectionGroup key={section.id} section={section} isFirst={i === 0} />
          ))}
        </nav>

        <div className="mt-auto flex flex-col gap-2 border-t border-border/50 pt-2.5">
          {/* 1. Toggles & Meta */}
          <div className="flex items-center justify-between px-1">
            <div className="flex items-center gap-1.5">
              <LangToggle />
              <ThemeToggle />
            </div>
            <span className="text-[10px] font-mono text-muted-foreground/40 select-none">
              Felis v0.1.0
            </span>
          </div>

          {/* 2. Profile Card */}
          <UserStrip />

          {/* 3. Branding Sign-off (sits tight at the absolute bottom with leading-tight and centered) */}
          <div className="px-1 text-[10px] text-muted-foreground/50 leading-tight text-center">
            {t("common:brand_tagline")}
          </div>
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 items-center justify-between border-b border-border px-4 md:hidden">
          <div className="flex items-center gap-2">
            <Cat className="h-5 w-5 text-primary" />
            <span className="font-semibold">{t("common:brand_name")}</span>
          </div>
          <div className="flex items-center gap-2">
            <LangToggle />
            <ThemeToggle />
          </div>
        </header>
        <main className="flex flex-1 flex-col p-6">
          <div className="mx-auto flex w-full max-w-8xl flex-1 flex-col gap-6">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
