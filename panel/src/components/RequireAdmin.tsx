import { Outlet } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { useTier } from "@/lib/tier";
import { AccessUnknown, Loading, NotAuthorized } from "@/components/States";

// Navigation guards supplement backend authorization on every data call.
// Transient identity failures offer retry rather than denying administrator access.
export function RequireAdmin() {
  const { isAdmin, loading, identityError } = useTier();
  const { t } = useTranslation("common");
  if (loading) return <Loading label={t("checking_access")} />;
  if (identityError) return <AccessUnknown error={identityError} />;
  return isAdmin ? <Outlet /> : <NotAuthorized />;
}
