import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import { ThemeProvider } from "@/lib/theme";
import { TierProvider } from "@/lib/tier";
import { AppShell } from "@/components/AppShell";
import { RequireAdmin } from "@/components/RequireAdmin";
import { RequireAuth } from "@/components/RequireAuth";
import { Login } from "@/pages/Login";
import { ChangePassword } from "@/pages/ChangePassword";
import { Dashboard } from "@/pages/Dashboard";
import { MyServers } from "@/pages/MyServers";
import { ServerConsole } from "@/pages/ServerConsole";
import { Account } from "@/pages/Account";
import { AdminHome } from "@/pages/admin/AdminHome";
import { ServerAdmin } from "@/pages/admin/ServerAdmin";
import { ImageAdmin } from "@/pages/admin/ImageAdmin";
import { OpsOverview } from "@/pages/ops/OpsOverview";
import { FleetTable } from "@/pages/ops/FleetTable";

// Three UX surfaces over two Zero-Trust tiers (DESIGN-WEB-3SIDES):
//   /        User-Side    — app-tier, every authenticated principal
//   /admin/* Admin-Side   — admin-tier, server & content administration
//   /ops/*   SysAdmin-Side— admin-tier, platform observability cockpit
// Both admin groups sit behind the SAME RequireAdmin wrapper (one is_admin gate);
// the split is by concern, not by tier. TierProvider fetches /me once at boot.
export default function App() {
  return (
    <ThemeProvider storageKey="felis-theme">
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
              <Route path="servers" element={<MyServers />} />
              <Route path="servers/:name" element={<ServerConsole />} />
              <Route path="account" element={<Account />} />

              {/* Admin-Side — admin-tier (server & content ops) */}
              <Route path="admin" element={<RequireAdmin />}>
                <Route index element={<AdminHome />} />
                <Route path="servers" element={<ServerAdmin />} />
                <Route path="images" element={<ImageAdmin />} />
              </Route>

              {/* SysAdmin-Side — admin-tier (platform observability) */}
              <Route path="ops" element={<RequireAdmin />}>
                <Route index element={<OpsOverview />} />
                <Route path="fleet" element={<FleetTable />} />
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
