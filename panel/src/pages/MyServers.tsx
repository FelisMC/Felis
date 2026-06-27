import { Server } from "lucide-react";
import { useTranslation } from "react-i18next";
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
  const { t } = useTranslation("servers");
  const servers = data ?? [];

  return (
    <>
      <div className="flex items-center gap-3">
        <Server className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t("my_servers_title")}</h1>
          <p className="text-sm text-muted-foreground">
            {t("my_servers_subtitle")}
          </p>
        </div>
      </div>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : !cfg ? (
        <Loading label={t("common:loading_config")} />
      ) : servers.length === 0 ? (
        <EmptyState
          title={t("no_servers_linked")}
          hint={t("no_servers_hint")}
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
          {t("my_servers_footer")}
        </CardContent>
      </Card>
    </>
  );
}
