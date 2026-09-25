import { Outlet, useParams } from "react-router-dom";
import { NotFound } from "@/components/States";

// ValidParam is a layout route that renders its children only when one URL
// parameter has the expected shape (lib/params.ts), and "not found" otherwise.
export function ValidParam({ param, pattern }: { param: string; pattern: RegExp }) {
  const value = useParams()[param];
  if (value === undefined || !pattern.test(value)) return <NotFound />;
  return <Outlet />;
}
