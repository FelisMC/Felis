import { Card, CardContent } from "@/components/ui/card";
import { ServerCard } from "@/components/ServerCard";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { api } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";

// MyServers is the User-Side fleet: list / wake / stop / claim the servers the
// caller owns or may claim. Creation lives on Admin-Side now (POST /servers is
// admin-tier) — the platform provisions servers; users claim and operate them.
export function MyServers() {
  const cfg = useConfig();
  const { data, error, loading, reload } = useAsync(() => api.myServers(), []);
  const servers = data ?? [];

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">My servers</h1>
        <p className="text-sm text-muted-foreground">
          Wake, stop, or claim the servers you can manage.
        </p>
      </div>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : !cfg ? (
        <Loading label="Loading config…" />
      ) : servers.length === 0 ? (
        <EmptyState
          title="No servers linked to you"
          hint="Claim an unowned server you have access to, or ask an admin to provision one."
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
          Servers and game types are provisioned and managed by the platform. You
          claim a node to operate it; raw cluster config is never exposed here
          (spec §15).
        </CardContent>
      </Card>
    </div>
  );
}
