import { Loader2 } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useConfig, usePolling, useRefreshSignal } from "@/lib/hooks";
import type { ExecutionNode } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { InlineError } from "@/components/MessageLine";
import { InlineConfirm } from "@/components/InlineConfirm";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Label } from "@/components/ui/label";
import { FailoverDialog } from "@/components/FailoverDialog";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { formatRelative } from "@/lib/format";
import { useTier } from "@/lib/tier";

export function DistributedNodes() {
  const { t, i18n } = useTranslation("servers");
  const { isOwner } = useTier();
  const nodes = useAsync(api.nodes, [], { coalesce: true });
  usePolling(nodes.reload, 10_000);
  useRefreshSignal(nodes.reload);
  const cfg = useConfig();
  const controller = nodes.data?.find((n) => n.role === "controller");
  const controllers = nodes.data?.filter((n) => n.role === "controller").map((n) => n.name) ?? [];
  // A lost worker's worlds can be failed over; with controller high availability, a lost
  // controller's too, the control plane carrying on on another one.
  const failsOver = (n: ExecutionNode) => !n.ready && (n.role === "worker" || (n.role === "controller" && !!cfg?.controllerHA));
  // A worker the controller has moved past: another k3s version, or another felis build than
  // the one serving this page. An unrecorded felis build is not counted as behind.
  function behind(n: ExecutionNode) {
    if (n.role !== "worker") return false;
    if (controller?.k3sVersion && n.k3sVersion && n.k3sVersion !== controller.k3sVersion) return true;
    return !!n.felisVersion && !!cfg?.build?.version && n.felisVersion !== cfg.build.version;
  }
  return (
    <Card>
      <CardHeader><CardTitle>{t("nodes")}</CardTitle></CardHeader>
      <CardContent className="space-y-3">
      {nodes.loading && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("nodes_loading")}</p>}
      {!nodes.loading && !nodes.error && nodes.data?.length === 0 && <p className="text-sm text-muted-foreground">{t("nodes_empty")}</p>}
      {!!nodes.error && <InlineError message={humanizeError(nodes.error)} />}
      <ul className="space-y-1 text-sm">{nodes.data?.map((n) => (
        <li key={n.name} className="flex flex-wrap items-center gap-3">
          <code>{n.name}</code><span>{t(n.role === "controller" ? "node_controller" : n.role === "worker" ? "node_worker" : "node_unassigned")}</span>
          <Badge variant={n.ready ? "default" : "destructive"}>{n.ready ? t("node_online") : t("node_offline")}</Badge>
          {n.role === "worker" && <Badge variant={n.approved ? "default" : "muted"}>{n.approved ? t("node_approved") : t("node_pending")}</Badge>}
          {behind(n) && <Badge variant="warning" title={t("node_behind_hint")}>{t("node_behind")}</Badge>}
          {n.fenced && <Badge variant="muted" title={t("node_fenced_hint")}>{t("node_fenced")}</Badge>}
          {!n.ready && n.lostSince && <span className="text-destructive">{t("node_lost_since", { when: formatRelative(n.lostSince, Date.now(), i18n.language) })}</span>}
          <span className="text-muted-foreground">{n.architecture} · {n.addresses.join(", ")}{n.k3sVersion ? ` · k3s ${n.k3sVersion}` : ""}</span>
          {isOwner && failsOver(n) && <FailoverDialog node={n.name} controllers={controllers} onChanged={nodes.reload} />}
          {isOwner && n.role === "controller" && n.fenced && n.ready && <UnfenceController node={n.name} onChanged={nodes.reload} />}
        </li>
      ))}</ul>
      </CardContent>
    </Card>
  );
}

// A fenced controller that is online again returns to service from its row. A fenced worker
// returns through approval under Node operations instead, which checks it again.
function UnfenceController({ node, onChanged }: { node: string; onChanged: () => void }) {
  const { t } = useTranslation("servers");
  const reauth = useReauth();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [started, setStarted] = useState(false);
  const [error, setError] = useState<string | null>(null);
  async function unfence() {
    setBusy(true); setError(null);
    try {
      await reauth.guard(() => api.startNodeTask({ action: "unfence", name: node, confirmMaintenance: true }));
      setStarted(true); setConfirming(false); onChanged();
    } catch (e) { if (!isReauthCancelled(e)) setError(humanizeError(e)); }
    finally { setBusy(false); }
  }
  return <div className="basis-full space-y-2">
    {started ? <p role="status" className="text-muted-foreground">{t("node_unfence_started")}</p>
      : confirming ? <div className="space-y-2 rounded-md border p-3"><p className="text-muted-foreground">{t("node_unfence_confirm")}</p><InlineConfirm open confirming={busy} onConfirm={() => void unfence()} onCancel={() => setConfirming(false)} confirmLabel={t("node_unfence")} cancelLabel={t("common:cancel")} confirmVariant="default" className="flex justify-end gap-2" /></div>
      : <Button variant="outline" size="sm" onClick={() => setConfirming(true)}>{t("node_unfence")}</Button>}
    {error && <InlineError message={error} />}
    {reauth.dialog}
  </div>;
}

export function MigrationDialog({ name, nodeName, stopped, onChanged }: {
  name: string; nodeName?: string; stopped: boolean; onChanged: () => void;
}) {
  const { t } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmAbort, setConfirmAbort] = useState(false);
  const nodes = useAsync(() => open ? api.nodes() : Promise.resolve(null), [open]);
  const controllerName = nodes.data?.find((n) => n.role === "controller")?.name;
  const progress = useAsync(async () => {
    if (!open) return null;
    try { return await api.migration(name); }
    catch (e) { if ((e as { status?: number }).status === 404) return null; throw e; }
  }, [open, name]);
  usePolling(() => { if (open) progress.reload(); }, 3_000);
  const op = progress.data;
  const failed = op?.state === "failed";
  const active = !!op && !failed && op.state !== "succeeded" && op.state !== "aborted";
  // Worlds move to an approved worker or back to the controller; a server without a node
  // runs on the controller.
  const current = nodeName || controllerName;
  const workers = (nodes.data ?? []).filter((n) => n.ready && !n.fenced && ((n.approved && n.role === "worker") || n.role === "controller") && n.name !== current);
  async function run(retry: boolean) {
    setBusy(true); setError(null);
    try {
      if (retry && op) await api.retryMigration(name, op.id);
      else await api.migrateServer(name, target);
      progress.reload(); onChanged();
    } catch (e) { setError(humanizeError(e)); }
    finally { setBusy(false); }
  }
  // Abandoning keeps the server on its source world; the target node need not be online.
  async function abort() {
    if (!op) return;
    setBusy(true); setError(null);
    try {
      await api.abortMigration(name, op.id);
      setConfirmAbort(false);
      progress.reload(); onChanged();
    } catch (e) { setError(humanizeError(e)); }
    finally { setBusy(false); }
  }
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild><Button size="sm" variant="outline">{t("migration")}</Button></DialogTrigger>
      <DialogContent>
        <DialogHeader><DialogTitle>{t("migration")}</DialogTitle><DialogDescription>{t("migration_hint")}</DialogDescription></DialogHeader>
        {nodeName && <p className="text-sm">{t("node")}: {nodeName}</p>}
        {op && <div className="space-y-1 text-sm" role="status">
          <p>{op.sourceNode} → {op.targetNode}</p>
          <p>{t(op.kind === "failover" ? `failover_state_${op.state}` : `migration_state_${op.state}`, { defaultValue: op.state, target: op.targetNode })}</p>
          <code className="text-xs break-all">{op.id}</code>
          {op.error && <InlineError message={op.error} />}
        </div>}
        {!!progress.error && <InlineError message={humanizeError(progress.error)} />}
        {!!nodes.error && <InlineError message={humanizeError(nodes.error)} />}
        <Label htmlFor={`target-${name}`}>{t("node_choose")}</Label>
        <Select value={target} onValueChange={setTarget} disabled={active || busy || failed}>
          <SelectTrigger id={`target-${name}`}><SelectValue placeholder={t("node_choose")} /></SelectTrigger>
          <SelectContent>{workers.map((n) => <SelectItem key={n.name} value={n.name}>{n.role === "controller" ? t("failover_target_controller", { name: n.name }) : n.name}</SelectItem>)}</SelectContent>
        </Select>
        {!stopped && <p className="text-sm text-muted-foreground">{t("migration_stop")}</p>}
        <InlineError message={error} />
        {failed ? (
          <div className="space-y-3">
            <div className="flex flex-wrap gap-2">
              <Button onClick={() => run(true)} disabled={!stopped || busy || confirmAbort}>{t("common:try_again")}</Button>
              {!confirmAbort && (
                <Button variant="outline" onClick={() => setConfirmAbort(true)} disabled={busy}>{t("migration_abort")}</Button>
              )}
            </div>
            {confirmAbort && (
              <div className="space-y-2 rounded-md border p-3">
                <p className="text-sm text-muted-foreground">{t("migration_abort_confirm")}</p>
                <InlineConfirm
                  open
                  confirming={busy}
                  onConfirm={() => void abort()}
                  onCancel={() => setConfirmAbort(false)}
                  confirmLabel={t("migration_abort")}
                  cancelLabel={t("common:cancel")}
                  className="flex justify-end gap-2"
                />
              </div>
            )}
          </div>
        ) : (
          <Button onClick={() => run(false)} disabled={!stopped || active || busy || progress.loading || !!progress.error || !workers.some((n) => n.name === target)}>{t("migration_start")}</Button>
        )}
      </DialogContent>
    </Dialog>
  );
}
