import { Outlet } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { useTier } from "@/lib/tier";
import { AccessUnknown, Loading, NotAuthorized } from "@/components/States";

// RequireAdmin is the single route wrapper for BOTH /admin/* and /ops/* (the two
// admin-tier concerns). It is belt-and-suspenders: it spares non-admins a wall of
// 403s, but the server enforces the boundary independently on every data call.
//
// While /me is still loading we show a spinner rather than NotAuthorized, so a
// genuine admin is never briefly told "not authorized" on a slow boot. A /me that
// failed with anything but a 401 leaves the answer unknown, so the page says the
// check failed and offers a retry. Otherwise isAdmin is the fail-closed verdict.
export function RequireAdmin() {
  const { isAdmin, loading, identityError } = useTier();
  const { t } = useTranslation("common");
  if (loading) return <Loading label={t("checking_access")} />;
  if (identityError) return <AccessUnknown error={identityError} />;
  return isAdmin ? <Outlet /> : <NotAuthorized />;
}
