import { useState, useCallback, useEffect } from "react";
import { Link, useNavigate } from "react-router-dom";
import { RoleBadge } from "@/components/RoleBadge";
import { UserStatusBadge } from "@/components/UserStatusBadge";
import {
  Users,
  Search,
  Server,
  Mail,
  ChevronRight,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { Pagination } from "@/components/Pagination";
import { CreateUserDialog } from "@/components/CreateUserDialog";
import { SelfServiceQuotaCard } from "@/components/SelfServiceQuotaCard";
import { api } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";
import { formatAbsolute } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { UserView } from "@/lib/types";

const SEARCH_DEBOUNCE_MS = 300;

export function UsersPage() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const navigate = useNavigate();
  const config = useConfig();
  const hasServers = config !== null && !config.nano;

  // Debounce server-side search and reset pagination; Enter applies immediately.
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const [roleFilter, setRoleFilter] = useState("");
  const [disabledFilter, setDisabledFilter] = useState("");
  const [page, setPage] = useState(0);
  const pageSize = 20;

  useEffect(() => {
    const next = query.trim();
    if (next === search) return;
    const timer = setTimeout(() => {
      setSearch(next);
      setPage(0);
    }, SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [query, search]);

  const fetchUsers = useCallback(
    () =>
      api.listUsers({
        query: search || undefined,
        role: (roleFilter || undefined) as "admin" | "user" | undefined,
        disabled: (disabledFilter || undefined) as "true" | "false" | undefined,
        limit: pageSize,
        offset: page * pageSize,
      }),
    [search, roleFilter, disabledFilter, page],
  );

  const { data, error, loading, reload } = useAsync(fetchUsers, [fetchUsers], { keepPrevious: true });

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault();
    const next = query.trim();
    if (next === search && page === 0) {
      reload();
      return;
    }
    setSearch(next);
    setPage(0);
  };

  const totalPages = data ? Math.max(1, Math.ceil(data.total / pageSize)) : 1;

  return (
    <div className="space-y-4">
      <PageHeader
        icon={Users}
        title={t("users_title")}
        subtitle={t("users_subtitle")}
        actions={<CreateUserDialog onCreated={(id) => { reload(); navigate(`/admin/users/${id}`); }} />}
      />

      {hasServers && <SelfServiceQuotaCard collapsible />}

      <form onSubmit={handleSearch} className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-[200px] flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder={t("users_search_placeholder")}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="pl-8"
          />
        </div>
        <Select value={roleFilter} onValueChange={(v) => { setRoleFilter(v); setPage(0); }}>
          <SelectTrigger className="w-[124px]" aria-label={t("users_filter_role")}>
            <SelectValue placeholder={t("users_filter_role_all")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="">{t("users_filter_role_all")}</SelectItem>
            <SelectItem value="owner">{t("users_role_owner")}</SelectItem>
            <SelectItem value="admin">{t("users_role_admin")}</SelectItem>
            <SelectItem value="user">{t("users_role_user")}</SelectItem>
          </SelectContent>
        </Select>
        <Select value={disabledFilter} onValueChange={(v) => { setDisabledFilter(v); setPage(0); }}>
          <SelectTrigger className="w-[132px]" aria-label={t("users_filter_status")}>
            <SelectValue placeholder={t("users_filter_status_all")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="">{t("users_filter_status_all")}</SelectItem>
            <SelectItem value="false">{t("users_status_active")}</SelectItem>
            <SelectItem value="true">{t("users_status_disabled")}</SelectItem>
          </SelectContent>
        </Select>
        <Button type="submit" variant="outline">
          {t("users_search_btn")}
        </Button>
      </form>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : !data || data.users.length === 0 ? (
        <EmptyState
          title={t("users_empty_title")}
          hint={t("users_empty_hint")}
        >
          <CreateUserDialog
            onCreated={(id) => {
              reload();
              navigate(`/admin/users/${id}`);
            }}
          />
        </EmptyState>
      ) : (
        <>
          <Card className="overflow-hidden">
            <CardContent className="p-0">
              <div className="overflow-x-auto">
                <table className="w-full text-xs">
                  <thead>
                    <tr className="border-b border-border/60 bg-muted text-left text-[11px] text-muted-foreground">
                      <th className="px-3.5 py-2 font-normal">{t("users_col_user")}</th>
                      <th className="hidden px-3.5 py-2 font-normal sm:table-cell">{t("users_col_role")}</th>
                      {hasServers && <th className="hidden px-3.5 py-2 font-normal md:table-cell">{t("users_col_servers")}</th>}
                      <th className="hidden px-3.5 py-2 font-normal lg:table-cell">{t("users_col_status")}</th>
                      <th className="hidden px-3.5 py-2 font-normal lg:table-cell">{t("users_col_created")}</th>
                      <th className="w-0 px-3.5 py-2 font-normal" />
                    </tr>
                  </thead>
                  <tbody>
                    {data.users.map((u: UserView) => (
                      <UserRow key={u.id} user={u} locale={locale} showServers={hasServers} />
                    ))}
                  </tbody>
                </table>
              </div>
            </CardContent>
          </Card>

          {totalPages > 1 && (
            <Pagination page={page + 1} pageSize={pageSize} total={data.total} onChange={(p) => setPage(p - 1)} />
          )}
        </>
      )}
    </div>
  );
}

function UserRow({ user, locale, showServers }: { user: UserView; locale: string; showServers: boolean }) {
  const { t } = useTranslation("admin");

  return (
    <tr
      className={cn(
        "border-b border-border/50 transition-colors last:border-0 hover:bg-accent/40",
        user.disabled && "opacity-60",
      )}
    >
      <td className="px-3.5 py-2.5">
        <Link
          to={`/admin/users/${user.id}`}
          className="font-medium text-foreground hover:text-primary hover:underline"
        >
          {user.username}
        </Link>
        {user.email && (
          <div className="mt-0.5 flex items-center gap-1 text-[11px] text-muted-foreground">
            <Mail className="h-3 w-3 shrink-0 opacity-75" />
            {user.email}
          </div>
        )}
      </td>
      <td className="hidden px-3.5 py-2.5 sm:table-cell">
        <RoleBadge role={user.role} />
      </td>
      {showServers && (
        <td className="hidden px-3.5 py-2.5 md:table-cell">
          <span className="inline-flex items-center gap-1 font-mono text-muted-foreground tabular-nums">
            <Server className="h-3 w-3 opacity-75" />
            {user.server_count}
          </span>
        </td>
      )}
      <td className="hidden px-3.5 py-2.5 lg:table-cell">
        <UserStatusBadge disabled={user.disabled} />
      </td>
      <td className="hidden px-3.5 py-2.5 font-mono text-[11px] text-muted-foreground tabular-nums lg:table-cell">
        {formatAbsolute(user.created_at, locale)}
      </td>
      <td className="px-3.5 py-2.5 text-right">
        <Button variant="outline" size="sm" asChild>
          <Link to={`/admin/users/${user.id}`}>
            {t("users_view_detail")}
            <ChevronRight className="h-3.5 w-3.5 opacity-65" />
          </Link>
        </Button>
      </td>
    </tr>
  );
}
