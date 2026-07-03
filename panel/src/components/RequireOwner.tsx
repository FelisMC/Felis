import { Outlet } from "react-router-dom";
import { useTranslation } from "react-i18next";
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
