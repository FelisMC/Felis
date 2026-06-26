import { NavLink, Outlet } from "react-router-dom";
import { Cat } from "lucide-react";
import { cn } from "@/lib/utils";
import { useTier } from "@/lib/tier";
import { visibleSections } from "@/lib/nav";

export function AppShell() {
  const { isAdmin } = useTier();
  // Sections are derived purely from is_admin: User-Side always, Admin/SysAdmin
  // only for admins. isAdmin is fail-closed (false while /me loads or on failure),
  // so admin sections appear only once identity is confirmed.
  const sections = visibleSections(isAdmin);

  return (
    <div className="flex min-h-screen">
      <aside className="hidden w-60 shrink-0 flex-col border-r border-border bg-card/40 p-4 md:flex">
        <div className="mb-6 flex items-center gap-2 px-2">
          <Cat className="h-6 w-6 text-primary" />
          <span className="text-lg font-semibold tracking-tight">Felis</span>
        </div>

        <nav className="flex flex-col gap-4">
          {sections.map((section) => (
            <div key={section.id} className="flex flex-col gap-1">
              {section.title && (
                <div className="px-3 pb-1 text-xs font-semibold uppercase tracking-wider text-muted-foreground/70">
                  {section.title}
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
                  {item.label}
                </NavLink>
              ))}
            </div>
          ))}
        </nav>

        <div className="mt-auto px-3 pt-4 text-xs text-muted-foreground">
          K8s-native Minecraft orchestration
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 items-center gap-2 border-b border-border px-4 md:hidden">
          <Cat className="h-5 w-5 text-primary" />
          <span className="font-semibold">Felis</span>
        </header>
        <main className="flex-1 p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
