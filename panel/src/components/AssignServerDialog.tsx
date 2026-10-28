import { useEffect, useMemo, useState, type ReactNode } from "react";
import { AlertTriangle, Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { InlineError } from "@/components/MessageLine";
import { useToast } from "@/components/Toaster";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { cn } from "@/lib/utils";
import type { QuotaExcess, UserView } from "@/lib/types";

/** owner is the current owner as the fleet names it (see userIdentity); empty when unclaimed or unknown. */
export interface AssignServer {
  name: string;
  label: string;
  owner: string;
}

export interface AssignUser {
  id: string;
  label: string;
  identity: string;
}

/** userIdentity is how the fleet names an account as an owner: its email, or its username without one. */
export function userIdentity(user: Pick<UserView, "username" | "email">): string {
  return user.email || user.username;
}

const SEARCH_DEBOUNCE_MS = 300;
const USERS_SHOWN = 50;

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The entry point fixes one side; the dialog lets the person pick the other. */
  server?: AssignServer;
  user?: AssignUser;
  onAssigned: () => void;
}

export function AssignServerDialog({ open, onOpenChange, server, user, onAssigned }: Props) {
  const { t } = useTranslation("admin");
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("assign_title")}</DialogTitle>
          <DialogDescription>{t("assign_desc")}</DialogDescription>
        </DialogHeader>
        <AssignForm server={server} user={user} onClose={() => onOpenChange(false)} onAssigned={onAssigned} />
      </DialogContent>
    </Dialog>
  );
}

function AssignForm({
  server,
  user,
  onClose,
  onAssigned,
}: {
  server?: AssignServer;
  user?: AssignUser;
  onClose: () => void;
  onAssigned: () => void;
}) {
  const { t } = useTranslation("admin");
  const toast = useToast();
  const [pickedServer, setPickedServer] = useState<AssignServer | null>(null);
  const [pickedUser, setPickedUser] = useState<AssignUser | null>(null);
  const [allowOver, setAllowOver] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const target = server ?? pickedServer;
  const assignee = user ?? pickedUser;

  const preview = useAsync(
    () => (target && assignee ? api.previewServerAssignment(target.name, assignee.id) : Promise.resolve(null)),
    [target?.name, assignee?.id],
  );
  const over = preview.data?.over ?? [];

  function pick<T>(set: (value: T) => void, value: T) {
    set(value);
    setAllowOver(false);
    setError(null);
  }

  async function assign() {
    if (!target || !assignee || !preview.data) return;
    setBusy(true);
    setError(null);
    try {
      await api.assignServer(target.name, { user_id: assignee.id, allow_over_quota: over.length > 0 && allowOver });
      toast.success(t("assign_done", { server: target.label, user: assignee.label }));
      onClose();
      onAssigned();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }

  const round = (n: number) => Number(n.toFixed(2));
  const amount = (resource: QuotaExcess["resource"], n: number) => {
    if (resource === "servers") return String(n);
    if (resource === "cpu_milli") return t("user_quota_cores", { n: round(n / 1000) });
    return t("user_quota_gb", { n: round(n / 1024) });
  };
  const from = preview.data ? target?.owner || preview.data.owner_id : "";

  return (
    <div className="grid gap-4">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        {server && (
          <>
            <dt className="text-muted-foreground">{t("assign_server_label")}</dt>
            <dd className="min-w-0 truncate font-medium">{server.label}</dd>
          </>
        )}
        {user && (
          <>
            <dt className="text-muted-foreground">{t("assign_user_label")}</dt>
            <dd className="min-w-0 truncate font-medium">{user.label}</dd>
          </>
        )}
      </dl>

      {user && !server && (
        <ServerPicker user={user} picked={pickedServer} onPick={(s) => pick(setPickedServer, s)} />
      )}
      {server && !user && (
        <UserPicker server={server} picked={pickedUser} onPick={(u) => pick(setPickedUser, u)} />
      )}

      {target && assignee && preview.loading && (
        <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          {t("assign_checking")}
        </p>
      )}
      {!!preview.error && <InlineError message={humanizeError(preview.error)} />}
      {target && assignee && preview.data && (
        <p className="text-sm">
          {preview.data.owner_id
            ? t("assign_change", { server: target.label, from, to: assignee.label })
            : t("assign_from_unclaimed", { server: target.label, to: assignee.label })}
        </p>
      )}
      {over.length > 0 && (
        <div className="grid gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs">
          <p className="flex items-start gap-1.5 font-medium text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
            {t("assign_over_title")}
          </p>
          <ul className="list-disc space-y-0.5 pl-9 text-foreground">
            {over.map((o) => (
              <li key={o.resource}>
                {t(`assign_over_${o.resource}`, { after: amount(o.resource, o.after), limit: amount(o.resource, o.limit) })}
              </li>
            ))}
          </ul>
          <label className="flex cursor-pointer items-center gap-2 font-medium text-foreground">
            <Checkbox className="mt-0.5" checked={allowOver} onChange={(e) => setAllowOver(e.target.checked)} />
            {t("assign_allow_over")}
          </label>
        </div>
      )}
      <InlineError message={error} />

      <ConfirmFooter
        onCancel={onClose}
        onConfirm={() => void assign()}
        loading={busy}
        disabled={!preview.data || (over.length > 0 && !allowOver)}
        cancelLabel={t("common:cancel")}
        confirmLabel={t("assign_confirm")}
        confirmVariant="default"
      />
    </div>
  );
}

function ServerPicker({ user, picked, onPick }: { user: AssignUser; picked: AssignServer | null; onPick: (s: AssignServer) => void }) {
  const { t } = useTranslation("admin");
  const fleet = useAsync(api.fleet, []);
  const [filter, setFilter] = useState("");
  const rows = useMemo(() => {
    const terms = filter.trim().toLowerCase();
    return (fleet.data ?? []).filter(
      (s) =>
        !s.system &&
        !s.retiring &&
        !(s.owner && s.owner === user.identity) &&
        (terms === "" || s.name.toLowerCase().includes(terms) || (s.displayName ?? "").toLowerCase().includes(terms)),
    );
  }, [fleet.data, filter, user.identity]);

  return (
    <Picker
      id="assign-find-server"
      label={t("assign_find_server")}
      filter={filter}
      onFilter={setFilter}
      loading={fleet.loading && !fleet.data}
      error={fleet.error}
      emptyText={t("assign_no_servers")}
      empty={rows.length === 0}
    >
      {rows.map((s) => {
        const label = s.displayName || s.name;
        return (
          <PickRow
            key={s.name}
            selected={picked?.name === s.name}
            onClick={() => onPick({ name: s.name, label, owner: s.owner ?? "" })}
            title={label}
            detail={[label !== s.name ? s.name : "", s.owner || t("ops:fleet_unclaimed")].filter(Boolean).join(" · ")}
          />
        );
      })}
    </Picker>
  );
}

function UserPicker({ server, picked, onPick }: { server: AssignServer; picked: AssignUser | null; onPick: (u: AssignUser) => void }) {
  const { t } = useTranslation("admin");
  const [filter, setFilter] = useState("");
  const [search, setSearch] = useState("");
  useEffect(() => {
    const next = filter.trim();
    if (next === search) return;
    const timer = setTimeout(() => setSearch(next), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [filter, search]);
  const users = useAsync(
    () => api.listUsers({ query: search || undefined, disabled: "false", limit: USERS_SHOWN }),
    [search],
    { keepPrevious: true },
  );
  const rows = (users.data?.users ?? []).filter((u) => !u.disabled && userIdentity(u) !== server.owner);

  return (
    <Picker
      id="assign-find-user"
      label={t("assign_find_user")}
      filter={filter}
      onFilter={setFilter}
      loading={users.loading && !users.data}
      error={users.error}
      emptyText={t("assign_no_users")}
      empty={rows.length === 0}
    >
      {rows.map((u) => (
        <PickRow
          key={u.id}
          selected={picked?.id === u.id}
          onClick={() => onPick({ id: u.id, label: u.username, identity: userIdentity(u) })}
          title={u.username}
          detail={u.email ?? ""}
        />
      ))}
    </Picker>
  );
}

function Picker({
  id,
  label,
  filter,
  onFilter,
  loading,
  error,
  empty,
  emptyText,
  children,
}: {
  id: string;
  label: string;
  filter: string;
  onFilter: (value: string) => void;
  loading: boolean;
  error: unknown;
  empty: boolean;
  emptyText: string;
  children: ReactNode;
}) {
  const { t } = useTranslation("common");
  let body: ReactNode;
  if (error) {
    body = <InlineError message={humanizeError(error)} />;
  } else if (loading) {
    body = (
      <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        {t("loading")}
      </p>
    );
  } else if (empty) {
    body = <p className="text-sm text-muted-foreground">{emptyText}</p>;
  } else {
    body = <ul className="max-h-56 space-y-1 overflow-y-auto">{children}</ul>;
  }
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} value={filter} autoComplete="off" onChange={(e) => onFilter(e.target.value)} />
      {body}
    </div>
  );
}

function PickRow({ selected, onClick, title, detail }: { selected: boolean; onClick: () => void; title: string; detail: string }) {
  return (
    <li>
      <button
        type="button"
        aria-pressed={selected}
        onClick={onClick}
        className={cn(
          "flex w-full min-w-0 items-baseline justify-between gap-3 rounded-md border px-3 py-2 text-left text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          selected ? "border-primary bg-primary/5" : "border-border hover:bg-accent",
        )}
      >
        <span className="truncate font-medium">{title}</span>
        <span className="truncate text-xs text-muted-foreground">{detail}</span>
      </button>
    </li>
  );
}
