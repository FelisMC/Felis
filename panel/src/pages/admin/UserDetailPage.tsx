import { useState, useEffect } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { MessageLine, InlineError } from "@/components/MessageLine";
import { RoleBadge } from "@/components/RoleBadge";
import { UserStatusBadge } from "@/components/UserStatusBadge";
import {
  UserRound,
  Mail,
  Calendar,
  Clock,
  Trash2,
  Power,
  PowerOff,
  Save,
  Loader2,
  CheckCircle2,
  RefreshCw,
  Unlink,
  Link,
  X,
  AlertTriangle,
  Fingerprint,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { BackLink } from "@/components/BackLink";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Loading, ErrorState } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { formatAbsolute } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { UserDetail, SessionView } from "@/lib/types";

export function UserDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const { identity } = useTier();

  const { data: user, error, loading, reload } = useAsync(
    () => api.getUser(id!),
    [id],
  );

  if (loading && !user) return <Loading />;
  if (error) return <ErrorState error={error} onRetry={reload} />;
  if (!user) return <ErrorState error={new Error("user not found")} />;

  return (
    <div className="space-y-6">
      {/* Back link */}
      <BackLink to="/admin/users" label={t("users_back_to_list")} />

      <PageHeader
        icon={(
          <div className={cn(
            "rounded-full p-2",
            user.role === "admin" ? "bg-primary/10 text-primary" : "bg-muted text-muted-foreground",
          )}>
            <UserRound className="h-6 w-6" />
          </div>
        )}
        title={user.username}
        subtitle={(
          <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
            {user.email && (
              <span className="inline-flex items-center gap-1">
                <Mail className="h-3.5 w-3.5" />
                {user.email}
                {user.email_verified && (
                  <CheckCircle2 className="h-3 w-3 text-emerald-500" />
                )}
              </span>
            )}
            <span className="inline-flex items-center gap-1">
              <Calendar className="h-3.5 w-3.5" />
              {formatAbsolute(user.created_at, locale)}
            </span>
          </div>
        )}
        actions={(
          <div className="flex items-center gap-2">
            <UserStatusBadge disabled={user.disabled} />
            <RoleBadge role={user.role} />
          </div>
        )}
        className="mb-6"
      />

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        {/* Edit profile */}
        <EditProfileCard user={user} onSaved={reload} isSelf={identity?.user_id === id} />
        {/* Linked accounts */}
        <LinkedAccountsCard user={user} onChanged={reload} />
        {/* Quotas */}
        <QuotasCard userId={user.id} />
        {/* Sessions */}
        <SessionsCard userId={user.id} onChanged={reload} />
      </div>

      {/* Danger zone */}
      <DangerZone user={user} onChanged={reload} navigate={navigate} />
    </div>
  );
}

function EditProfileCard({ user, onSaved, isSelf }: { user: UserDetail; onSaved: () => void; isSelf: boolean }) {
  const { t } = useTranslation("admin");
  const [username, setUsername] = useState(user.username);
  const [email, setEmail] = useState(user.email ?? "");
  const [role, setRole] = useState(user.role);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);

  const dirty = username !== user.username || email !== (user.email ?? "") || role !== user.role;

  async function handleSave() {
    if (!dirty) return;
    setSaving(true);
    setErr(null);
    setOk(null);
    try {
      const patch: any = {};
      if (username !== user.username) patch.username = username;
      if (email !== (user.email ?? "")) patch.email = email;
      if (role !== user.role) patch.role = role;
      await api.patchUser(user.id, patch);
      setOk(t("users_save_ok"));
      onSaved();
    } catch (e: any) {
      if (e && e.code === "already_exists") {
        setErr(t("users_create_validation_username_taken"));
      } else {
        setErr(humanizeError(e));
      }
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base font-semibold">{t("users_edit_profile")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="user-detail-username" className="text-xs font-semibold text-muted-foreground">{t("users_field_username")}</Label>
          <Input
            id="user-detail-username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            className="h-9 text-sm"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="user-detail-email" className="text-xs font-semibold text-muted-foreground">{t("users_field_email")}</Label>
          <Input
            id="user-detail-email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="user@example.com"
            className="h-9 text-sm"
          />
        </div>
        {!isSelf && (
          <div className="space-y-1.5">
            <Label htmlFor="user-detail-role" className="text-xs font-semibold text-muted-foreground">{t("users_field_role")}</Label>
            <Select value={role} onValueChange={(v: "admin" | "user") => setRole(v)}>
              <SelectTrigger id="user-detail-role" className="h-9 text-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="user">{t("users_role_user")}</SelectItem>
                <SelectItem value="admin">{t("users_role_admin")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
        )}

        {err && (
          <MessageLine kind="error" message={err} />
        )}
        {ok && (
          <MessageLine kind="success" message={ok} />
        )}

        <Button
          onClick={handleSave}
          disabled={!dirty || saving}
          size="sm"
          className="gap-1.5"
        >
          {saving ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <Save className="h-4 w-4" />
          )}
          {t("users_save_btn")}
        </Button>
      </CardContent>
    </Card>
  );
}

function LinkedAccountsCard({ user, onChanged }: { user: UserDetail; onChanged: () => void }) {
  const { t } = useTranslation("admin");
  const accounts = user.linked_accounts ?? [];
  const [unlinking, setUnlinking] = useState<string | null>(null);
  const [unlinkDlg, setUnlinkDlg] = useState<string | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [newUUID, setNewUUID] = useState("");
  const [newSource, setNewSource] = useState("mojang");
  const [adding, setAdding] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function handleUnlinkConfirm() {
    if (!unlinkDlg) return;
    const mcUuid = unlinkDlg;
    setUnlinking(mcUuid);
    setErr(null);
    try {
      await api.unlinkAccount(user.id, mcUuid);
      setUnlinkDlg(null);
      onChanged();
    } catch (e) {
      setErr(humanizeError(e));
    } finally {
      setUnlinking(null);
    }
  }

  async function handleAdd(e: React.FormEvent) {
    e.preventDefault();
    if (!newUUID.trim()) return;
    setAdding(true);
    setErr(null);
    try {
      await api.linkAccount(user.id, newUUID.trim(), newSource);
      setNewUUID("");
      setShowAdd(false);
      onChanged();
    } catch (e) {
      setErr(humanizeError(e));
    } finally {
      setAdding(false);
    }
  }

  return (
    <>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="text-base font-semibold">
            {t("users_linked_accounts")} ({accounts.length})
        </CardTitle>
        <Button
          variant="outline"
          size="sm"
          className="gap-1 text-xs"
          onClick={() => setShowAdd(!showAdd)}
        >
          <Link className="h-3.5 w-3.5" />
          {showAdd ? t("common:cancel") : t("users_link_add")}
        </Button>
      </CardHeader>
      <CardContent className="space-y-3">
        {/* Add form */}
        {showAdd && (
          <form onSubmit={handleAdd} className="space-y-3 rounded-md border border-border/50 bg-muted/20 p-3">
            <div className="flex gap-2">
              <div className="flex-1 space-y-1.5">
                <Label htmlFor="user-detail-uuid" className="text-[11px] font-semibold text-muted-foreground">Minecraft UUID</Label>
                <Input
                  id="user-detail-uuid"
                  value={newUUID}
                  onChange={(e) => setNewUUID(e.target.value)}
                  placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
                  className="h-8 text-xs font-mono"
                />
              </div>
              <div className="w-28 space-y-1.5">
                <Label htmlFor="user-detail-link-source" className="text-[11px] font-semibold text-muted-foreground">{t("users_link_source")}</Label>
                <Select value={newSource} onValueChange={setNewSource}>
                  <SelectTrigger id="user-detail-link-source" className="h-8 text-xs">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                     <SelectItem value="mojang">Mojang</SelectItem>
                    <SelectItem value="thirdparty">Third-party Yggdrasil</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            <InlineError message={err} className="text-xs" />
            <Button type="submit" size="sm" disabled={adding || !newUUID.trim()} className="h-8 text-xs gap-1">
              {adding ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Link className="h-3.5 w-3.5" />}
              {t("users_link_confirm")}
            </Button>
          </form>
        )}

        {accounts.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("users_no_linked")}</p>
        ) : (
          <div className="space-y-2">
            {accounts.map((acc) => (
              <div key={acc.mc_uuid} className="flex items-center justify-between rounded-md border border-border/50 bg-muted/20 pl-3 pr-1 py-2 text-sm">
                <div className="min-w-0 flex-1">
                  <span className="font-mono text-xs truncate block">{acc.mc_uuid}</span>
                  <div className="mt-0.5 text-xs text-muted-foreground">
                    {acc.auth_source} &middot; {formatAbsolute(acc.verified_at, "en-US")}
                  </div>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 w-7 p-0 text-muted-foreground hover:text-destructive shrink-0 mr-0.5"
                   disabled={unlinking === acc.mc_uuid}
                  onClick={() => setUnlinkDlg(acc.mc_uuid)}
                  aria-label={t("users_unlink_tooltip")}
                  title={t("users_unlink_tooltip")}
                >
                  {unlinking === acc.mc_uuid ? (
                    <Loader2 className="h-3.5 w-3.5 animate-spin" />
                  ) : (
                    <Unlink className="h-3.5 w-3.5" />
                  )}
                </Button>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>

    {/* Unlink confirmation dialog */}
    <Dialog open={!!unlinkDlg} onOpenChange={() => setUnlinkDlg(null)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("users_unlink_dlg_title")}</DialogTitle>
          <DialogDescription>{t("users_unlink_dlg_desc")}</DialogDescription>
        </DialogHeader>
        <ConfirmFooter onCancel={() => setUnlinkDlg(null)} onConfirm={handleUnlinkConfirm} cancelLabel={t("common:cancel")} confirmLabel={t("users_unlink_btn")} />
      </DialogContent>
    </Dialog>
    </>
  );
}

function QuotasCard({ userId }: { userId: string }) {
  const { t } = useTranslation("admin");
  const { data: quotas, error, loading, reload } = useAsync(
    () => api.getUserQuotas(userId),
    [userId],
  );

  const [maxServers, setMaxServers] = useState<string>("");
  const [maxCpu, setMaxCpu] = useState<string>("");
  const [maxMem, setMaxMem] = useState<string>("");
  const [maxStorage, setMaxStorage] = useState<string>("");
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);

  useEffect(() => {
    if (quotas) {
      setMaxServers(quotas.max_servers != null ? String(quotas.max_servers) : "");
      setMaxCpu(quotas.max_cpu_milli != null ? String(quotas.max_cpu_milli) : "");
      setMaxMem(quotas.max_memory_mb != null ? String(quotas.max_memory_mb) : "");
      setMaxStorage(quotas.max_storage_gb != null ? String(quotas.max_storage_gb) : "");
    }
  }, [quotas]);

  async function handleSave() {
    setSaving(true);
    setErr(null);
    setOk(null);
    try {
      const toNum = (s: string) => (s === "" ? null : parseInt(s, 10));
      await api.setUserQuotas(userId, {
        max_servers: toNum(maxServers),
        max_cpu_milli: toNum(maxCpu),
        max_memory_mb: toNum(maxMem),
        max_storage_gb: toNum(maxStorage),
      });
      setOk(t("users_save_ok"));
      reload();
    } catch (e) {
      setErr(humanizeError(e));
    } finally {
      setSaving(false);
    }
  }

  if (loading && !quotas) return <Loading label={t("common:loading")} />;
  if (error) return (
    <Card>
      <CardHeader><CardTitle className="text-base font-semibold">{t("users_quotas")}</CardTitle></CardHeader>
      <CardContent><ErrorState error={error} onRetry={reload} /></CardContent>
    </Card>
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base font-semibold">{t("users_quotas")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1.5">
            <Label htmlFor="user-detail-quota-servers" className="text-xs font-semibold text-muted-foreground">{t("users_quota_servers")}</Label>
            <Input
              id="user-detail-quota-servers"
              type="number"
              value={maxServers}
              onChange={(e) => setMaxServers(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="user-detail-quota-cpu" className="text-xs font-semibold text-muted-foreground">{t("users_quota_cpu")}</Label>
            <Input
              id="user-detail-quota-cpu"
              type="number"
              value={maxCpu}
              onChange={(e) => setMaxCpu(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="user-detail-quota-memory" className="text-xs font-semibold text-muted-foreground">{t("users_quota_memory")}</Label>
            <Input
              id="user-detail-quota-memory"
              type="number"
              value={maxMem}
              onChange={(e) => setMaxMem(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="user-detail-quota-storage" className="text-xs font-semibold text-muted-foreground">{t("users_quota_storage")}</Label>
            <Input
              id="user-detail-quota-storage"
              type="number"
              value={maxStorage}
              onChange={(e) => setMaxStorage(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
        </div>

        {err && (
          <MessageLine kind="error" message={err} />
        )}
        {ok && (
          <MessageLine kind="success" message={ok} />
        )}

        <Button onClick={handleSave} disabled={saving} size="sm" className="gap-1.5">
          {saving ? <Loader2 className="h-4 w-4 animate-spin" /> : <Save className="h-4 w-4" />}
          {t("users_save_btn")}
        </Button>
      </CardContent>
    </Card>
  );
}

function SessionsCard({ userId, onChanged }: { userId: string; onChanged: () => void }) {
  const { t } = useTranslation("admin");
  const { data: sessions, error, loading, reload } = useAsync(
    () => api.listUserSessions(userId),
    [userId],
  );
  const [revoking, setRevoking] = useState<string | null>(null);
  const [revokingAll, setRevokingAll] = useState(false);
  const [revokeOneDlg, setRevokeOneDlg] = useState<string | null>(null);
  const [revokeAllDlg, setRevokeAllDlg] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);

  async function handleRevokeOne() {
    if (!revokeOneDlg) return;
    const hash = revokeOneDlg;
    setRevoking(hash);
    setErr(null);
    try {
      await api.revokeUserSession(userId, hash);
      setRevokeOneDlg(null);
      await reload();
      onChanged();
    } catch (e) {
      setErr(humanizeError(e));
    } finally {
      setRevoking(null);
    }
  }

  async function handleRevokeAll() {
    setRevokingAll(true);
    setErr(null);
    setOk(null);
    try {
      await api.revokeUserSessions(userId);
      setRevokeAllDlg(false);
      setOk(t("users_sessions_revoked"));
      await reload();
      onChanged();
    } catch (e) {
      setErr(humanizeError(e));
    } finally {
      setRevokingAll(false);
    }
  }

  return (
    <>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="text-base font-semibold">
            {t("users_sessions")} ({sessions?.length ?? 0})
        </CardTitle>
        <Button
          variant="outline"
          size="sm"
          className="gap-1 text-xs text-destructive hover:text-destructive hover:bg-destructive/10"
          disabled={!sessions || sessions.length === 0 || revokingAll}
          onClick={() => setRevokeAllDlg(true)}
        >
          {revokingAll ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" />
          ) : (
            <RefreshCw className="h-3.5 w-3.5" />
          )}
          {t("users_sessions_revoke_all")}
        </Button>
      </CardHeader>
      <CardContent>
        {err && (
          <MessageLine kind="error" message={err} />
        )}
        {ok && (
          <MessageLine kind="success" message={ok} />
        )}
        {loading ? (
          <Loading label="" />
        ) : error ? (
          <ErrorState error={error} onRetry={reload} />
        ) : !sessions || sessions.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("users_no_sessions")}</p>
        ) : (
          <div className="space-y-2 max-h-[350px] overflow-y-auto">
            {sessions.map((s: SessionView) => (
              <div
                key={s.token_hash}
                className="flex items-center justify-between rounded-md border border-border/50 bg-muted/20 pl-3 pr-1 py-2 text-xs"
              >
                <div className="min-w-0 flex-1">
                  <span className="font-mono text-[11px] text-muted-foreground truncate block">
                    {s.token_hash.slice(0, 20)}...
                  </span>
                  <div className="mt-0.5 text-muted-foreground/70">
                    <Clock className="inline h-3 w-3 mr-0.5" />
                    {t("users_session_expires")}: {formatAbsolute(s.expires_at, "en-US")}
                  </div>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 w-7 p-0 text-muted-foreground hover:text-destructive shrink-0 ml-2 mr-0.5"
                  disabled={revoking === s.token_hash}
                  onClick={() => setRevokeOneDlg(s.token_hash)}
                  aria-label={t("users_session_revoke_one")}
                  title={t("users_session_revoke_one")}
                >
                  {revoking === s.token_hash ? (
                    <Loader2 className="h-3.5 w-3.5 animate-spin" />
                  ) : (
                    <X className="h-3.5 w-3.5" />
                  )}
                </Button>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>

    {/* Revoke one confirmation dialog */}
    <Dialog open={!!revokeOneDlg} onOpenChange={() => setRevokeOneDlg(null)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("users_session_revoke_one_dlg_title")}</DialogTitle>
          <DialogDescription>{t("users_session_revoke_one_dlg_desc")}</DialogDescription>
        </DialogHeader>
        <ConfirmFooter onCancel={() => setRevokeOneDlg(null)} onConfirm={handleRevokeOne} cancelLabel={t("common:cancel")} confirmLabel={t("users_session_revoke_confirm")} />
      </DialogContent>
    </Dialog>

    {/* Revoke all confirmation dialog */}
    <Dialog open={revokeAllDlg} onOpenChange={setRevokeAllDlg}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("users_session_revoke_all_dlg_title")}</DialogTitle>
          <DialogDescription>{t("users_session_revoke_all_dlg_desc")}</DialogDescription>
        </DialogHeader>
        <ConfirmFooter onCancel={() => setRevokeAllDlg(false)} onConfirm={handleRevokeAll} cancelLabel={t("common:cancel")} confirmLabel={t("users_session_revoke_confirm")} />
      </DialogContent>
    </Dialog>
    </>
  );
}

function DangerZone({
  user,
  onChanged,
  navigate,
}: {
  user: UserDetail;
  onChanged: () => void;
  navigate: (path: string) => void;
}) {
  const { t } = useTranslation("admin");
  const [dlg, setDlg] = useState<"disable" | "delete" | "passkeys" | null>(null);

  return (
    <Card className="border-destructive/30">
      <CardHeader>
        <CardTitle className="text-base font-semibold text-destructive">{t("users_danger_zone")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-5">
        {/* Enable / Disable */}
        <DangerRow
          icon={user.disabled ? Power : PowerOff}
          title={user.disabled ? t("users_danger_enable") : t("users_danger_disable")}
          desc={user.disabled ? t("users_danger_enable_desc") : t("users_danger_disable_desc")}
          btnLabel={user.disabled ? t("users_danger_enable_btn") : t("users_danger_disable_btn")}
          btnVariant={user.disabled ? "default" : "destructive"}
          onAction={() => setDlg("disable")}
        />

        {/* Delete user */}
        <DangerRow
          icon={Trash2}
          title={t("users_danger_delete")}
          desc={t("users_danger_delete_desc")}
          btnLabel={t("users_danger_delete_btn")}
          btnVariant="destructive"
          onAction={() => setDlg("delete")}
        />

        {/* Unbind passkeys — credential remediation, not a lockout */}
        <DangerRow
          icon={Fingerprint}
          title={t("users_danger_passkeys")}
          desc={t("users_danger_passkeys_desc")}
          btnLabel={t("users_danger_passkeys_btn")}
          btnVariant="outline"
          onAction={() => setDlg("passkeys")}
        />

        <DangerDialogs dlg={dlg} setDlg={setDlg} user={user} onChanged={onChanged} navigate={navigate} />
      </CardContent>
    </Card>
  );
}

function DangerRow({
  icon: Icon,
  title,
  desc,
  btnLabel,
  btnVariant,
  onAction,
}: {
  icon: typeof Power;
  title: string;
  desc: string;
  btnLabel: string;
  btnVariant: "default" | "destructive" | "outline";
  onAction: () => void;
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-border/50 bg-muted/20 p-4">
      <div>
        <p className="text-sm font-medium">{title}</p>
        <p className="text-xs text-muted-foreground mt-0.5">{desc}</p>
      </div>
      <Button variant={btnVariant} size="sm" onClick={onAction} className="gap-1.5">
        <Icon className="h-4 w-4" />
        {btnLabel}
      </Button>
    </div>
  );
}

function DangerDialogs({
  dlg,
  setDlg,
  user,
  onChanged,
  navigate,
}: {
  dlg: "disable" | "delete" | "passkeys" | null;
  setDlg: (v: null) => void;
  user: UserDetail;
  onChanged: () => void;
  navigate: (path: string) => void;
}) {
  const { t } = useTranslation("admin");
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  function close() {
    setDlg(null);
    setErr(null);
    setLoading(false);
  }

  async function handleDisable() {
    setLoading(true);
    setErr(null);
    try {
      await api.disableUser(user.id, !user.disabled);
      close();
      onChanged();
    } catch (e) {
      setErr(humanizeError(e));
      setLoading(false);
    }
  }

  async function handleDelete() {
    setLoading(true);
    setErr(null);
    try {
      await api.deleteUser(user.id);
      navigate("/admin/users");
    } catch (e) {
      setErr(humanizeError(e));
      setLoading(false);
    }
  }

  async function handleUnbindPasskeys() {
    setLoading(true);
    setErr(null);
    try {
      await api.unbindUserPasskeys(user.id);
      close();
      onChanged();
    } catch (e) {
      setErr(humanizeError(e));
      setLoading(false);
    }
  }

  return (
    <>
      {/* Disable / Enable dialog */}
      <Dialog open={dlg === "disable"} onOpenChange={(v) => { if (!v) close(); }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2 text-destructive">
              <AlertTriangle className="h-5 w-5" />
              {user.disabled ? t("users_danger_enable_dlg_title") : t("users_danger_disable_dlg_title")}
            </DialogTitle>
            <DialogDescription>
              {user.disabled ? t("users_danger_enable_dlg_desc") : t("users_danger_disable_dlg_desc")}
            </DialogDescription>
          </DialogHeader>
          <InlineError message={err} />
          <ConfirmFooter onCancel={close} onConfirm={handleDisable} loading={loading} cancelLabel={t("common:cancel")} confirmLabel={user.disabled ? t("users_danger_enable_btn") : t("users_danger_disable_btn")} confirmVariant={user.disabled ? "default" : "destructive"} />
        </DialogContent>
      </Dialog>

      {/* Delete user dialog */}
      <Dialog open={dlg === "delete"} onOpenChange={(v) => { if (!v) close(); }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2 text-destructive">
              <AlertTriangle className="h-5 w-5" />
              {t("users_danger_delete_dlg_title")}
            </DialogTitle>
            <DialogDescription>
              {t("users_danger_delete_dlg_desc")}
            </DialogDescription>
          </DialogHeader>
          <InlineError message={err} />
          <ConfirmFooter onCancel={close} onConfirm={handleDelete} loading={loading} cancelLabel={t("common:cancel")} confirmLabel={t("users_danger_delete_yes")} />
        </DialogContent>
      </Dialog>

      {/* Unbind passkeys dialog */}
      <Dialog open={dlg === "passkeys"} onOpenChange={(v) => { if (!v) close(); }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2 text-destructive">
              <AlertTriangle className="h-5 w-5" />
              {t("users_danger_passkeys_dlg_title")}
            </DialogTitle>
            <DialogDescription>
              {t("users_danger_passkeys_dlg_desc")}
            </DialogDescription>
          </DialogHeader>
          <InlineError message={err} />
          <ConfirmFooter onCancel={close} onConfirm={handleUnbindPasskeys} loading={loading} cancelLabel={t("common:cancel")} confirmLabel={t("users_danger_passkeys_yes")} />
        </DialogContent>
      </Dialog>
    </>
  );
}
