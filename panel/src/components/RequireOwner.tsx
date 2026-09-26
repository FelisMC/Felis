import { Outlet } from "react-router-dom";
import { useTier } from "@/lib/tier";
import { AccessUnknown, Loading, NotAuthorized } from "@/components/States";

// RequireOwner gates user management, one level above admin. Like RequireAdmin
// it waits for /me, and a /me that failed with anything but a 401 gets a retry:
// the owner is not told "not authorized" on a flaky read.
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
