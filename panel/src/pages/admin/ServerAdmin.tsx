import { useState } from "react";
import { ServerCog } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { ServerCard } from "@/components/ServerCard";
import { Pagination } from "@/components/Pagination";
import { CreateServerDialog } from "@/components/CreateServerDialog";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { api } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";

const PAGE_SIZE = 12;

export function ServerAdmin() {
  const cfg = useConfig();
  const { data, error, loading, reload } = useAsync(() => api.myServers(), []);
  const { t } = useTranslation("admin");
  const { t: ts } = useTranslation("servers");
  const servers = data ?? [];
  const [page, setPage] = useState(1);
  const totalPages = Math.max(1, Math.ceil(servers.length / PAGE_SIZE));
  const paged = servers.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE);

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <ServerCog className="h-6 w-6 text-primary" />
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{t("servers")}</h1>
            <p className="text-sm text-muted-foreground">
              {t("server_admin_subtitle")}
            </p>
          </div>
        </div>
        {cfg && <CreateServerDialog cfg={cfg} onCreated={reload} />}
      </div>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : !cfg ? (
        <Loading label={ts("common:loading_config")} />
      ) : servers.length === 0 ? (
        <EmptyState
          title={ts("no_servers_managed")}
          hint={ts("no_servers_managed_hint")}
        />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {paged.map((s) => (
            <ServerCard key={s.name} server={s} cfg={cfg} onChanged={reload} />
          ))}
        </div>
      )}

      {servers.length > 0 && (
        <div className="pt-3">
          <Pagination
            page={page}
            pageSize={PAGE_SIZE}
            total={servers.length}
            onChange={(p) => { setPage(p); if (p > totalPages) setPage(totalPages); }}
          />
        </div>
      )}
    </>
  );
}
