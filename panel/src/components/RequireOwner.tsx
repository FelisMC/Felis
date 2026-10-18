import { Outlet } from "react-router-dom";
import { useTier } from "@/lib/tier";
import { AccessUnknown, Loading, NotAuthorized } from "@/components/States";

// Retry unknown owner access after transient identity failures instead of declaring the account unauthorized.
export function RequireOwner() {
  const { loading, isOwner, identityError } = useTier();

  if (loading) {
    return <Loading />;
  }
  if (identityError) {
    return <AccessUnknown error={identityError} />;
  }
  if (!isOwner) {
    return <NotAuthorized />;
  }
  return <Outlet />;
}
