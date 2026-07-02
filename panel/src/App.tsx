import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import { ThemeProvider } from "@/lib/theme";
import { TierProvider } from "@/lib/tier";
import { AppShell } from "@/components/AppShell";
import { RequireAdmin } from "@/components/RequireAdmin";
import { RequireAuth } from "@/components/RequireAuth";
import { Login } from "@/pages/Login";
import { ChangePassword } from "@/pages/ChangePassword";
import { Dashboard } from "@/pages/Dashboard";
import { ServersPage } from "@/pages/servers/ServersPage";
import { ServerConsole } from "@/pages/ServerConsole";
import { ServerPlayers } from "@/pages/ServerPlayers";
import { ServerBackups } from "@/pages/ServerBackups";
import { Account } from "@/pages/Account";
import { ImageAdmin } from "@/pages/admin/ImageAdmin";
import { ImageBuildPage } from "@/pages/admin/ImageBuildPage";

// Three UX surfaces over two Zero-Trust tiers (DESIGN-WEB-3SIDES):
//   /        User-Side    — app-tier, every authenticated principal
//   /admin/* Admin-Side   — admin-tier, server & content administration
//   /ops/*   SysAdmin-Side— admin-tier, platform observability cockpit
// Both admin groups sit behind the SAME RequireAdmin wrapper (one is_admin gate);
// the split is by concern, not by tier. TierProvider fetches /me once at boot.
export default function App() {
  return (
    <ThemeProvider>
      <TierProvider>
        <BrowserRouter>
        <Routes>
          {/* Pre-app local-password surfaces (spec §B1). They sit OUTSIDE
              RequireAuth — RequireAuth redirects here — and outside AppShell, so
              they render their own centered chrome with no nav/tier dependency. */}
          <Route path="/login" element={<Login />} />
          <Route path="/change-password" element={<ChangePassword />} />

          {/* Everything else requires a session. RequireAuth gates the whole app:
              no/expired session → /login, forced first-login change →
              /change-password, transient /me failure → still renders (graded ZT). */}
          <Route element={<RequireAuth />}>
            <Route element={<AppShell />}>
              {/* User-Side — app-tier */}
              <Route index element={<Dashboard />} />
              <Route path="servers" element={<ServersPage />} />
              <Route path="servers/:name" element={<ServerConsole />} />
              <Route path="servers/:name/players" element={<ServerPlayers />} />
              <Route path="servers/:name/backups" element={<ServerBackups />} />
              <Route path="account" element={<Account />} />

              {/* Admin-Side — admin-tier (server & content ops).
                  /admin has no landing page; redirect to the first concrete view
                  so the section root and any stale bookmarks land somewhere useful. */}
              <Route path="admin" element={<RequireAdmin />}>
                <Route index element={<Navigate to="/admin/images" replace />} />
                <Route path="images" element={<ImageAdmin />} />
                <Route path="builds" element={<ImageBuildPage />} />
              </Route>

              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Route>
        </Routes>
      </BrowserRouter>
    </TierProvider>
  </ThemeProvider>
  );
}
