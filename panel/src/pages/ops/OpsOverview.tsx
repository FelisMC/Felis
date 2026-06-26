import { Link } from "react-router-dom";
import { Gauge, Network, ServerOff } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { PendingBackend } from "@/components/States";

// OpsOverview is the SysAdmin-Side landing: a platform-observability cockpit. It is
// admin-tier (same gate as Admin-Side) but a separate *concern* — read-mostly
// visibility across the whole platform, not content/server administration.
//
// The cluster-wide fleet rollup needs a fleet-wide read (GET /admin/servers) that
// does not exist yet — that is a separate Oracle-verifiable backend slice. Until
// then this surface is honest about the gap rather than charting fake data.
export function OpsOverview() {
  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <div className="flex items-center gap-3">
        <Gauge className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">SysAdmin</h1>
          <p className="text-sm text-muted-foreground">
            Platform observability cockpit — a window, not a lever.
          </p>
        </div>
      </div>

      <Card>
        <CardContent className="space-y-3 p-4">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Network className="h-4 w-4 text-primary" /> Cluster-wide fleet
          </div>
          <PendingBackend
            endpoint="GET /api/v1/admin/servers"
            note="A platform-scoped fleet read (every server, not just the ones you own) powers the cluster-wide rollup and 3D fleet view. It is an admin-tier backend slice still to be added — distinct from the app-tier GET /me/servers."
          />
          <Link
            to="/ops/fleet"
            className="inline-block text-sm font-medium text-primary hover:underline"
          >
            Open fleet table →
          </Link>
        </CardContent>
      </Card>

      <Card className="border-dashed bg-transparent">
        <CardContent className="flex items-start gap-3 p-4 text-xs text-muted-foreground">
          <ServerOff className="mt-0.5 h-4 w-4 shrink-0" />
          <span>
            Control-plane scaling, RBAC, Secrets and cluster lifecycle are{" "}
            <span className="font-medium">kubectl / CRD operations</span> and are not
            reachable from here. SysAdmin-Side is observability only — the
            four-power separation (build / runtime / operator / app) is preserved.
          </span>
        </CardContent>
      </Card>
    </div>
  );
}
