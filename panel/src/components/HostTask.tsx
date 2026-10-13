import { useState } from "react";
import { Download, ExternalLink, Loader2, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { saveBlob } from "@/lib/download";
import { useAsync, usePolling, useRefreshSignal } from "@/lib/hooks";
import type { HostTaskAction, NodeControlRequest, NodeControlTask } from "@/lib/types";

/** useHostTask follows the newest host task of the given actions: it starts one (behind
 *  a fresh reauthentication), and reads its progress while it runs. The host runs one
 *  task at a time, so busy also holds while any other task runs. */
export function useHostTask(actions: HostTaskAction[]) {
  const reauth = useReauth();
  const list = useAsync(api.nodeTasks, [], { coalesce: true });
  const [chosen, setChosen] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const latest = list.data?.tasks.find((t) => (actions as string[]).includes(t.request.action));
  const id = chosen ?? latest?.id ?? null;
  const detail = useAsync(() => (id ? api.nodeTask(id) : Promise.resolve(null)), [id], { coalesce: true });
  const task = detail.data;
  const running = task?.state === "running";
  const otherRunning = !!list.data?.tasks.some((t) => t.state === "running" && t.id !== id);
  usePolling(() => {
    list.reload();
    if (id) detail.reload();
  }, running ? 2000 : otherRunning ? 5000 : 30000);
  useRefreshSignal(list.reload);
  useRefreshSignal(detail.reload);
  async function start(request: NodeControlRequest): Promise<NodeControlTask | null> {
    setStarting(true);
    setError(null);
    try {
      const created = await reauth.guard(() => api.startNodeTask(request));
      setChosen(created.id);
      list.reload();
      return created;
    } catch (e) {
      if (!isReauthCancelled(e)) setError(humanizeError(e));
      return null;
    } finally {
      setStarting(false);
    }
  }
  return {
    available: list.data?.available === true,
    loading: list.loading && !list.data,
    listError: list.error,
    task,
    running,
    otherRunning,
    busy: starting || running || otherRunning,
    starting,
    error,
    start,
    reauth,
  };
}

const cloudflareLogin = /https:\/\/dash\.cloudflare\.com\/argotunnel\S+/;

/** HostTaskStatus shows one host task: state, stage, error, its log, and what it left
 *  behind (a download, the Cloudflare page to authorize the host on). */
export function HostTaskStatus({ task, onRetry, retryDisabled }: { task: NodeControlTask; onRetry?: () => void; retryDisabled?: boolean }) {
  const { t } = useTranslation("admin");
  const reauth = useReauth();
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState<string | null>(null);
  const login = task.state === "running" ? cloudflareLogin.exec(task.log ?? "")?.[0] : undefined;
  const stage = t(`host_stage_${task.stage}`, { defaultValue: t(`node_control_stage_${task.stage}`, { defaultValue: task.stage }) });
  async function download() {
    setDownloading(true);
    setDownloadError(null);
    try {
      const file = await reauth.guard(() => api.nodeTaskArtifact(task.id));
      saveBlob(file.blob, file.filename);
    } catch (e) {
      if (!isReauthCancelled(e)) setDownloadError(humanizeError(e));
    } finally {
      setDownloading(false);
    }
  }
  return (
    <section aria-label={t("host_task_progress")} className="space-y-3 rounded-lg border border-border p-4">
      <div className="flex flex-wrap items-center gap-3">
        <Badge variant={task.state === "failed" ? "destructive" : task.state === "succeeded" ? "default" : "muted"}>
          {task.state === "running" && <Loader2 className="h-3 w-3 animate-spin" aria-hidden="true" />}
          {t(`node_control_state_${task.state}`)}
        </Badge>
        <span className="text-sm font-medium">{t(`host_action_${task.request.action}`, { defaultValue: t(`node_control_${task.request.action}`) })}</span>
        <span className="text-sm text-muted-foreground">{stage}</span>
      </div>
      <p className="text-xs text-muted-foreground">
        {t("node_control_started", { time: new Date(task.startedAt).toLocaleString() })}
        {task.finishedAt && ` · ${t("host_task_finished", { time: new Date(task.finishedAt).toLocaleString() })}`}
      </p>
      {login && (
        <div className="space-y-2 rounded-md border border-primary/30 bg-primary/5 p-3">
          <p className="text-sm">{t("host_cloudflare_login")}</p>
          <Button asChild size="sm">
            <a href={login} target="_blank" rel="noreferrer"><ExternalLink />{t("host_cloudflare_login_open")}</a>
          </Button>
        </div>
      )}
      {task.error && <MessageLine kind="error" message={task.error} />}
      {task.state === "succeeded" && task.artifact && (
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="outline" size="sm" disabled={downloading} onClick={() => void download()}>
            {downloading ? <Loader2 className="animate-spin" /> : <Download />}
            {t("host_task_download")}
          </Button>
          <span className="text-xs text-muted-foreground break-all">{task.artifact.split("/").pop()}</span>
        </div>
      )}
      {downloadError && <MessageLine kind="error" message={downloadError} />}
      <details open={task.state !== "succeeded"} className="group">
        <summary className="cursor-pointer text-xs font-medium text-muted-foreground">{t("node_control_log")}</summary>
        <pre aria-label={t("node_control_log")} className="mt-2 max-h-72 overflow-auto rounded-md bg-muted p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-all">
          {task.log || t("node_control_log_pending")}
        </pre>
      </details>
      {task.state === "failed" && onRetry && (
        <Button variant="outline" size="sm" disabled={retryDisabled} onClick={onRetry}><RefreshCw />{t("node_control_retry")}</Button>
      )}
      {reauth.dialog}
    </section>
  );
}

/** HostTaskNotice is the line a card shows while the host runs another task. */
export function HostTaskNotice({ otherRunning, available, loading }: { otherRunning: boolean; available: boolean; loading: boolean }) {
  const { t } = useTranslation("admin");
  if (loading) return <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("node_control_loading")}</p>;
  if (!available) return <p role="status" className="text-sm text-muted-foreground">{t("node_control_unavailable")}</p>;
  if (otherRunning) return <p role="status" className="text-sm text-muted-foreground">{t("host_task_other_running")}</p>;
  return null;
}
