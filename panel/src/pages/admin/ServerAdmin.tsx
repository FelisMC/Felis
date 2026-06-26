import { Card, CardContent } from "@/components/ui/card";
import { ServerCard } from "@/components/ServerCard";
import { CreateServerDialog } from "@/components/CreateServerDialog";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { api } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";

// ServerAdmin is the Admin-Side create surface. CreateServerDialog lives here (it
// calls the admin-tier POST /servers, spec §15) rather than on User-Side, where it
// previously rendered for everyone and relied on a 403 — moving it removes that
// UX/tier mismatch.
//
// The list below currently uses the app-tier GET /me/servers, i.e. the servers
// THIS admin owns or may manage. A fleet-wide GET /admin/servers (every server,
// not just mine) is a separate Oracle-verifiable backend slice that powers the
// SysAdmin-Side FleetTable; it is intentionally not faked in here.
export function ServerAdmin() {
  const cfg = useConfig();
  const { data, error, loading, reload } = useAsync(() => api.myServers(), []);
  const servers = data ?? [];

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Servers</h1>
          <p className="text-sm text-muted-foreground">
            Create platform servers from the structured form and manage the ones
            you operate.
          </p>
        </div>
        {cfg && <CreateServerDialog cfg={cfg} onCreated={reload} />}
      </div>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : !cfg ? (
        <Loading label="Loading config…" />
      ) : servers.length === 0 ? (
        <EmptyState
          title="No servers under your management yet"
          hint="Use Create server to provision one from the vetted spec."
        />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {servers.map((s) => (
            <ServerCard key={s.name} server={s} cfg={cfg} onChanged={reload} />
          ))}
        </div>
      )}

      <Card className="border-dashed bg-transparent">
        <CardContent className="p-4 text-xs text-muted-foreground">
          Server creation goes through the structured form only — the platform maps
          your choices onto a vetted Kubernetes spec. Raw cluster config (host
          networking, host paths, arbitrary images, privileged pods) is never
          expressible here (spec §15).
        </CardContent>
      </Card>
    </div>
  );
}
