import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, humanizeError } from "@/lib/api";
import { useAsync, usePolling } from "@/lib/hooks";
import { Button } from "@/components/ui/button";
import { InlineError } from "@/components/MessageLine";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Label } from "@/components/ui/label";

export function DistributedNodes() {
  const { t } = useTranslation("servers");
  const nodes = useAsync(api.nodes);
  usePolling(nodes.reload, 10_000);
  return (
    <div className="rounded-lg border p-4 space-y-2">
      <h2 className="font-medium">{t("nodes")}</h2>
      {!!nodes.error && <InlineError message={humanizeError(nodes.error)} />}
      <ul className="space-y-1 text-sm">{nodes.data?.map((n) => (
        <li key={n.name} className="flex flex-wrap gap-3">
          <code>{n.name}</code><span>{n.role || t("node_pending")}</span>
          <span>{n.ready ? t("node_online") : t("node_offline")}</span>
          <span>{n.approved ? t("node_approved") : t("node_pending")}</span>
          <span className="text-muted-foreground">{n.architecture} · {n.addresses.join(", ")}</span>
        </li>
      ))}</ul>
    </div>
  );
}

export function MigrationDialog({ name, nodeName, stopped, onChanged }: {
  name: string; nodeName?: string; stopped: boolean; onChanged: () => void;
}) {
  const { t } = useTranslation("servers");
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const nodes = useAsync(() => open ? api.nodes() : Promise.resolve(null), [open]);
  const progress = useAsync(async () => {
    if (!open) return null;
    try { return await api.migration(name); }
    catch (e) { if ((e as { status?: number }).status === 404) return null; throw e; }
  }, [open, name]);
  usePolling(() => { if (open) progress.reload(); }, 3_000);
  const op = progress.data;
  const active = !!op && op.state !== "succeeded" && op.state !== "failed";
  const workers = (nodes.data ?? []).filter((n) => n.ready && n.approved && n.role === "worker" && n.name !== nodeName);
  async function run(retry: boolean) {
    setBusy(true); setError(null);
    try {
      if (retry && op) await api.retryMigration(name, op.id);
      else await api.migrateServer(name, target);
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
          <p>{t(`migration_state_${op.state}`, { defaultValue: op.state })}</p>
          <code className="text-xs break-all">{op.id}</code>
          {op.error && <InlineError message={op.error} />}
        </div>}
        {!!progress.error && <InlineError message={humanizeError(progress.error)} />}
        {!!nodes.error && <InlineError message={humanizeError(nodes.error)} />}
        <Label htmlFor={`target-${name}`}>{t("node_choose")}</Label>
        <Select value={target} onValueChange={setTarget} disabled={active || busy || op?.state === "failed"}>
          <SelectTrigger id={`target-${name}`}><SelectValue placeholder={t("node_choose")} /></SelectTrigger>
          <SelectContent>{workers.map((n) => <SelectItem key={n.name} value={n.name}>{n.name}</SelectItem>)}</SelectContent>
        </Select>
        {!stopped && <p className="text-sm text-muted-foreground">{t("migration_stop")}</p>}
        <InlineError message={error} />
        {op?.state === "failed" ? (
          <Button onClick={() => run(true)} disabled={!stopped || busy}>{t("common:try_again")}</Button>
        ) : (
          <Button onClick={() => run(false)} disabled={!stopped || active || busy || progress.loading || !!progress.error || !workers.some((n) => n.name === target)}>{t("migration_start")}</Button>
        )}
      </DialogContent>
    </Dialog>
  );
}
