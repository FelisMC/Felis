import { useState } from "react";
import { Loader2, ShieldAlert } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { InlineError, MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { formatBytes, formatRelative } from "@/lib/format";
import { useAsync, usePolling } from "@/lib/hooks";
import type { FailoverResult, FailoverServer } from "@/lib/types";

/** failingOver reports whether a server already has a failover underway or done; one that
 *  failed may be started again. */
function failingOver(s: FailoverServer): boolean {
  return s.operation?.kind === "failover" && s.operation.state !== "failed" && s.operation.state !== "aborted";
}

export function FailoverDialog({ node, controllers = [], onChanged }: { node: string; controllers?: string[]; onChanged: () => void }) {
  const { t, i18n } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<FailoverResult | null>(null);
  const reauth = useReauth();
  const plan = useAsync(() => (open ? api.failoverPlan(node) : Promise.resolve(null)), [open, node]);
  usePolling(() => { if (open) plan.reload(); }, 5_000);
  const p = plan.data;
  const now = Date.now();
  const movable = p?.servers.filter((s) => s.backup && !failingOver(s)) ?? [];
  const confirmed = confirm.trim() === node;
  const canStart = !!p && p.lost && p.targets.includes(target) && confirmed && !busy;

  function reset(next: boolean) {
    setOpen(next);
    if (!next) {
      setTarget("");
      setConfirm("");
      setError(null);
      setResult(null);
    }
  }
  async function start() {
    if (!canStart) return;
    setBusy(true);
    setError(null);
    try {
      const res = await reauth.guard(() => api.failOverNode(node, target, confirm.trim()));
      setResult(res);
      setConfirm("");
      plan.reload();
      onChanged();
    } catch (e) {
      if (!isReauthCancelled(e)) setError(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }

  function status(s: FailoverServer) {
    const op = s.operation;
    if (op?.kind === "failover") {
      return (
        <div role="status" className="space-y-1">
          <p>{t(`failover_state_${op.state}`, { defaultValue: op.state, target: op.targetNode })}</p>
          {op.error && <InlineError message={op.error} />}
        </div>
      );
    }
    if (s.backup) {
      return <p className="text-muted-foreground">{t("failover_backup", { when: formatRelative(s.backup.createdAt, now, i18n.language), size: formatBytes(s.backup.sizeBytes) })}</p>;
    }
    return <p className="text-amber-700 dark:text-amber-300">{t("failover_no_backup")}</p>;
  }

  return (
    <Dialog open={open} onOpenChange={reset}>
      <DialogTrigger asChild><Button size="sm" variant="destructive"><ShieldAlert />{t("failover")}</Button></DialogTrigger>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{t("failover_title", { node })}</DialogTitle>
          <DialogDescription>{t("failover_hint")}</DialogDescription>
        </DialogHeader>
        {!p && plan.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("failover_loading")}</p>}
        {!!plan.error && <InlineError message={humanizeError(plan.error)} />}
        {p && !p.lost && <MessageLine kind="error" message={t("failover_online")} />}
        {p && p.lost && (
          <div className="space-y-4">
            {p.servers.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("failover_no_servers")}</p>
            ) : (
              <div className="space-y-2">
                <p className="text-sm font-medium">{t("failover_servers")}</p>
                <ul className="divide-y divide-border/60 rounded-md border text-sm">
                  {p.servers.map((s) => (
                    <li key={s.name} className="space-y-1 p-3">
                      <div className="flex flex-wrap items-center justify-between gap-2">
                        <code className="font-medium">{s.name}</code>
                        <Badge variant="muted">{t(s.desiredState === "Running" || s.operation?.resume === "Running" ? "failover_resume_running" : "failover_resume_stopped")}</Badge>
                      </div>
                      {status(s)}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            {movable.length > 0 && (
              <>
                <div className="space-y-2">
                  <Label htmlFor={`failover-target-${node}`}>{t("failover_target")}</Label>
                  <Select value={target} onValueChange={setTarget} disabled={busy}>
                    <SelectTrigger id={`failover-target-${node}`}><SelectValue placeholder={t("node_choose")} /></SelectTrigger>
                    <SelectContent>{p.targets.map((n) => <SelectItem key={n} value={n}>{controllers.includes(n) ? t("failover_target_controller", { name: n }) : n}</SelectItem>)}</SelectContent>
                  </Select>
                  {p.targets.length === 0 && <p className="text-sm text-muted-foreground">{t("failover_no_targets")}</p>}
                </div>
                <div className="space-y-2">
                  <Label htmlFor={`failover-confirm-${node}`}>{t("failover_confirm_label", { node })}</Label>
                  <Input id={`failover-confirm-${node}`} value={confirm} disabled={busy} autoComplete="off" spellCheck={false} onChange={(e) => { setConfirm(e.target.value); setError(null); }} />
                </div>
              </>
            )}
            {result && (
              <div className="space-y-1 rounded-md border p-3 text-sm" role="status">
                <p>{t("failover_started", { count: result.operations.length })}</p>
                {result.skipped.map((s) => <p key={s.name} className="text-muted-foreground">{t(`failover_skipped_${s.reason}`, { name: s.name, error: s.error ?? "" })}</p>)}
                {result.fenceError && <p className="text-amber-700 dark:text-amber-300">{t("failover_fence_pending", { error: result.fenceError })}</p>}
              </div>
            )}
            <InlineError message={error} />
            {movable.length > 0 && (
              <div className="flex justify-end">
                <Button variant="destructive" disabled={!canStart} onClick={() => void start()}>{busy ? <Loader2 className="animate-spin" /> : <ShieldAlert />}{t("failover_start")}</Button>
              </div>
            )}
          </div>
        )}
        {reauth.dialog}
      </DialogContent>
    </Dialog>
  );
}
