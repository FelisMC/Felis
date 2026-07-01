import { useState } from "react";
import { Server, Search, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ServerCard } from "@/components/ServerCard";
import { Pagination } from "@/components/Pagination";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";
import { hostFor } from "@/lib/config";

const PAGE_SIZE = 12;

export function MyServers() {
  const cfg = useConfig();
  const { data, error, loading, reload } = useAsync(() => api.myServers(), []);
  const { t } = useTranslation("servers");
  const servers = data ?? [];
  const [page, setPage] = useState(1);
  const [searchQuery, setSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState("all");

  const filtered = servers.filter((s) => {
    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase().trim();
      const host = cfg ? hostFor(s.subdomain, cfg).toLowerCase() : "";
      const name = s.name.toLowerCase();
      const displayName = (s.displayName || "").toLowerCase();
      if (!name.includes(q) && !displayName.includes(q) && !host.includes(q)) {
        return false;
      }
    }
    if (statusFilter !== "all") {
      if (s.phase !== statusFilter) {
        return false;
      }
    }
    return true;
  });

  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const paged = filtered.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE);

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-3">
        <Server className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t("my_servers_title")}</h1>
          <p className="text-sm text-muted-foreground">
            {t("my_servers_subtitle")}
          </p>
        </div>
      </div>

      {servers.length > 0 && cfg && (
        <div className="flex flex-wrap items-center gap-3">
          <div className="relative min-w-[14rem] flex-1">
            <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              placeholder={t("search_placeholder")}
              className="pl-9 pr-9"
              value={searchQuery}
              onChange={(e) => {
                setSearchQuery(e.target.value);
                setPage(1);
              }}
              autoComplete="off"
              spellCheck={false}
            />
            {searchQuery && (
              <button
                type="button"
                onClick={() => {
                  setSearchQuery("");
                  setPage(1);
                }}
                className="absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              >
                <X className="h-4 w-4" />
              </button>
            )}
          </div>
          <Select
            value={statusFilter}
            onValueChange={(val) => {
              setStatusFilter(val);
              setPage(1);
            }}
          >
            <SelectTrigger className="w-44">
              <SelectValue placeholder={t("filter_status_all")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("filter_status_all")}</SelectItem>
              <SelectItem value="Running">{t("phase_running")}</SelectItem>
              <SelectItem value="Starting">{t("phase_starting")}</SelectItem>
              <SelectItem value="Stopping">{t("phase_stopping")}</SelectItem>
              <SelectItem value="Stopped">{t("phase_stopped")}</SelectItem>
              <SelectItem value="Failed">{t("phase_failed")}</SelectItem>
            </SelectContent>
          </Select>
          <span className="shrink-0 text-xs text-muted-foreground">
            {filtered.length === servers.length
              ? t("servers_count", { count: servers.length })
              : t("servers_count_filtered", {
                  shown: filtered.length,
                  total: servers.length,
                })}
          </span>
        </div>
      )}

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
      ) : filtered.length === 0 ? (
        <div className="flex flex-col items-center justify-center py-12 text-center border border-dashed rounded-lg border-border bg-card/50">
          <p className="text-sm font-medium text-foreground">{t("search_no_match")}</p>
          <Button
            variant="outline"
            size="sm"
            className="mt-4"
            onClick={() => {
              setSearchQuery("");
              setStatusFilter("all");
              setPage(1);
            }}
          >
            {t("search_clear_btn")}
          </Button>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {paged.map((s) => (
            <ServerCard key={s.name} server={s} cfg={cfg} onChanged={reload} />
          ))}
        </div>
      )}

      {totalPages > 1 && (
        <div className="pt-3">
          <Pagination
            page={page}
            pageSize={PAGE_SIZE}
            total={filtered.length}
            onChange={(p) => { setPage(p); if (p > totalPages) setPage(totalPages); }}
          />
        </div>
      )}
    </div>
  );
}
