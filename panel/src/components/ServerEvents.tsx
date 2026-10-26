import { useId } from "react";
import { useTranslation } from "react-i18next";
import { History, PowerOff, RotateCcw } from "lucide-react";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { ServerEvent, ServerStatus } from "@/lib/types";

type T = (key: string, opts?: Record<string, unknown>) => string;

export function crashCause(t: T, reason: string | undefined, exitCode: number): string {
  if (reason === "OOMKilled") return t("crash_cause_oom");
  if (reason === "Completed") return t("crash_cause_clean");
  return t("crash_cause_exit", { code: exitCode });
}

export function eventText(t: T, ev: ServerEvent): string {
  switch (ev.type) {
    case "Started":
      return t("event_started");
    case "Stopped":
      if (ev.reason === "Idle") return t("event_stopped_idle");
      if (ev.reason === "Crashed" || ev.reason === "CrashLimit") return t("event_stopped_crash");
      if (ev.reason === "HostRestarted") return t("event_stopped_host");
      if (ev.reason === "DiskFull") return t("event_stopped_disk");
      return t("event_stopped");
    case "HostRestarted":
      return t(ev.stopping ? "event_host_stop" : "event_host_resume");
    case "StartFailed":
      return t("event_start_failed");
    case "DiskWarning":
      return t("event_disk_warning");
    case "DiskFull":
      return t("event_disk_full");
    case "Crashed": {
      const cause = crashCause(t, ev.reason, ev.exitCode ?? 0);
      if (!ev.stopping) {
        return ev.limit ? t("event_crashed_restart", { cause, n: ev.crashes, max: ev.limit }) : t("event_crashed_restart_any", { cause });
      }
      return t(ev.limit ? "event_crashed_stop_limit" : "event_crashed_stop_off", { cause, n: ev.crashes, max: ev.limit });
    }
  }
}

const PROBLEM: ReadonlySet<ServerEvent["type"]> = new Set(["Crashed", "HostRestarted", "StartFailed", "DiskWarning", "DiskFull"]);

/** RecoveryNotice explains a platform-made stop or recent crashes of a running server. */
export function RecoveryNotice({ data }: { data: ServerStatus }) {
  const { t, i18n } = useTranslation("servers");
  const titleId = useId();
  const stop = data.desiredState === "Stopped" ? data.autoStop : undefined;
  const crash = data.lastCrash;
  const when = (iso: string) => formatRelative(iso, Date.now(), i18n.language);

  if (stop && stop.reason !== "Idle") {
    const disk = stop.reason === "DiskFull";
    const crashed = !disk && stop.reason !== "HostRestarted";
    const at = disk ? stop.at : crash?.at ?? stop.at;
    const title = t(disk ? "autostop_disk_title" : crashed ? (stop.reason === "CrashLimit" ? "autostop_crash_limit_title" : "autostop_crashed_title") : "autostop_host_title");
    const body = disk ? t("autostop_disk_body", { when: when(stop.at) }) : crashed
      ? t(stop.reason === "CrashLimit" ? "autostop_crash_limit_body" : "autostop_crashed_body", {
          cause: crash ? crashCause(t, crash.reason, crash.exitCode) : t("crash_cause_unknown"),
          when: when(crash?.at ?? stop.at),
          max: data.maxCrashRestarts,
        })
      : t("autostop_host_body", { when: when(stop.at) });
    return (
      <div role="status" aria-labelledby={titleId}
        className="flex shrink-0 items-start gap-3 rounded-lg border border-red-500/40 bg-red-500/10 p-4 text-sm">
        <PowerOff className="mt-0.5 h-5 w-5 shrink-0 text-red-600 dark:text-red-400" />
        <div className="min-w-0 space-y-1">
          <p id={titleId} className="font-medium text-foreground">{title}</p>
          <p className="text-muted-foreground" title={formatAbsolute(at, i18n.language)}>{body}</p>
          <p className="text-xs text-muted-foreground">{t(disk ? (data.diskFull ? "disk_full_blocked" : "autostop_disk_next") : crashed ? "autostop_crash_next" : "autostop_host_next")}</p>
        </div>
      </div>
    );
  }
  if (data.phase === "Running" && data.recentCrashes && crash) {
    return (
      <div role="status" className="flex shrink-0 items-start gap-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-4 text-sm">
        <RotateCcw className="mt-0.5 h-5 w-5 shrink-0 text-amber-600 dark:text-amber-400" />
        <p className="text-muted-foreground" title={formatAbsolute(crash.at, i18n.language)}>
          {t(data.restartOnCrash ? "recent_crashes" : "recent_crashes_any", {
            n: data.recentCrashes,
            max: data.maxCrashRestarts,
            cause: crashCause(t, crash.reason, crash.exitCode),
            when: when(crash.at),
          })}
        </p>
      </div>
    );
  }
  return null;
}

const SHOWN_EVENTS = 8;

export function ServerEventsCard({ events }: { events: ServerEvent[] }) {
  const { t, i18n } = useTranslation("servers");
  const titleId = useId();
  const latest = events.slice(-SHOWN_EVENTS).reverse();
  return (
    <section aria-labelledby={titleId} className="rounded-xl border border-border bg-card p-4">
      <h2 id={titleId} className="mb-3 flex items-center gap-2 text-sm font-medium">
        <History className="h-4 w-4 text-primary" />
        {t("events_title")}
      </h2>
      <ol className="space-y-2.5">
        {latest.map((ev) => (
          <li key={ev.seq} className="flex gap-2.5 text-xs">
            <span aria-hidden="true" className={cn("mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full", PROBLEM.has(ev.type) || ev.stopping ? "bg-red-500" : "bg-muted-foreground/50")} />
            <span className="min-w-0 flex-1">
              <span className="block leading-relaxed text-foreground">{eventText(t, ev)}</span>
              <time dateTime={ev.at} title={formatAbsolute(ev.at, i18n.language)} className="text-muted-foreground">
                {formatRelative(ev.at, Date.now(), i18n.language)}
              </time>
            </span>
          </li>
        ))}
      </ol>
    </section>
  );
}
