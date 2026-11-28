import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router-dom";
import {
  ChevronDown,
  Globe,
  LogOut,
  Menu,
  Moon,
  Monitor,
  Sun,
  WifiOff,
  X,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import * as SelectPrimitive from "@radix-ui/react-select";
import * as DialogPrimitive from "@radix-ui/react-dialog";
import { cn } from "@/lib/utils";
import { useTier } from "@/lib/tier";
import { useFeatures } from "@/lib/features";
import { useConfig } from "@/lib/hooks";
import { visibleSections, type NavSection } from "@/lib/nav";
import { Select, SelectContent, SelectItem } from "@/components/ui/select";
import { useTheme, type ThemePreference } from "@/lib/theme";
import { useBranding } from "@/lib/branding";
import { api, CONNECTION_EVENT, isConnectionLost } from "@/lib/api";
import { chooseLanguage } from "@/i18n/language";
import { Button } from "@/components/ui/button";
import { ConfigBanner, VersionBadge } from "@/components/RuntimeStatus";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { Loading } from "@/components/States";
import { ROLE_LABEL_KEY } from "@/components/RoleBadge";
import { BrandLogo } from "@/components/BrandLogo";
import { PoweredBy } from "@/components/PoweredBy";
import { PageHeaderHostContext } from "@/components/PageHeader";
import { MainScrollContext } from "@/components/MainScroll";
import { phaseColor } from "@/components/PhaseBadge";
import { ShellContext, type ActiveServerMeta } from "@/components/ShellContext";
import { useEnterAnimation, useSlidingIndicator } from "@/lib/motion";

const TOP_BTN =
  "inline-flex h-7 shrink-0 items-center justify-center gap-1.5 rounded-md border border-border bg-card px-2 text-xs font-medium text-foreground transition-[color,background-color,border-color,transform] duration-150 ease-out hover:border-muted-foreground/30 hover:bg-accent/70 active:scale-[0.97] active:duration-75 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:cursor-not-allowed disabled:opacity-45";

function routeMotionKey(pathname: string): string {
  const serverMatch = pathname.match(/^\/servers\/[^/]+/);
  return serverMatch ? serverMatch[0] : pathname;
}

function BrandMark({ size = 19 }: { size?: number }) {
  return (
    <span className="inline-flex h-[22px] w-[22px] shrink-0 items-center justify-center overflow-hidden rounded-[5px] border border-border bg-white">
      <BrandLogo size={size} />
    </span>
  );
}

function SectionGroup({
  section,
  isFirst,
  onNavigate,
  sliding = false,
}: {
  section: NavSection;
  isFirst: boolean;
  onNavigate?: () => void;
  sliding?: boolean;
}) {
  const { t } = useTranslation("navigation");

  return (
    <div className={cn("flex flex-col", !isFirst && "mt-3.5")}>
      {section.titleKey && (
        <div className="px-4 pb-1 text-[10.5px] text-muted-foreground/75">
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
              "group relative z-10 flex items-center gap-2.5 px-4 py-[6.5px] text-[12.5px] transition-[color,background-color,transform] duration-150 ease-out active:scale-[0.98]",
              isActive
                ? cn(
                    "font-medium text-foreground",
                    !sliding && "bg-accent before:absolute before:bottom-1 before:left-0 before:top-1 before:w-[2.5px] before:rounded-r-sm before:bg-primary",
                  )
                : "text-muted-foreground hover:bg-accent/55 hover:text-foreground",
            )
          }
        >
          {({ isActive }) => (
            <>
              <item.icon
                className={cn(
                  "h-3.5 w-3.5 shrink-0 transition-[color,opacity,transform] duration-150",
                  isActive ? "text-foreground opacity-95" : "opacity-65 group-hover:opacity-90",
                )}
                strokeWidth={1.7}
              />
              <span className="truncate">{t(item.key)}</span>
            </>
          )}
        </NavLink>
      ))}
    </div>
  );
}

function UserStrip({ compact = false }: { compact?: boolean }) {
  const { identity, refresh } = useTier();
  const { t } = useTranslation("account");
  const [signingOut, setSigningOut] = useState(false);

  async function signOut() {
    if (signingOut) return;
    setSigningOut(true);
    try {
      await api.logout();
    } finally {
      await refresh();
    }
  }

  if (compact) {
    return (
      <div className="flex min-w-0 items-center gap-1.5">
        <Link
          to="/account"
          className="inline-flex h-7 min-w-0 items-center gap-2 rounded-md border border-border bg-card px-2.5 text-xs transition-[color,background-color,border-color,transform] duration-150 ease-out hover:border-muted-foreground/30 hover:bg-accent/70 active:scale-[0.98]"
          title={identity?.email ?? ""}
        >
          <span className="max-w-[160px] truncate font-medium text-foreground">
            {identity?.email ?? <span className="text-muted-foreground/50">—</span>}
          </span>
          <span className="shrink-0 text-[11px] text-muted-foreground">
            {identity ? t(`admin:${ROLE_LABEL_KEY[identity.role]}`) : <span className="text-muted-foreground/50">—</span>}
          </span>
        </Link>
        <button
          type="button"
          onClick={signOut}
          disabled={signingOut || !identity}
          className={cn(TOP_BTN, "w-7 px-0 text-muted-foreground hover:border-destructive/40 hover:text-destructive")}
          aria-label={t("sign_out")}
          title={t("sign_out")}
        >
          <LogOut className="h-3.5 w-3.5" />
        </button>
      </div>
    );
  }

  return (
    <div className="flex min-w-0 items-center gap-2 px-3 py-1.5">
      <Link to="/account" className="min-w-0 flex-1 leading-tight">
        <div className="truncate text-xs font-medium text-foreground" title={identity?.email ?? ""}>
          {identity?.email ?? <span className="text-muted-foreground/50">—</span>}
        </div>
        <div className="mt-0.5 text-[11px] text-muted-foreground">
          {identity ? t(`admin:${ROLE_LABEL_KEY[identity.role]}`) : <span className="text-muted-foreground/50">—</span>}
        </div>
      </Link>
      <button
        type="button"
        onClick={signOut}
        disabled={signingOut || !identity}
        className={cn(TOP_BTN, "h-8 w-8 px-0 text-muted-foreground hover:border-destructive/40 hover:text-destructive")}
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
  const shortLabel = i18n.language.startsWith("zh") ? "中文" : "EN";

  return (
    <Select value={i18n.language} onValueChange={(v) => void chooseLanguage(i18n, v)}>
      <SelectPrimitive.Trigger
        className={cn(TOP_BTN, "group")}
        aria-label={t("change_language")}
        title={t("change_language")}
      >
        <Globe className="h-3.5 w-3.5 text-muted-foreground" />
        <span className="hidden sm:inline">{shortLabel}</span>
        <ChevronDown className="h-3 w-3 text-muted-foreground/75 transition-transform duration-150 ease-out group-data-[state=open]:rotate-180" />
      </SelectPrimitive.Trigger>
      <SelectContent align="end" className="min-w-[6.5rem]">
        <SelectItem value="en-US">English</SelectItem>
        <SelectItem value="zh-CN">中文</SelectItem>
      </SelectContent>
    </Select>
  );
}

function ThemeToggle() {
  const { preference, setTheme } = useTheme();
  const { t } = useTranslation("common");
  const label = preference === "light" ? t("theme_light") : preference === "dark" ? t("theme_dark") : t("theme_system");

  return (
    <Select value={preference} onValueChange={(value) => setTheme(value as ThemePreference)}>
      <SelectPrimitive.Trigger className={cn(TOP_BTN, "group")} aria-label={t("toggle_theme")} title={t("toggle_theme")}>
        {preference === "light" && <Sun className="h-3.5 w-3.5 text-muted-foreground" />}
        {preference === "dark" && <Moon className="h-3.5 w-3.5 text-muted-foreground" />}
        {preference === "system" && <Monitor className="h-3.5 w-3.5 text-muted-foreground" />}
        <span className="hidden lg:inline">{label}</span>
        <ChevronDown className="h-3 w-3 text-muted-foreground/75 transition-transform duration-150 ease-out group-data-[state=open]:rotate-180" />
      </SelectPrimitive.Trigger>
      <SelectContent align="end">
        <SelectItem value="light">{t("theme_light")}</SelectItem>
        <SelectItem value="dark">{t("theme_dark")}</SelectItem>
        <SelectItem value="system">{t("theme_system")}</SelectItem>
      </SelectContent>
    </Select>
  );
}

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
      className="flex animate-slide-down flex-wrap items-center justify-between gap-2 border-b border-amber-500/30 bg-amber-500/10 px-4 py-2 text-xs text-amber-800 dark:text-amber-200"
    >
      <span className="flex min-w-0 items-center gap-2">
        <WifiOff className="h-3.5 w-3.5 shrink-0" />
        {t("connection_lost")}
      </span>
      <Button size="sm" variant="outline" onClick={() => window.location.reload()}>
        {t("reload_page")}
      </Button>
    </div>
  );
}

function TopbarBreadcrumb({
  sections,
  activeServer,
}: {
  sections: NavSection[];
  activeServer: ActiveServerMeta | null;
}) {
  const { pathname } = useLocation();
  const { t } = useTranslation("navigation");

  if (activeServer) {
    return (
      <div key={`server:${activeServer.name}`} className="flex min-w-0 animate-fade-in items-center gap-1.5 text-xs">
        <Link
          to="/servers"
          className="inline-flex h-6 shrink-0 items-center rounded-md px-1.5 text-muted-foreground transition-colors duration-150 hover:bg-accent/70 hover:text-foreground"
        >
          {t("my_servers")}
        </Link>
        <span className="text-muted-foreground/45">/</span>
        <span className="inline-flex min-w-0 items-center gap-1.5 font-medium text-foreground">
          {activeServer.phase && (
            <span
              className="h-1.5 w-1.5 shrink-0 rounded-full transition-colors duration-200"
              style={{ backgroundColor: phaseColor(activeServer.phase) }}
            />
          )}
          <span className="truncate">{activeServer.displayName || activeServer.name}</span>
        </span>
      </div>
    );
  }

  for (const section of sections) {
    for (const item of section.items) {
      const matched = item.end ? pathname === item.to : pathname === item.to || pathname.startsWith(`${item.to}/`);
      if (matched) {
        return (
          <div key={item.to} className="flex min-w-0 animate-fade-in items-center gap-1.5 text-xs">
            {section.titleKey && (
              <>
                <span className="shrink-0 text-muted-foreground">{t(section.titleKey)}</span>
                <span className="text-muted-foreground/45">/</span>
              </>
            )}
            <span className="truncate font-medium text-foreground">{t(item.key)}</span>
          </div>
        );
      }
    }
  }

  return null;
}

function MobileNav({ sections }: { sections: NavSection[] }) {
  const { t } = useTranslation("common");
  const { name } = useBranding();
  const { pathname } = useLocation();
  const [open, setOpen] = useState(false);
  useEffect(() => setOpen(false), [pathname]);

  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
      <DialogPrimitive.Trigger
        className={cn(TOP_BTN, "w-7 px-0")}
        aria-label={t("open_menu")}
        title={t("open_menu")}
      >
        <Menu className="h-4 w-4" />
      </DialogPrimitive.Trigger>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/55 backdrop-blur-sm duration-200 ease-out data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:duration-150 data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 md:hidden" />
        <DialogPrimitive.Content
          aria-describedby={undefined}
          className="fixed inset-y-0 left-0 z-50 flex w-60 max-w-[82vw] flex-col border-r border-border bg-sidebar py-3 shadow-2xl duration-300 ease-[cubic-bezier(0.22,1,0.36,1)] data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:duration-200 data-[state=closed]:slide-out-to-left data-[state=open]:slide-in-from-left md:hidden"
        >
          <div className="mb-2 flex h-9 items-center justify-between border-b border-border/60 px-4 pb-2">
            <DialogPrimitive.Title className="flex items-center gap-2 text-[13.5px] font-semibold tracking-tight">
              <BrandMark size={18} />
              {name}
            </DialogPrimitive.Title>
            <DialogPrimitive.Close className={cn(TOP_BTN, "w-7 px-0")} aria-label={t("close_sr")}>
              <X className="h-3.5 w-3.5" />
            </DialogPrimitive.Close>
          </div>
          <nav className="flex flex-1 flex-col overflow-y-auto py-1">
            {sections.map((section, i) => (
              <SectionGroup
                key={section.id}
                section={section}
                isFirst={i === 0}
                onNavigate={() => setOpen(false)}
              />
            ))}
          </nav>
          <div className="mt-auto border-t border-border/60 pt-2">
            <UserStrip />
            <div className="mt-1 flex items-center justify-between gap-2 px-4">
              <VersionBadge />
              <PoweredBy className="text-[10px]" />
            </div>
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

export function AppShell() {
  const [headerHost, setHeaderHost] = useState<HTMLDivElement | null>(null);
  const navRef = useRef<HTMLElement>(null);
  const routeContentRef = useRef<HTMLDivElement>(null);
  const [sidebarSlot, setSidebarSlot] = useState<HTMLDivElement | null>(null);
  const [main, setMain] = useState<HTMLElement | null>(null);
  const [activeServer, setActiveServerState] = useState<ActiveServerMeta | null>(null);
  const { isAdmin, isOwner } = useTier();
  const { pathname } = useLocation();
  const { t, i18n } = useTranslation("navigation");
  const { name } = useBranding();

  const setActiveServer = useCallback((server: ActiveServerMeta | null) => {
    setActiveServerState(server);
  }, []);

  const motionKey = routeMotionKey(pathname);
  useEnterAnimation(routeContentRef, motionKey, -6, 200, 0.4);

  useEffect(() => {
    if (main) main.scrollTop = 0;
  }, [main, pathname]);

  const config = useConfig();
  const { features } = useFeatures();
  const sections = visibleSections(isAdmin, isOwner, {
    noLobby: config?.noLobby || !features.useLobby,
    nano: config?.nano,
    customImages: features.customImages,
    allowSubmissions: features.allowSubmissions,
  });
  const navIndicator = useSlidingIndicator(navRef, '[aria-current="page"]', `${pathname}:${sections.length}:${Boolean(activeServer)}`, "y");

  useEffect(() => {
    document.documentElement.lang = i18n.language;
    document.title = t("common:page_title", { brand: name });
  }, [i18n.language, t, name]);

  const shellContext = useMemo(
    () => ({ sidebarSlot, activeServer, setActiveServer }),
    [sidebarSlot, activeServer, setActiveServer],
  );

  return (
    <ShellContext.Provider value={shellContext}>
      <div className="flex h-screen flex-col overflow-hidden bg-background">
        <ConnectionBanner />
        <ConfigBanner />

        <header className="flex h-11 shrink-0 items-center justify-between gap-3 border-b border-border bg-topbar px-3 md:px-4">
          <div className="flex min-w-0 items-center gap-2.5">
            <div className="md:hidden">
              <MobileNav sections={sections} />
            </div>
            <Link
              to="/"
              className="flex shrink-0 items-center gap-2 rounded-md py-0.5 pr-1 transition-opacity hover:opacity-85"
            >
              <BrandMark />
              <span className="text-[13.5px] font-semibold tracking-tight text-foreground">{name}</span>
            </Link>
            <span className="hidden h-3.5 w-px shrink-0 bg-border md:inline-block" aria-hidden="true" />
            <div className="hidden min-w-0 md:block">
              <TopbarBreadcrumb sections={sections} activeServer={activeServer} />
            </div>
          </div>

          <div className="flex shrink-0 items-center gap-1.5">
            <LangToggle />
            <ThemeToggle />
            <span className="hidden h-3.5 w-px shrink-0 bg-border md:inline-block" aria-hidden="true" />
            <div className="hidden md:flex">
              <UserStrip compact />
            </div>
          </div>
        </header>

        <div className="flex min-h-0 flex-1 overflow-hidden">
          <aside className="hidden w-48 shrink-0 flex-col border-r border-border bg-sidebar py-3 md:flex">
            <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
              {/* Contextual slot: ServerLayout portals single-server navigation here. */}
              <div ref={setSidebarSlot} className="empty:hidden [&:not(:empty)+nav]:hidden" />
              <nav ref={navRef} className="relative flex flex-col">
                {navIndicator.ready && (
                  <span
                    aria-hidden="true"
                    className={cn(
                      "pointer-events-none absolute inset-x-0 top-0 z-0 bg-accent before:absolute before:inset-y-1 before:left-0 before:w-[2.5px] before:rounded-r-sm before:bg-primary motion-reduce:transition-none",
                      navIndicator.animate && "transition-glide-y",
                    )}
                    style={{
                      transform: `translateY(${navIndicator.start}px)`,
                      height: `${navIndicator.size}px`,
                    }}
                  />
                )}
                {sections.map((section, i) => (
                  <SectionGroup key={section.id} section={section} isFirst={i === 0} sliding={navIndicator.ready} />
                ))}
              </nav>
            </div>

            <div className="mt-auto border-t border-border/60 px-4 pt-2.5">
              <div className="flex flex-col gap-1">
                <VersionBadge />
                <PoweredBy className="text-left text-[10.5px] leading-tight" />
              </div>
            </div>
          </aside>

          <MainScrollContext.Provider value={main}>
            <PageHeaderHostContext.Provider value={headerHost}>
              <div className="flex min-h-0 min-w-0 flex-1 flex-col">
                <div
                  ref={setHeaderHost}
                  className="shrink-0 bg-background px-4 pt-4 empty:hidden md:px-6 md:pt-5"
                />

                <main
                  ref={setMain}
                  className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4 pt-4 md:px-6"
                >
                  <div
                    ref={routeContentRef}
                    className="flex min-h-0 w-full flex-1 flex-col gap-4 after:-mt-4 after:h-6 after:shrink-0 after:content-['']"
                  >
                    <ErrorBoundary resetKey={pathname}>
                      <Suspense fallback={<Loading />}>
                        <Outlet />
                      </Suspense>
                    </ErrorBoundary>
                  </div>
                </main>
              </div>
            </PageHeaderHostContext.Provider>
          </MainScrollContext.Provider>
        </div>
      </div>
    </ShellContext.Provider>
  );
}
