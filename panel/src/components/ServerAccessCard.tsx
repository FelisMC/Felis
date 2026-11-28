import { useMemo, useState } from "react";
import { Loader2, Plus, Shield, ShieldCheck, Trash2, Users } from "lucide-react";
import { useTranslation } from "react-i18next";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { RoleBadge } from "@/components/RoleBadge";
import { ErrorState, Loading } from "@/components/States";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import type { CollaboratorCandidate, CollaboratorRole, ServerCollaborator } from "@/lib/types";

interface ServerAccessCardProps {
  serverName: string;
  isProtected: boolean;
  isOwner: boolean;
  onProtectionChanged: () => void;
}

function filterCandidates(
  candidates: readonly CollaboratorCandidate[],
  collaborators: readonly ServerCollaborator[],
  query: string,
): CollaboratorCandidate[] {
  const assigned = new Set(collaborators.map((c) => c.user_id));
  const needle = query.trim().toLowerCase();
  return candidates.filter((c) => {
    if (assigned.has(c.id)) return false;
    if (!needle) return true;
    return c.username.toLowerCase().includes(needle) || (c.email ?? "").toLowerCase().includes(needle);
  });
}

function candidateLabel(c: CollaboratorCandidate): string {
  return c.email ? `${c.username} (${c.email})` : c.username;
}

/** Manages protected-server status (Owner) and per-server collaborators (Owner/Admin). */
export function ServerAccessCard({
  serverName,
  isProtected,
  isOwner,
  onProtectionChanged,
}: ServerAccessCardProps) {
  const { t } = useTranslation("servers");
  const reauth = useReauth();
  const query = useAsync(() => api.listServerCollaborators(serverName), [serverName]);

  const [toggling, setToggling] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedUserId, setSelectedUserId] = useState("");
  const [selectedRole, setSelectedRole] = useState<CollaboratorRole>("operator");
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  const collaborators = useMemo(() => query.data?.collaborators ?? [], [query.data]);
  const candidates = useMemo(() => query.data?.candidates ?? [], [query.data]);
  const available = useMemo(
    () => filterCandidates(candidates, collaborators, search),
    [candidates, collaborators, search],
  );
  const activeUserId = available.some((c) => c.id === selectedUserId) ? selectedUserId : "";

  async function handleToggleProtected(next: boolean) {
    if (!isOwner || toggling) return;
    setToggling(true);
    setMessage(null);
    try {
      await reauth.guard(() => api.setServerProtection(serverName, next));
      setMessage({
        kind: "success",
        text: t(next ? "protected_enabled_success" : "protected_disabled_success"),
      });
      onProtectionChanged();
      query.reload();
    } catch (e) {
      if (!isReauthCancelled(e)) setMessage({ kind: "error", text: humanizeError(e) });
    } finally {
      setToggling(false);
    }
  }

  async function handleAdd() {
    const picked = available.find((c) => c.id === activeUserId);
    if (!picked || busyKey) return;
    setBusyKey("add");
    setMessage(null);
    try {
      await api.setServerCollaborator(serverName, picked.id, selectedRole);
      setSelectedUserId("");
      setSearch("");
      setMessage({ kind: "success", text: t("collaborator_saved", { name: picked.username }) });
      query.reload();
    } catch (e) {
      setMessage({ kind: "error", text: humanizeError(e) });
    } finally {
      setBusyKey(null);
    }
  }

  async function handleRoleChange(c: ServerCollaborator, role: CollaboratorRole) {
    if (role === c.role || busyKey) return;
    setBusyKey(c.user_id);
    setMessage(null);
    try {
      await api.setServerCollaborator(serverName, c.user_id, role);
      setMessage({ kind: "success", text: t("collaborator_saved", { name: c.username }) });
      query.reload();
    } catch (e) {
      setMessage({ kind: "error", text: humanizeError(e) });
    } finally {
      setBusyKey(null);
    }
  }

  async function handleRemove(c: ServerCollaborator) {
    if (busyKey) return;
    setBusyKey(c.user_id);
    setMessage(null);
    try {
      await api.removeServerCollaborator(serverName, c.user_id);
      setMessage({ kind: "success", text: t("collaborator_removed", { name: c.username }) });
      query.reload();
    } catch (e) {
      setMessage({ kind: "error", text: humanizeError(e) });
    } finally {
      setBusyKey(null);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center justify-between gap-2">
          <span className="flex items-center gap-2">
            {isProtected ? (
              <ShieldCheck className="h-4 w-4 text-primary" aria-hidden="true" />
            ) : (
              <Shield className="h-4 w-4 text-primary" aria-hidden="true" />
            )}
            {t("access_card_title")}
          </span>
          {isProtected && <Badge variant="warning">{t("protected_badge")}</Badge>}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {isOwner && (
          <div className="flex items-start justify-between gap-3 rounded-lg border border-border px-3.5 py-2.5">
            <div className="min-w-0 space-y-0.5">
              <Label htmlFor="server-protected-toggle" className="text-sm">
                {t("protected_toggle_label")}
              </Label>
              <p className="text-xs leading-relaxed text-muted-foreground">
                {t("protected_toggle_hint")}
              </p>
            </div>
            <Switch
              id="server-protected-toggle"
              checked={isProtected}
              disabled={toggling}
              onCheckedChange={(checked) => void handleToggleProtected(checked)}
            />
          </div>
        )}

        <div className="space-y-3">
          <div className="space-y-0.5">
            <h4 className="flex items-center gap-1.5 text-sm font-medium text-foreground">
              <Users className="h-3.5 w-3.5 text-muted-foreground" aria-hidden="true" />
              {t("collaborators_section_title")}
            </h4>
            <p className="text-xs text-muted-foreground">{t("collaborators_section_hint")}</p>
          </div>

          <div className="grid gap-1 rounded-md border border-border/60 bg-muted/35 px-3 py-2 text-xs text-muted-foreground">
            <p>{t("collaborator_role_operator_hint")}</p>
            <p>{t("collaborator_role_manager_hint")}</p>
          </div>

          {query.loading && !query.data ? (
            <Loading />
          ) : query.error && !query.data ? (
            <ErrorState error={query.error} onRetry={query.reload} />
          ) : (
            <>
              <div className="grid gap-2 sm:grid-cols-[1fr_1fr_9rem_auto]">
                <Input
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  placeholder={t("collaborator_search_placeholder")}
                  aria-label={t("collaborator_search_placeholder")}
                  disabled={busyKey !== null}
                />
                {available.length > 0 ? (
                  <Select value={activeUserId} onValueChange={setSelectedUserId} disabled={busyKey !== null}>
                    <SelectTrigger aria-label={t("collaborator_select_placeholder")}>
                      <SelectValue placeholder={t("collaborator_select_placeholder")} />
                    </SelectTrigger>
                    <SelectContent>
                      {available.map((c) => (
                        <SelectItem key={c.id} value={c.id}>
                          {candidateLabel(c)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : (
                  <div className="flex h-8 items-center rounded-md border border-dashed border-border px-2.5 text-xs text-muted-foreground">
                    {t("collaborator_no_candidates")}
                  </div>
                )}
                <Select
                  value={selectedRole}
                  onValueChange={(v) => setSelectedRole(v as CollaboratorRole)}
                  disabled={busyKey !== null}
                >
                  <SelectTrigger aria-label={t("collaborator_role_label")}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="operator">{t("collaborator_role_operator")}</SelectItem>
                    <SelectItem value="manager">{t("collaborator_role_manager")}</SelectItem>
                  </SelectContent>
                </Select>
                <Button
                  type="button"
                  size="sm"
                  disabled={!activeUserId || busyKey !== null}
                  onClick={() => void handleAdd()}
                >
                  {busyKey === "add" ? <Loader2 className="animate-spin" /> : <Plus />}
                  {t("collaborator_add")}
                </Button>
              </div>

              {collaborators.length === 0 ? (
                <p className="py-2 text-xs text-muted-foreground">{t("collaborator_empty")}</p>
              ) : (
                <ul className="divide-y divide-border rounded-lg border border-border">
                  {collaborators.map((c) => {
                    const rowBusy = busyKey === c.user_id;
                    return (
                      <li
                        key={c.user_id}
                        className="flex flex-wrap items-center justify-between gap-2 px-3 py-2"
                      >
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-2">
                            <span className="truncate text-sm font-medium text-foreground">{c.username}</span>
                            <RoleBadge role={c.user_role} />
                          </div>
                          {c.email && (
                            <p className="truncate text-xs text-muted-foreground">{c.email}</p>
                          )}
                        </div>
                        <div className="flex items-center gap-2">
                          <Select
                            value={c.role}
                            onValueChange={(v) => void handleRoleChange(c, v as CollaboratorRole)}
                            disabled={busyKey !== null}
                          >
                            <SelectTrigger
                              className="w-32"
                              aria-label={t("collaborator_role_for", { name: c.username })}
                            >
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent>
                              <SelectItem value="operator">{t("collaborator_role_operator")}</SelectItem>
                              <SelectItem value="manager">{t("collaborator_role_manager")}</SelectItem>
                            </SelectContent>
                          </Select>
                          <Button
                            type="button"
                            size="sm"
                            variant="ghost"
                            disabled={busyKey !== null}
                            onClick={() => void handleRemove(c)}
                            aria-label={t("collaborator_remove_aria", { name: c.username })}
                          >
                            {rowBusy ? <Loader2 className="animate-spin" /> : <Trash2 />}
                            <span className="sr-only sm:not-sr-only">{t("collaborator_remove")}</span>
                          </Button>
                        </div>
                      </li>
                    );
                  })}
                </ul>
              )}
            </>
          )}
        </div>

        {message && <MessageLine kind={message.kind} message={message.text} />}
        {reauth.dialog}
      </CardContent>
    </Card>
  );
}
