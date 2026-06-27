import { useEffect } from "react";
import { NavLink, Outlet } from "react-router-dom";
import { Cat, Globe, Sun, Moon } from "lucide-react";
import { useTranslation } from "react-i18next";
import * as SelectPrimitive from "@radix-ui/react-select";
import { cn } from "@/lib/utils";
import { useTier } from "@/lib/tier";
import { visibleSections, type NavSection } from "@/lib/nav";
import { Select, SelectContent, SelectItem } from "@/components/ui/select";
import { useTheme } from "@/lib/theme";

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

function LangToggle() {
  const { i18n } = useTranslation();

  return (
    <Select value={i18n.language} onValueChange={(v) => i18n.changeLanguage(v)}>
      <SelectPrimitive.Trigger className="h-7 w-7 p-0 flex items-center justify-center border border-border rounded-md bg-muted/40 hover:bg-accent text-muted-foreground hover:text-foreground focus:outline-none transition-colors shrink-0">
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
      className="h-7 w-7 p-0 flex items-center justify-center border border-border rounded-md bg-muted/40 hover:bg-accent text-muted-foreground hover:text-foreground focus:outline-none transition-all duration-200 active:scale-95 shrink-0"
      aria-label={t("toggle_theme")}
      title={t("toggle_theme")}
    >
      {theme === "light" ? (
        <Sun className="h-4 w-4" />
      ) : (
        <Moon className="h-4 w-4" />
      )}
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

        <div className="mt-auto px-3 pt-4 border-t border-border/60">
          <div className="flex items-start gap-2">
            <div className="flex items-center gap-1.5 shrink-0">
              <LangToggle />
              <ThemeToggle />
            </div>
            <span className="text-[10px] text-muted-foreground/60 leading-tight pt-1">
              {t("common:brand_tagline")}
            </span>
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
