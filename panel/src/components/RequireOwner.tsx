import { Outlet } from "react-router-dom";
import { useTier } from "@/lib/tier";
import { NotAuthorized } from "@/components/States";
import { Loading } from "@/components/States";

export function RequireOwner() {
  const { loading, isOwner } = useTier();

  if (loading) {
    return <Loading />;
  }
  if (!isOwner) {
    return <NotAuthorized />;
  }
  return <Outlet />;
}
