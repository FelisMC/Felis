import { Navigate, Outlet, useLocation } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useTier } from "@/lib/tier";

// Only 401 redirects to login; preserve the return page.
// Transient identity failures retain user-side access while backend role checks remain authoritative.
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
