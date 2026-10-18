import { Outlet, useParams } from "react-router-dom";
import { NotFound } from "@/components/States";

// Remount on parameter changes to discard another server's drafts, dialogs, and log buffer.
export function ValidParam({ param, pattern }: { param: string; pattern: RegExp }) {
  const value = useParams()[param];
  if (value === undefined || !pattern.test(value)) return <NotFound />;
  return <Outlet key={value} />;
}
