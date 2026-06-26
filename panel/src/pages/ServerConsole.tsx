import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Terminal, Moon, ShieldAlert, HelpCircle, type LucideIcon } from "lucide-react";
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
function notStreamingCopy(phase: Phase): { icon: LucideIcon; title: string; body: string } {
  switch (phase) {
    case "Stopped":
      return {
        icon: Moon,
        title: "Server is asleep",
        body: "Wake it to boot a pod — the live console attaches automatically the moment it starts up.",
      };
    case "Stopping":
      return {
        icon: Moon,
        title: "Server is shutting down",
        body: "The pod is terminating, so the live console has detached. It will be asleep in a moment.",
      };
    case "Failed":
      return {
        icon: ShieldAlert,
        title: "Server failed to start",
        body: "No pod is running, so there is nothing to stream. Wake it to retry; the console attaches once a pod comes back up.",
      };
    default:
      return {
        icon: HelpCircle,
        title: "Console is offline",
        body: "The live console attaches automatically as soon as the server is running.",
      };
  }
}

function NotStreaming({ phase }: { phase: Phase }) {
  const { icon: Icon, title, body } = notStreamingCopy(phase);
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
    <div className="mx-auto max-w-4xl space-y-6">
      <Link
        to="/servers"
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="h-4 w-4" /> My servers
      </Link>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : data ? (
        <>
          <div className="flex flex-wrap items-center justify-between gap-3">
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
            <div className="flex items-center gap-2">
              <PhaseBadge phase={data.phase} />
              {streamable ? (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => api.stop(name).then(reload)}
                >
                  Stop
                </Button>
              ) : (
                <Button size="sm" onClick={() => api.wake(name).then(reload)}>
                  Wake
                </Button>
              )}
            </div>
          </div>

          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2 text-base">
                <Terminal className="h-4 w-4" /> Console
              </CardTitle>
              <p className="text-sm text-muted-foreground">
                Live, read-only output streamed from the running pod over SSE
                (spec §8). Commands run through a separate path — the panel never
                holds an RCON password.
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
    </div>
  );
}
