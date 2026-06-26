import { Network } from "lucide-react";
import { PendingBackend } from "@/components/States";

// FleetTable is the SysAdmin-Side cluster-wide server list: ALL servers with
// phase + capacity, distinct from the User-Side "My servers". It depends on the
// fleet-wide GET /admin/servers read, which is the next Oracle-verifiable backend
// slice. Honest placeholder until that lands — no fabricated rows.
export function FleetTable() {
  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div className="flex items-center gap-3">
        <Network className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Fleet</h1>
          <p className="text-sm text-muted-foreground">
            Every server on the platform — phase, owner, capacity.
          </p>
        </div>
      </div>

      <PendingBackend
        endpoint="GET /api/v1/admin/servers"
        note="The cluster-wide fleet table reads every server's status (not just yours). That admin-tier read is the next backend slice; rows appear here once it ships."
      />
    </div>
  );
}
