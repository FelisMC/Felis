import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Terminal, Moon, ShieldAlert, HelpCircle, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { PhaseBadge } from "@/components/PhaseBadge";
import { LogConsole } from "@/components/LogConsole";
import { Loading, ErrorState } from "@/components/States";
import { api, consoleStreamURL } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";
import { hostFor } from "@/lib/config";
import type { Phase } from "@/lib/types";

// notStreamingCopy explains why there is no live feed for a phase that has no
// streamable pod. The read path only has something to relay once a pod is up, so
// Stopped/Failed/Stopping each get their own honest line rather than an empty
// console. `default` covers Unknown plus any future phase the backend may emit
// that the panel hasn't modelled yet — the screen stays informative regardless.
function useNotStreamingCopy(phase: Phase): { icon: LucideIcon; title: string; body: string } {
  const { t } = useTranslation("servers");
  switch (phase) {
    case "Stopped":
      return {
        icon: Moon,
        title: t("server_asleep_title"),
        body: t("server_asleep_body"),
      };
    case "Stopping":
      return {
        icon: Moon,
        title: t("server_shutting_down_title"),
        body: t("server_shutting_down_body"),
      };
    case "Failed":
      return {
        icon: ShieldAlert,
        title: t("server_failed_title"),
        body: t("server_failed_body"),
      };
    default:
      return {
        icon: HelpCircle,
        title: t("console_offline_title"),
        body: t("console_offline_body"),
      };
  }
}

function NotStreaming({ phase }: { phase: Phase }) {
  const { icon: Icon, title, body } = useNotStreamingCopy(phase);
  return (
    <div className="flex items-start gap-3 rounded-md border border-dashed border-border bg-muted/30 p-6 text-sm text-muted-foreground">
      <Icon className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground/70" />
      <div className="space-y-1">
        <p className="font-medium text-foreground">{title}</p>
        <p>{body}</p>
      </div>
    </div>
  );
}

export function ServerConsole() {
  const { name = "" } = useParams();
  const cfg = useConfig();
  const { t } = useTranslation("servers");
  const { data, error, loading, reload } = useAsync(
    () => api.status(name),
    [name],
  );

  // The read-side stream (spec §8) follows the running pod's log. A pod exists
  // during BOTH Starting and Running — the operator marks Starting once the pod
  // is up but RCON is not yet reachable (it only flips to Running after RCON
  // readiness). Boot logs flow precisely in that Starting window, which is when a
  // read most wants them, so the gate streams for both, not Running alone.
  const streamable = data?.phase === "Running" || data?.phase === "Starting";

  return (
    <>
      <Link
        to="/servers"
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="h-4 w-4" /> {t("my_servers_breadcrumb")}
      </Link>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : data ? (
        <>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex items-center gap-3">
              <Terminal className="h-6 w-6 text-primary" />
              <div>
                <h1 className="text-2xl font-semibold tracking-tight">
                  {data.displayName || data.name}
                </h1>
                {cfg && (
                  <p className="text-sm text-muted-foreground">
                    {hostFor(data.subdomain, cfg)}
                  </p>
                )}
              </div>
            </div>
            <div className="flex items-center gap-2">
              <PhaseBadge phase={data.phase} />
              {streamable ? (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => api.stop(name).then(reload)}
                >
                  {t("stop")}
                </Button>
              ) : (
                <Button size="sm" onClick={() => api.wake(name).then(reload)}>
                  {t("wake")}
                </Button>
              )}
            </div>
          </div>

          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2 text-base">
                <Terminal className="h-4 w-4" /> {t("console_card_title")}
              </CardTitle>
              <p className="text-sm text-muted-foreground">
                {t("console_card_desc")}
              </p>
            </CardHeader>
            <CardContent>
              {!streamable ? (
                <NotStreaming phase={data.phase} />
              ) : cfg ? (
                // key on name so navigating between servers remounts the viewport
                // (fresh stream + scroll state). useLogStream also resets its
                // controller when the url changes, so this is belt-and-suspenders.
                <LogConsole key={name} url={consoleStreamURL(cfg.apiBase, name)} />
              ) : (
                <Loading />
              )}
            </CardContent>
          </Card>
        </>
      ) : null}
    </>
  );
}
