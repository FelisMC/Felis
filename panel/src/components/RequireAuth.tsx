import { Navigate, Outlet, useLocation } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useTier } from "@/lib/tier";

// RequireAuth is the layout gate in front of the whole authenticated app (AppShell
// and everything under it). It encodes the three-way verdict from the tier model:
//
//   loading            → a full-screen spinner (never flash login during boot /me)
//   unauthenticated    → /login   (a genuine 401: no/expired session), carrying
//                        the page to return to (?next=) and, when a session
//                        ended under an open page, state.sessionEnded
//   otherwise          → render the app (<Outlet/>)
//
// The "otherwise" branch deliberately includes the graded-Zero-Trust degraded case
// (a transient/5xx /me failure leaves identity null but unauthenticated false): the
// app still renders User-Side, exactly as before local auth existed. Only a true
// 401 bounces to /login. Every admin route remains independently server-guarded.
export function RequireAuth() {
  const { loading, unauthenticated, sessionEnded } = useTier();
  const { t } = useTranslation("common");
  const location = useLocation();

  if (loading) {
    return (
      <div className="flex min-h-screen items-center justify-center gap-2 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        {t("loading")}
      </div>
    );
  }
  if (unauthenticated) {
    const back = location.pathname + location.search;
    const to = back === "/" ? "/login" : `/login?next=${encodeURIComponent(back)}`;
    return <Navigate to={to} replace state={sessionEnded ? { sessionEnded: true } : undefined} />;
  }
  return <Outlet />;
}
