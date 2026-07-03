import { useState, useEffect } from "react";
import { useParams, useNavigate } from "react-router-dom";
import {
  ArrowLeft,
  UserRound,
  Mail,
  Shield,
  Crown,
  Calendar,
  Circle,
  Key,
  Clock,
  Trash2,
  Power,
  PowerOff,
  Save,
  Loader2,
  AlertCircle,
  CheckCircle2,
  RefreshCw,
  Unlink,
  Link,
  X,
  AlertTriangle,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Loading, ErrorState } from "@/components/States";
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
  const isSelf = identity?.user_id === id;

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
      <button
        onClick={() => navigate("/admin/users")}
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors"
      >
        <ArrowLeft className="h-4 w-4" />
        {t("users_back_to_list")}
      </button>

      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <div className={cn(
            "rounded-full p-2",
            user.role === "admin" ? "bg-primary/10 text-primary" : "bg-muted text-muted-foreground",
          )}>
            <UserRound className="h-6 w-6" />
          </div>
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{user.username}</h1>
            <div className="flex flex-wrap items-center gap-2 mt-1 text-sm text-muted-foreground">
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
          </div>
        </div>
        <div className="flex items-center gap-2">
          {user.disabled ? (
            <span className="inline-flex items-center gap-1 rounded-full bg-destructive/10 px-3 py-1 text-xs font-medium text-destructive">
              <Circle className="h-2 w-2 fill-destructive" />
              {t("users_status_disabled")}
            </span>
          ) : (
            <span className="inline-flex items-center gap-1 rounded-full bg-emerald-500/10 px-3 py-1 text-xs font-medium text-emerald-500">
              <Circle className="h-2 w-2 fill-emerald-500" />
              {t("users_status_active")}
            </span>
          )}
          <span className={cn(
            "inline-flex items-center gap-1 rounded-full px-3 py-1 text-xs font-medium",
            user.role === "owner"
              ? "bg-yellow-500/10 text-yellow-600"
              : user.role === "admin"
              ? "bg-primary/10 text-primary"
              : "bg-muted text-muted-foreground",
          )}>
            {user.role === "owner" ? (
              <Crown className="h-3 w-3" />
            ) : (
              <Shield className="h-3 w-3" />
            )}
            {user.role === "owner" ? t("users_role_owner") : user.role === "admin" ? t("users_role_admin") : t("users_role_user")}
          </span>
        </div>
      </div>

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
    } catch (e) {
      setErr(humanizeError(e));
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
          <Label className="text-xs font-semibold text-muted-foreground">{t("users_field_username")}</Label>
          <Input
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            className="h-9 text-sm"
          />
        </div>
        <div className="space-y-1.5">
          <Label className="text-xs font-semibold text-muted-foreground">{t("users_field_email")}</Label>
          <Input
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="user@example.com"
            className="h-9 text-sm"
          />
        </div>
        {!isSelf && (
          <div className="space-y-1.5">
            <Label className="text-xs font-semibold text-muted-foreground">{t("users_field_role")}</Label>
            <Select value={role} onValueChange={(v: "admin" | "user") => setRole(v)}>
              <SelectTrigger className="h-9 text-sm">
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
          <div className="flex items-center gap-2 rounded-md border border-destructive/20 bg-destructive/10 p-3 text-sm text-destructive">
            <AlertCircle className="h-4 w-4 shrink-0" />
            <p>{err}</p>
          </div>
        )}
        {ok && (
          <div className="flex items-center gap-2 rounded-md border border-emerald-500/20 bg-emerald-500/10 p-3 text-sm text-emerald-500">
            <CheckCircle2 className="h-4 w-4 shrink-0" />
            <p>{ok}</p>
          </div>
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
                <Label className="text-[11px] font-semibold text-muted-foreground">Minecraft UUID</Label>
                <Input
                  value={newUUID}
                  onChange={(e) => setNewUUID(e.target.value)}
                  placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
                  className="h-8 text-xs font-mono"
                />
              </div>
              <div className="w-28 space-y-1.5">
                <Label className="text-[11px] font-semibold text-muted-foreground">{t("users_link_source")}</Label>
                <Select value={newSource} onValueChange={setNewSource}>
                  <SelectTrigger className="h-8 text-xs">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                     <SelectItem value="mojang">Mojang</SelectItem>
                    <SelectItem value="thirdparty">Third-party Yggdrasil</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            {err && (
              <p className="text-xs text-destructive">{err}</p>
            )}
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
        <DialogFooter>
          <Button variant="outline" size="sm" onClick={() => setUnlinkDlg(null)}>
            {t("common:cancel")}
          </Button>
          <Button variant="destructive" size="sm" onClick={handleUnlinkConfirm}>
            {t("users_unlink_btn")}
          </Button>
        </DialogFooter>
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
            <Label className="text-xs font-semibold text-muted-foreground">{t("users_quota_servers")}</Label>
            <Input
              type="number"
              value={maxServers}
              onChange={(e) => setMaxServers(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs font-semibold text-muted-foreground">{t("users_quota_cpu")}</Label>
            <Input
              type="number"
              value={maxCpu}
              onChange={(e) => setMaxCpu(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs font-semibold text-muted-foreground">{t("users_quota_memory")}</Label>
            <Input
              type="number"
              value={maxMem}
              onChange={(e) => setMaxMem(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs font-semibold text-muted-foreground">{t("users_quota_storage")}</Label>
            <Input
              type="number"
              value={maxStorage}
              onChange={(e) => setMaxStorage(e.target.value)}
              placeholder={t("users_quota_unlimited")}
              className="h-9 text-sm"
            />
          </div>
        </div>

        {err && (
          <div className="flex items-center gap-2 rounded-md border border-destructive/20 bg-destructive/10 p-3 text-sm text-destructive">
            <AlertCircle className="h-4 w-4 shrink-0" />
            <p>{err}</p>
          </div>
        )}
        {ok && (
          <div className="flex items-center gap-2 rounded-md border border-emerald-500/20 bg-emerald-500/10 p-3 text-sm text-emerald-500">
            <CheckCircle2 className="h-4 w-4 shrink-0" />
            <p>{ok}</p>
          </div>
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
          <div className="flex items-center gap-2 rounded-md border border-destructive/20 bg-destructive/10 p-3 text-sm text-destructive mb-3">
            <AlertCircle className="h-4 w-4 shrink-0" />
            <p>{err}</p>
          </div>
        )}
        {ok && (
          <div className="flex items-center gap-2 rounded-md border border-emerald-500/20 bg-emerald-500/10 p-3 text-sm text-emerald-500 mb-3">
            <CheckCircle2 className="h-4 w-4 shrink-0" />
            <p>{ok}</p>
          </div>
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
        <DialogFooter>
          <Button variant="outline" size="sm" onClick={() => setRevokeOneDlg(null)}>
            {t("common:cancel")}
          </Button>
          <Button variant="destructive" size="sm" onClick={handleRevokeOne}>
            {t("users_session_revoke_confirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    {/* Revoke all confirmation dialog */}
    <Dialog open={revokeAllDlg} onOpenChange={setRevokeAllDlg}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("users_session_revoke_all_dlg_title")}</DialogTitle>
          <DialogDescription>{t("users_session_revoke_all_dlg_desc")}</DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" size="sm" onClick={() => setRevokeAllDlg(false)}>
            {t("common:cancel")}
          </Button>
          <Button variant="destructive" size="sm" onClick={handleRevokeAll}>
            {t("users_session_revoke_confirm")}
          </Button>
        </DialogFooter>
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
  const [dlg, setDlg] = useState<"disable" | "resetPw" | "delete" | null>(null);

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

        {/* Reset password */}
        <DangerRow
          icon={Key}
          title={t("users_danger_reset_pw")}
          desc={user.email
            ? t("users_danger_reset_pw_desc_email", { email: user.email })
            : t("users_danger_reset_pw_desc")}
          btnLabel={t("users_danger_reset_pw_btn")}
          btnVariant="destructive"
          onAction={() => setDlg("resetPw")}
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
  dlg: "disable" | "resetPw" | "delete" | null;
  setDlg: (v: null) => void;
  user: UserDetail;
  onChanged: () => void;
  navigate: (path: string) => void;
}) {
  const { t } = useTranslation("admin");
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);

  function close() {
    setDlg(null);
    setErr(null);
    setOk(null);
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

  async function handleResetPassword() {
    setLoading(true);
    setErr(null);
    try {
      const r = await api.resetUserPassword(user.id);
      setOk(t("users_pw_reset_ok", { email: r.email }));
      setLoading(false);
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
          {err && <p className="text-sm text-destructive">{err}</p>}
          <DialogFooter>
            <Button variant="outline" size="sm" onClick={close} disabled={loading}>
              {t("common:cancel")}
            </Button>
            <Button
              variant={user.disabled ? "default" : "destructive"}
              size="sm"
              disabled={loading}
              onClick={handleDisable}
              className="gap-1.5"
            >
              {loading && <Loader2 className="h-4 w-4 animate-spin" />}
              {user.disabled ? t("users_danger_enable_btn") : t("users_danger_disable_btn")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Reset password dialog */}
      <Dialog open={dlg === "resetPw"} onOpenChange={(v) => { if (!v) close(); }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <Key className="h-5 w-5 text-primary" />
              {t("users_danger_reset_pw_dlg_title")}
            </DialogTitle>
            <DialogDescription>
              {user.email
                ? t("users_danger_reset_pw_dlg_desc_email", { email: user.email })
                : t("users_danger_reset_pw_dlg_desc_no_email")}
            </DialogDescription>
          </DialogHeader>
          {err && <p className="text-sm text-destructive">{err}</p>}
          {ok && (
            <p className="rounded-md border border-emerald-500/20 bg-emerald-500/10 p-3 text-sm text-emerald-500">
              <CheckCircle2 className="inline h-4 w-4 mr-1" />
              {ok}
            </p>
          )}
          <DialogFooter>
            <Button variant="outline" size="sm" onClick={close} disabled={loading}>
              {t("common:cancel")}
            </Button>
            <Button
              size="sm"
              disabled={loading || ok !== null}
              onClick={handleResetPassword}
              className="gap-1.5"
            >
              {loading && <Loader2 className="h-4 w-4 animate-spin" />}
              {t("users_danger_reset_pw_confirm")}
            </Button>
          </DialogFooter>
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
          {err && <p className="text-sm text-destructive">{err}</p>}
          <DialogFooter>
            <Button variant="outline" size="sm" onClick={close} disabled={loading}>
              {t("common:cancel")}
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={loading}
              onClick={handleDelete}
              className="gap-1.5"
            >
              {loading && <Loader2 className="h-4 w-4 animate-spin" />}
              <Trash2 className="h-4 w-4" />
              {t("users_danger_delete_yes")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
