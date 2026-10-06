import { useEffect, useState } from "react";
import { Loader2, Play, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { useAsync, usePolling } from "@/lib/hooks";
import type { NodeControlRequest, NodeControlTask } from "@/lib/types";

export function NodeControlPanel({ distributed }: { distributed: boolean | null }) {
  const { t } = useTranslation("admin");
  const reauth = useReauth();
  const tasks = useAsync(api.nodeTasks, [], { coalesce: true });
  const [id, setId] = useState<string | null>(null);
  const [action, setAction] = useState<NodeControlRequest["action"]>(distributed ? "join" : "enable");
  const [name, setName] = useState("");
  const [ssh, setSSH] = useState("");
  const [ip, setIP] = useState("");
  const [peers, setPeers] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const progress = useAsync(() => id ? api.nodeTask(id) : Promise.resolve(null), [id], { coalesce: true });
  const task = progress.data;
  const running = tasks.data?.tasks.some((entry) => entry.state === "running") || task?.state === "running";
  const busy = submitting || !!running;
  const available = tasks.data?.available === true && !tasks.error && distributed !== null;
  useEffect(() => { if (distributed !== null) { setAction(distributed ? "join" : "enable"); setConfirmed(false); } }, [distributed]);
  useEffect(() => { if (!id && tasks.data?.tasks[0]) setId(tasks.data.tasks[0].id); }, [id, tasks.data]);
  usePolling(() => { tasks.reload(); if (id) progress.reload(); }, running || progress.error || tasks.error ? 3000 : 10000);
  async function run(retry = false) {
    if (!available || busy || (!retry && !confirmed)) return;
    setSubmitting(true); setError(null);
    try {
      const result: NodeControlTask = await reauth.guard(() => retry && id ? api.retryNodeTask(id) : api.startNodeTask({
        action, ...(action !== "enable" ? { name: name.trim(), sshTarget: ssh.trim() } : {}),
        ...(action !== "approve" ? { externalIP: ip.trim(), peers: peers.split(/[\s,]+/).filter(Boolean) } : {}),
        confirmMaintenance: confirmed,
      }));
      setId(result.id); setConfirmed(false); tasks.reload();
    } catch (e) { if (!isReauthCancelled(e)) setError(humanizeError(e)); }
    finally { setSubmitting(false); }
  }
  const valid = confirmed && (action === "enable" || !!name.trim() && !!ssh.trim()) && (action === "approve" || !!ip.trim());
  return <Card>
    <CardHeader className="flex-row items-center justify-between gap-3"><CardTitle>{t("node_control_title")}</CardTitle><Button variant="outline" size="sm" disabled={tasks.loading} onClick={() => { tasks.reload(); progress.reload(); }}><RefreshCw />{t("node_control_reload")}</Button></CardHeader>
    <CardContent className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">{t("node_control_description")}</p>
      {tasks.loading && !tasks.data && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("node_control_loading")}</p>}
      {tasks.error != null && <MessageLine kind="error" message={humanizeError(tasks.error)} />}
      {tasks.data?.available === false && <p role="status" className="text-sm text-muted-foreground">{t("node_control_unavailable")}</p>}
      {running && task?.state !== "running" && <div className="flex flex-wrap items-center gap-3"><p role="status" className="text-sm text-muted-foreground">{t("node_control_other_running")}</p><Button variant="outline" size="sm" onClick={() => { const active = tasks.data?.tasks.find((entry) => entry.state === "running"); if (active) setId(active.id); }}>{t("node_control_show_running")}</Button></div>}
      <div className="grid gap-5 md:grid-cols-2">
        <div className="space-y-2"><Label htmlFor="node-action">{t("node_control_action")}</Label><Select value={action} disabled={!available || busy} onValueChange={(value) => { setAction(value as NodeControlRequest["action"]); setConfirmed(false); }}><SelectTrigger id="node-action"><SelectValue /></SelectTrigger><SelectContent>{!distributed && <SelectItem value="enable">{t("node_control_enable")}</SelectItem>}<SelectItem value="join" disabled={!distributed}>{t("node_control_join")}</SelectItem><SelectItem value="approve" disabled={!distributed}>{t("node_control_approve")}</SelectItem></SelectContent></Select></div>
        {action !== "enable" && <>
          <div className="space-y-2"><Label htmlFor="node-name">{t("node_control_name")}</Label><Input id="node-name" value={name} disabled={!available || busy} onChange={(e) => setName(e.target.value)} placeholder="worker-01" /></div>
          <div className="space-y-2"><Label htmlFor="node-ssh">{t("node_control_ssh")}</Label><Input id="node-ssh" value={ssh} disabled={!available || busy} onChange={(e) => setSSH(e.target.value)} placeholder="root@192.168.1.20" /><p className="text-xs text-muted-foreground">{t("node_control_ssh_hint")}</p></div>
        </>}
        {action !== "approve" && <>
          <div className="space-y-2"><Label htmlFor="node-ip">{t("node_control_ip")}</Label><Input id="node-ip" value={ip} disabled={!available || busy} onChange={(e) => setIP(e.target.value)} placeholder="192.168.1.20" /></div>
          <div className="space-y-2 md:col-span-2"><Label htmlFor="node-peers">{t("node_control_peers")}</Label><Input id="node-peers" value={peers} disabled={!available || busy} onChange={(e) => setPeers(e.target.value)} placeholder="192.168.1.21/32, 192.168.1.22/32" /><p className="text-xs text-muted-foreground">{t("node_control_peers_hint")}</p></div>
        </>}
      </div>
      <div className="flex items-start gap-3 rounded-lg border border-border p-4"><Switch id="node-confirm" checked={confirmed} disabled={!available || busy} onCheckedChange={setConfirmed} /><Label htmlFor="node-confirm" className="text-sm leading-relaxed">{t(action === "enable" ? "node_control_confirm_enable" : "node_control_confirm_worker")}</Label></div>
      {error && <MessageLine kind="error" message={error} />}
      <Button disabled={!available || busy || !valid} onClick={() => void run()}>{submitting ? <Loader2 className="animate-spin" /> : <Play />}{t("node_control_submit")}</Button>
      <details className="rounded-lg border border-border p-4"><summary className="cursor-pointer text-sm font-medium">{t("node_control_prerequisites")}</summary><p className="mt-3 text-sm leading-relaxed text-muted-foreground">{t("node_control_prerequisites_body")}</p><Button asChild variant="outline" size="sm" className="mt-3"><a href="https://github.com/FelisMC/Felis/blob/main/docs/distributed.md" target="_blank" rel="noreferrer">{t("platform_distribution_runbook")}</a></Button></details>
      {!!tasks.data?.tasks.length && <div className="space-y-2"><Label htmlFor="node-task">{t("node_control_history")}</Label><Select value={id ?? undefined} onValueChange={setId}><SelectTrigger id="node-task"><SelectValue /></SelectTrigger><SelectContent>{tasks.data.tasks.map((entry) => <SelectItem key={entry.id} value={entry.id}>{t(`node_control_${entry.request.action}`)} · {entry.request.name || t("node_control_controller")} · {new Date(entry.startedAt).toLocaleString()} · {t(`node_control_state_${entry.state}`)}</SelectItem>)}</SelectContent></Select></div>}
      {progress.error != null && <MessageLine kind="error" message={t("node_control_connection_lost", { error: humanizeError(progress.error) })} />}
      {id && progress.loading && !task && <p role="status" className="text-sm text-muted-foreground">{t("node_control_loading")}</p>}
      {task && <section aria-label={t("node_control_progress")} className="space-y-3 rounded-lg border border-border p-4">
        <div className="flex flex-wrap items-center gap-3"><Badge variant={task.state === "failed" ? "destructive" : task.state === "succeeded" ? "default" : "muted"}>{t(`node_control_state_${task.state === "running" && (progress.error || tasks.error) ? "unknown" : task.state}`)}</Badge><span className="text-sm">{t(`node_control_stage_${task.stage}`, { defaultValue: task.stage })}</span></div>
        <p className="text-xs text-muted-foreground">{t("node_control_task_id")}: <code>{task.id}</code></p>
        <p className="text-xs text-muted-foreground">{t("node_control_started", { time: new Date(task.startedAt).toLocaleString() })} · {t("node_control_timeout")}</p>
        {task.error && <MessageLine kind="error" message={task.error} />}
        <pre aria-label={t("node_control_log")} className="max-h-80 overflow-auto rounded-md bg-muted p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-all">{task.log || t("node_control_log_pending")}</pre>
        {task.state === "failed" && <><p className="text-sm text-muted-foreground">{t("node_control_retry_hint")}</p><Button variant="outline" disabled={!available || busy} onClick={() => void run(true)}><RefreshCw />{t("node_control_retry")}</Button></>}
        {task.state === "succeeded" && task.request.action === "enable" && <Button variant="outline" onClick={() => window.location.reload()}>{t("node_control_refresh_deployment")}</Button>}
      </section>}
    </CardContent>
    {reauth.dialog}
  </Card>;
}
