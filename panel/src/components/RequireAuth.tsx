import { Navigate, Outlet } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useTier } from "@/lib/tier";

// RequireAuth is the layout gate in front of the whole authenticated app (AppShell
// and everything under it). It encodes the three-way verdict from the tier model:
//
//   loading            → a full-screen spinner (never flash login during boot /me)
//   unauthenticated    → /login   (a genuine 401: no/expired session)
//   mustChangePassword → /change-password   (forced first-login change)
//   otherwise          → render the app (<Outlet/>)
//
// The "otherwise" branch deliberately includes the graded-Zero-Trust degraded case
// (a transient/5xx /me failure leaves identity null but unauthenticated false): the
// app still renders User-Side, exactly as before local auth existed. Only a true
// 401 bounces to /login. Every admin route remains independently server-guarded.
export function RequireAuth() {
  const { loading, unauthenticated, mustChangePassword } = useTier();
  const { t } = useTranslation("common");

  if (loading) {
    return (
      <div className="flex min-h-screen items-center justify-center gap-2 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        {t("loading")}
      </div>
    );
  }
  if (unauthenticated) return <Navigate to="/login" replace />;
  if (mustChangePassword) return <Navigate to="/change-password" replace />;
  return <Outlet />;
}
