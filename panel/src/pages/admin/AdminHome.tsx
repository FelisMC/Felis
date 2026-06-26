import { Link } from "react-router-dom";
import { ServerCog, Boxes, ShieldCheck } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { useTier } from "@/lib/tier";

// AdminHome is the Admin-Side landing: a short orientation plus links into the
// content/server-admin surfaces. It deliberately states the four-power boundary
// so an admin never expects cluster-lifecycle levers here — those stay kubectl/CRD.

const LINKS = [
  {
    to: "/admin/servers",
    icon: ServerCog,
    title: "Servers",
    body: "Create platform servers from the structured §15 form and manage the ones you operate.",
  },
  {
    to: "/admin/images",
    icon: Boxes,
    title: "Images",
    body: "The platform image whitelist — the value space the create form draws from.",
  },
];

export function AdminHome() {
  const { identity } = useTier();

  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <div className="flex items-center gap-3">
        <ShieldCheck className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Admin</h1>
          <p className="text-sm text-muted-foreground">
            Server &amp; content administration
            {identity?.email ? ` · signed in as ${identity.email}` : ""}.
          </p>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        {LINKS.map((l) => (
          <Link key={l.to} to={l.to}>
            <Card className="h-full transition-colors hover:border-primary/40">
              <CardContent className="space-y-2 p-4">
                <l.icon className="h-5 w-5 text-primary" />
                <div className="font-medium">{l.title}</div>
                <p className="text-sm text-muted-foreground">{l.body}</p>
              </CardContent>
            </Card>
          </Link>
        ))}
      </div>

      <Card className="border-dashed bg-transparent">
        <CardContent className="p-4 text-xs text-muted-foreground">
          Cluster scaling, RBAC, Secrets and control-plane lifecycle are{" "}
          <span className="font-medium">kubectl / CRD operations</span> and are
          intentionally not available from the panel — the four-power separation
          (build / runtime / operator / app) is preserved. This is a window onto the
          platform, not a lever for operator power.
        </CardContent>
      </Card>
    </div>
  );
}
