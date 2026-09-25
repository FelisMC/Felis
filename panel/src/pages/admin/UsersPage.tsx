import { useState, useCallback } from "react";
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
import { api } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { formatAbsolute } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { UserView } from "@/lib/types";

export function UsersPage() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const navigate = useNavigate();

  const [query, setQuery] = useState("");
  const [roleFilter, setRoleFilter] = useState("");
  const [disabledFilter, setDisabledFilter] = useState("");
  const [page, setPage] = useState(0);
  const pageSize = 20;

  const fetchUsers = useCallback(
    () =>
      api.listUsers({
        query: query || undefined,
        role: (roleFilter || undefined) as "admin" | "user" | undefined,
        disabled: (disabledFilter || undefined) as "true" | "false" | undefined,
        limit: pageSize,
        offset: page * pageSize,
      }),
    [query, roleFilter, disabledFilter, page],
  );

  const { data, error, loading, reload } = useAsync(fetchUsers, [fetchUsers], { keepPrevious: true });

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault();
    setPage(0);
    reload();
  };

  const totalPages = data ? Math.max(1, Math.ceil(data.total / pageSize)) : 1;

  return (
    <div className="space-y-6">
      <PageHeader icon={Users} title={t("users_title")} subtitle={t("users_subtitle")} actions={<CreateUserDialog onCreated={(id) => { reload(); navigate(`/admin/users/${id}`); }} />} />

      {/* Filters */}
      <Card>
        <CardContent className="p-3">
          <form onSubmit={handleSearch} className="flex flex-wrap items-center gap-2">
            <div className="relative min-w-[200px] flex-1">
              <Search className="absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                placeholder={t("users_search_placeholder")}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                className="pl-8 h-9 text-sm"
              />
            </div>
            <Select value={roleFilter} onValueChange={(v) => { setRoleFilter(v); setPage(0); }}>
              <SelectTrigger className="h-9 w-[120px] text-sm" aria-label={t("users_filter_role")}>
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
              <SelectTrigger className="h-9 w-[130px] text-sm" aria-label={t("users_filter_status")}>
                <SelectValue placeholder={t("users_filter_status_all")} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="">{t("users_filter_status_all")}</SelectItem>
                <SelectItem value="false">{t("users_status_active")}</SelectItem>
                <SelectItem value="true">{t("users_status_disabled")}</SelectItem>
              </SelectContent>
            </Select>
            <Button type="submit" variant="outline" size="sm" className="h-9 text-sm">
              {t("users_search_btn")}
            </Button>
          </form>
        </CardContent>
      </Card>

      {/* Table */}
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
          <Card>
            <CardContent className="p-0">
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border bg-muted/30 text-left">
                      <th className="px-4 py-3 font-medium text-muted-foreground">{t("users_col_user")}</th>
                      <th className="px-4 py-3 font-medium text-muted-foreground hidden sm:table-cell">{t("users_col_role")}</th>
                      <th className="px-4 py-3 font-medium text-muted-foreground hidden md:table-cell">{t("users_col_servers")}</th>
                      <th className="px-4 py-3 font-medium text-muted-foreground hidden lg:table-cell">{t("users_col_status")}</th>
                      <th className="px-4 py-3 font-medium text-muted-foreground hidden lg:table-cell">{t("users_col_created")}</th>
                      <th className="px-4 py-3 font-medium text-muted-foreground w-0" />
                    </tr>
                  </thead>
                  <tbody>
                    {data.users.map((u: UserView) => (
                      <UserRow key={u.id} user={u} locale={locale} />
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

function UserRow({ user, locale }: { user: UserView; locale: string }) {
  const { t } = useTranslation("admin");

  return (
    <tr
      className={cn(
        "border-b border-border/50 hover:bg-muted/20 transition-colors",
        user.disabled && "opacity-60",
      )}
    >
      <td className="px-4 py-3">
        <Link
          to={`/admin/users/${user.id}`}
          className="font-medium text-foreground hover:text-primary hover:underline"
        >
          {user.username}
        </Link>
        {user.email && (
          <div className="flex items-center gap-1 mt-0.5 text-xs text-muted-foreground">
            <Mail className="h-3 w-3" />
            {user.email}
          </div>
        )}
      </td>
      <td className="px-4 py-3 hidden sm:table-cell">
        <RoleBadge role={user.role} />
      </td>
      <td className="px-4 py-3 hidden md:table-cell">
        <span className="inline-flex items-center gap-1 text-muted-foreground">
          <Server className="h-3.5 w-3.5" />
          {user.server_count}
        </span>
      </td>
      <td className="px-4 py-3 hidden lg:table-cell">
        <UserStatusBadge disabled={user.disabled} />
      </td>
      <td className="px-4 py-3 text-muted-foreground hidden lg:table-cell text-xs">
        {formatAbsolute(user.created_at, locale)}
      </td>
      <td className="px-4 py-3 text-right">
        <Button
          variant="outline"
          size="sm"
          className="h-8 gap-1 text-xs"
          asChild
        >
          <Link to={`/admin/users/${user.id}`}>
            {t("users_view_detail")}
            <ChevronRight className="h-3.5 w-3.5 opacity-60" />
          </Link>
        </Button>
      </td>
    </tr>
  );
}
