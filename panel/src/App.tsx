import { Suspense } from "react";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import { ThemeProvider } from "@/lib/theme";
import { TierProvider } from "@/lib/tier";
import { AppShell } from "@/components/AppShell";
import { RequireAdmin } from "@/components/RequireAdmin";
import { RequireAuth } from "@/components/RequireAuth";
import { RequireOwner } from "@/components/RequireOwner";
import { SetupRequiredRedirect } from "@/components/SetupRequiredRedirect";
import { Loading } from "@/components/States";
import { ValidParam } from "@/components/ValidParam";
import { SERVER_NAME_PARAM, USER_ID_PARAM } from "@/lib/params";
import { Login } from "@/pages/Login";
import { Dashboard } from "@/pages/Dashboard";
import { lazyWithReload } from "@/lib/chunk";

// Sign-in and the landing page ship in the entry bundle; every other page is
// its own chunk, fetched the first time it is opened, so a phone opening the
// panel never downloads the admin pages it cannot use. Vite names each chunk
// after its page, and panel.go caches the hashed files for a year.
const Setup = lazyWithReload(() => import("@/pages/Setup").then((m) => ({ default: m.Setup })));
const ServersPage = lazyWithReload(() =>
  import("@/pages/servers/ServersPage").then((m) => ({ default: m.ServersPage })),
);
const ServerConsole = lazyWithReload(() =>
  import("@/pages/ServerConsole").then((m) => ({ default: m.ServerConsole })),
);
const ServerPlayers = lazyWithReload(() =>
  import("@/pages/ServerPlayers").then((m) => ({ default: m.ServerPlayers })),
);
const ServerBackups = lazyWithReload(() =>
  import("@/pages/ServerBackups").then((m) => ({ default: m.ServerBackups })),
);
const ServerFiles = lazyWithReload(() =>
  import("@/pages/ServerFiles").then((m) => ({ default: m.ServerFiles })),
);
const ServerSchedules = lazyWithReload(() =>
  import("@/pages/ServerSchedules").then((m) => ({ default: m.ServerSchedules })),
);
const ServerLuckPerms = lazyWithReload(() =>
  import("@/pages/ServerLuckPerms").then((m) => ({ default: m.ServerLuckPerms })),
);
const Account = lazyWithReload(() => import("@/pages/Account").then((m) => ({ default: m.Account })));
const MySubmissionsPage = lazyWithReload(() =>
  import("@/pages/MySubmissionsPage").then((m) => ({ default: m.MySubmissionsPage })),
);
const ImageAdmin = lazyWithReload(() =>
  import("@/pages/admin/ImageAdmin").then((m) => ({ default: m.ImageAdmin })),
);
const ImageBuildPage = lazyWithReload(() =>
  import("@/pages/admin/ImageBuildPage").then((m) => ({ default: m.ImageBuildPage })),
);
const SubmissionsPage = lazyWithReload(() =>
  import("@/pages/admin/SubmissionsPage").then((m) => ({ default: m.SubmissionsPage })),
);
const UsersPage = lazyWithReload(() =>
  import("@/pages/admin/UsersPage").then((m) => ({ default: m.UsersPage })),
);
const UserDetailPage = lazyWithReload(() =>
  import("@/pages/admin/UserDetailPage").then((m) => ({ default: m.UserDetailPage })),
);
const LobbyPage = lazyWithReload(() =>
  import("@/pages/admin/LobbyPage").then((m) => ({ default: m.LobbyPage })),
);
const UpdatesPage = lazyWithReload(() =>
  import("@/pages/admin/UpdatesPage").then((m) => ({ default: m.UpdatesPage })),
);

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
          <Route
            path="/setup"
            element={
              <Suspense fallback={<Loading />}>
                <Setup />
              </Suspense>
            }
          />

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
                <Route path="schedules" element={<ServerSchedules />} />
                <Route path="luckperms" element={<ServerLuckPerms />} />
              </Route>
              <Route path="submissions" element={<MySubmissionsPage />} />
              <Route path="account" element={<Account />} />

              {/* Admin-Side — admin-tier (server & content ops).
                  /admin has no landing page; redirect to the first concrete view
                  so the section root and any stale bookmarks land somewhere useful. */}
              <Route path="admin" element={<RequireAdmin />}>
                <Route index element={<Navigate to="/admin/images" replace />} />
                <Route path="lobby" element={<LobbyPage />} />
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
