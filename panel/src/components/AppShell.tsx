import { Suspense, useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { Globe, Sun, Moon, LogOut, WifiOff, Menu, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import * as SelectPrimitive from "@radix-ui/react-select";
import * as DialogPrimitive from "@radix-ui/react-dialog";
import { cn } from "@/lib/utils";
import { useTier } from "@/lib/tier";
import { visibleSections, type NavSection } from "@/lib/nav";
import { Select, SelectContent, SelectItem } from "@/components/ui/select";
import { useTheme } from "@/lib/theme";
import { api, CONNECTION_EVENT, isConnectionLost } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { ConfigBanner, VersionBadge } from "@/components/RuntimeStatus";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { Loading } from "@/components/States";
import { ROLE_LABEL_KEY } from "@/components/RoleBadge";
import { FelisLogo } from "@/components/FelisLogo";

function SectionGroup({
  section,
  isFirst,
  onNavigate,
}: {
  section: NavSection;
  isFirst: boolean;
  onNavigate?: () => void;
}) {
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
          onClick={onNavigate}
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
  // It shows the email and the role by its name in the UI language (user_id is a
  // UUID). While /me is loading or has failed both lines hold a muted dash, so
  // the strip never flashes empty and never names a role it has not read.
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
        <div className="text-[10px] text-muted-foreground/70 font-medium">
          {identity ? t(`admin:${ROLE_LABEL_KEY[identity.role]}`) : <span className="text-muted-foreground/40">—</span>}
        </div>
      </div>
      <button
        type="button"
        onClick={signOut}
        disabled={signingOut || !identity}
        className="flex h-10 w-10 md:h-7 md:w-7 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-background hover:text-destructive border border-transparent hover:border-border/50 focus:outline-none transition-all active:scale-95 disabled:opacity-50"
        aria-label={t("sign_out")}
        title={t("sign_out")}
      >
        <LogOut className="h-3.5 w-3.5" />
      </button>
    </div>
  );
}

function LangToggle() {
  const { t, i18n } = useTranslation("common");

  return (
    <Select value={i18n.language} onValueChange={(v) => i18n.changeLanguage(v)}>
      <SelectPrimitive.Trigger
        className={FOOT_ICON_BTN}
        aria-label={t("change_language")}
        title={t("change_language")}
      >
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

// ConnectionBanner shows while API calls get no response at all (api.ts
// CONNECTION_EVENT): the network is down, or Cloudflare Access sent the call to
// its login page. It clears on the next call that gets through (the pages'
// polling makes one soon); an expired Access sign-in only passes on a full page
// load, hence the reload button.
function ConnectionBanner() {
  const { t } = useTranslation("common");
  const [lost, setLost] = useState(isConnectionLost);
  useEffect(() => {
    const onChange = (e: Event) => setLost(!(e as CustomEvent<{ ok: boolean }>).detail.ok);
    window.addEventListener(CONNECTION_EVENT, onChange);
    return () => window.removeEventListener(CONNECTION_EVENT, onChange);
  }, []);
  if (!lost) return null;
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center justify-between gap-2 border-b border-amber-500/30 bg-amber-500/10 px-4 py-2 text-sm text-amber-800 dark:text-amber-200"
    >
      <span className="flex min-w-0 items-center gap-2">
        <WifiOff className="h-4 w-4 shrink-0" />
        {t("connection_lost")}
      </span>
      <Button size="sm" variant="outline" onClick={() => window.location.reload()}>
        {t("reload_page")}
      </Button>
    </div>
  );
}

// MobileNav is the phone-width way into every page: the sidebar is md-only, so
// below md a menu button opens the same sections (and the same sign-out strip)
// as a drawer from the left. Following a link closes it; so does a route change
// from anywhere else (back button, a link inside the page).
function MobileNav({ sections }: { sections: NavSection[] }) {
  const { t } = useTranslation("common");
  const { pathname } = useLocation();
  const [open, setOpen] = useState(false);
  useEffect(() => setOpen(false), [pathname]);

  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
      <DialogPrimitive.Trigger
        className={FOOT_ICON_BTN}
        aria-label={t("open_menu")}
        title={t("open_menu")}
      >
        <Menu className="h-4 w-4" />
      </DialogPrimitive.Trigger>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/60 backdrop-blur-sm data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 md:hidden" />
        <DialogPrimitive.Content
          aria-describedby={undefined}
          className="fixed inset-y-0 left-0 z-50 flex w-72 max-w-[85vw] flex-col border-r border-border bg-card p-4 shadow-xl duration-200 data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:slide-out-to-left data-[state=open]:slide-in-from-left md:hidden"
        >
          <div className="mb-5 flex items-center justify-between px-2">
            <DialogPrimitive.Title className="flex items-center gap-2 text-lg font-semibold tracking-tight">
              <FelisLogo />
              {t("brand_name")}
            </DialogPrimitive.Title>
            <DialogPrimitive.Close className={FOOT_ICON_BTN} aria-label={t("close_sr")}>
              <X className="h-4 w-4" />
            </DialogPrimitive.Close>
          </div>
          <nav className="flex flex-col overflow-y-auto">
            {sections.map((section, i) => (
              <SectionGroup
                key={section.id}
                section={section}
                isFirst={i === 0}
                onNavigate={() => setOpen(false)}
              />
            ))}
          </nav>
          <div className="mt-auto border-t border-border/50 pt-3">
            {/* The strip's sign-out button grows to a thumb-sized target below md. */}
            <UserStrip />
            <VersionBadge className="mt-2 flex justify-center" />
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

export function AppShell() {
  const { isAdmin, isOwner } = useTier();
  const { pathname } = useLocation();
  const { t, i18n } = useTranslation("navigation");
  // Sections are derived purely from is_admin and is_owner: User-Side always,
  // Admin-Side only for admins, Owner-Side only for the platform owner. Both
  // flags are fail-closed (false while /me loads or on failure), so admin and
  // owner sections appear only once identity is confirmed.
  const sections = visibleSections(isAdmin, isOwner);

  // Sync document metadata with the active language.
  useEffect(() => {
    document.documentElement.lang = i18n.language;
    document.title = t("common:page_title");
  }, [i18n.language, t]);

  return (
    <div className="flex h-screen overflow-hidden">
      <aside className="hidden w-60 shrink-0 flex-col border-r border-border bg-card/40 p-4 md:flex">
        <div className="mb-6 flex items-center gap-2 px-2">
          <FelisLogo />
          <span className="text-lg font-semibold tracking-tight">{t("common:brand_name")}</span>
        </div>

        <nav className="flex flex-col">
          {sections.map((section, i) => (
            <SectionGroup key={section.id} section={section} isFirst={i === 0} />
          ))}
        </nav>

        <div className="mt-auto flex flex-col gap-2 border-t border-border/50 pt-2.5">
          {/* 1. Toggles & Meta */}
          <div className="flex items-center justify-between gap-2 px-1">
            <div className="flex items-center gap-1.5">
              <LangToggle />
              <ThemeToggle />
            </div>
            <VersionBadge />
          </div>

          {/* 2. Profile Card */}
          <UserStrip />

          {/* 3. Branding Sign-off (sits tight at the absolute bottom with leading-tight and centered) */}
          <div className="px-1 text-[10px] text-muted-foreground/50 leading-tight text-center">
            {t("common:brand_tagline")}
          </div>
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col h-full overflow-y-auto">
        <ConnectionBanner />
        <ConfigBanner />
        <header className="flex h-14 shrink-0 items-center justify-between border-b border-border px-4 md:hidden">
          <div className="flex items-center gap-2">
            <MobileNav sections={sections} />
            <FelisLogo />
            <span className="font-semibold">{t("common:brand_name")}</span>
          </div>
          <div className="flex items-center gap-2">
            <LangToggle />
            <ThemeToggle />
          </div>
        </header>
        <main className="flex flex-1 flex-col p-4 md:p-6">
          <div className="mx-auto flex w-full max-w-8xl flex-1 flex-col gap-6">
            {/* A crash on one page leaves the navigation usable; moving to
                another route clears it. Pages load as their own chunks, so the
                first visit to one shows a spinner here with the shell in place,
                and a chunk gone after a deploy lands in the boundary. */}
            <ErrorBoundary resetKey={pathname}>
              <Suspense fallback={<Loading />}>
                <Outlet />
              </Suspense>
            </ErrorBoundary>
          </div>
        </main>
      </div>
    </div>
  );
}
