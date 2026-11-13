import type { ReactNode } from "react";
import { Navigate, Outlet } from "react-router-dom";
import { Loading } from "@/components/States";
import { useConfig } from "@/lib/hooks";

/** FullFelisOnly holds the server pages; a Felis-nano panel has none and sends visitors to their account. */
export function FullFelisOnly() {
  const config = useConfig();
  if (!config) return <Loading />;
  return config.nano ? <Navigate to="/account" replace /> : <Outlet />;
}

/** NanoSwitch renders the Felis-nano variant of a page on a nano install. */
export function NanoSwitch({ nano, full }: { nano: ReactNode; full: ReactNode }) {
  const config = useConfig();
  if (!config) return <Loading />;
  return <>{config.nano ? nano : full}</>;
}
