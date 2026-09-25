import { useEffect } from "react";
import { BrowserRouter, Routes, Route, Navigate, useNavigate } from "react-router-dom";
import { ThemeProvider } from "@/lib/theme";
import { TierProvider } from "@/lib/tier";
import { SETUP_REQUIRED_EVENT } from "@/lib/api";
import { AppShell } from "@/components/AppShell";
import { RequireAdmin } from "@/components/RequireAdmin";
import { RequireAuth } from "@/components/RequireAuth";
import { RequireOwner } from "@/components/RequireOwner";
import { ValidParam } from "@/components/ValidParam";
import { SERVER_NAME_PARAM, USER_ID_PARAM } from "@/lib/params";
import { Login } from "@/pages/Login";
import { Setup } from "@/pages/Setup";
import { Dashboard } from "@/pages/Dashboard";
import { ServersPage } from "@/pages/servers/ServersPage";
import { ServerConsole } from "@/pages/ServerConsole";
import { ServerPlayers } from "@/pages/ServerPlayers";
import { ServerBackups } from "@/pages/ServerBackups";
import { ServerFiles } from "@/pages/ServerFiles";
import { ServerLuckPerms } from "@/pages/ServerLuckPerms";
import { Account } from "@/pages/Account";
import { ImageAdmin } from "@/pages/admin/ImageAdmin";
import { ImageBuildPage } from "@/pages/admin/ImageBuildPage";
import { SubmissionsPage } from "@/pages/admin/SubmissionsPage";
import { UsersPage } from "@/pages/admin/UsersPage";
import { UserDetailPage } from "@/pages/admin/UserDetailPage";
import { MySubmissionsPage } from "@/pages/MySubmissionsPage";
import { UpdatesPage } from "@/pages/admin/UpdatesPage";

// SetupRequiredRedirect listens for the `403 setup_required` signal api.ts emits
// when a session still owes forced onboarding (#8) and routes it to the wizard.
// It must live inside the Router (it navigates) and outside RequireAuth (/setup
// sits there too); the event fires from any protected call the app makes, so the
// listener is always mounted by the time one arrives.
function SetupRequiredRedirect() {
  const navigate = useNavigate();
  useEffect(() => {
    const toSetup = () => navigate("/setup", { replace: true });
    window.addEventListener(SETUP_REQUIRED_EVENT, toSetup);
    return () => window.removeEventListener(SETUP_REQUIRED_EVENT, toSetup);
  }, [navigate]);
  return null;
}

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
        <SetupRequiredRedirect />
        <Routes>
          {/* Pre-app sign-in surface (spec §B, passwordless). It sits OUTSIDE
              RequireAuth — RequireAuth redirects here — and outside AppShell, so
              it renders its own centered chrome with no nav/tier dependency. */}
          <Route path="/login" element={<Login />} />
          {/* Owner first-run onboarding. Like /login it sits OUTSIDE RequireAuth:
              the visitor arrives from the `felis setup` link with no session, and
              redeeming the one-time token is what mints one. */}
          <Route path="/setup" element={<Setup />} />

          {/* Everything else requires a session. RequireAuth gates the whole app:
              no/expired session → /login, transient /me failure → still renders
              (graded ZT). */}
          <Route element={<RequireAuth />}>
            <Route element={<AppShell />}>
              {/* User-Side — app-tier */}
              <Route index element={<Dashboard />} />
              <Route path="servers" element={<ServersPage />} />
              {/* A :name that is not a server name (a crafted link carrying
                  "/", "?" or "..") renders "not found" before any request. */}
              <Route
                path="servers/:name"
                element={<ValidParam param="name" pattern={SERVER_NAME_PARAM} />}
              >
                <Route index element={<ServerConsole />} />
                <Route path="players" element={<ServerPlayers />} />
                <Route path="backups" element={<ServerBackups />} />
                <Route path="files" element={<ServerFiles />} />
                <Route path="luckperms" element={<ServerLuckPerms />} />
              </Route>
              <Route path="submissions" element={<MySubmissionsPage />} />
              <Route path="account" element={<Account />} />

              {/* Admin-Side — admin-tier (server & content ops).
                  /admin has no landing page; redirect to the first concrete view
                  so the section root and any stale bookmarks land somewhere useful. */}
              <Route path="admin" element={<RequireAdmin />}>
                <Route index element={<Navigate to="/admin/images" replace />} />
                <Route path="images" element={<ImageAdmin />} />
                <Route path="builds" element={<ImageBuildPage />} />
                <Route path="submissions" element={<SubmissionsPage />} />
                <Route path="updates" element={<UpdatesPage />} />
                {/* Owner-gated: user management (one level above admin). */}
                <Route element={<RequireOwner />}>
                  <Route path="users" element={<UsersPage />} />
                  <Route path="users/:id" element={<ValidParam param="id" pattern={USER_ID_PARAM} />}>
                    <Route index element={<UserDetailPage />} />
                  </Route>
                </Route>
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
