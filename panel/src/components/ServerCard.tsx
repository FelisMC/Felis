import { useState } from "react";
import { Link } from "react-router-dom";
import { Play, Square, Hand, Users, ExternalLink, Terminal } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardFooter, CardHeader } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { PhaseBadge } from "@/components/PhaseBadge";
import { api, humanizeError } from "@/lib/api";
import { hostFor, type RuntimeConfig } from "@/lib/config";
import type { ServerInfo } from "@/lib/types";

interface Props {
  server: ServerInfo;
  cfg: RuntimeConfig;
  /** Called after a successful mutation so the parent can refetch. */
  onChanged: () => void;
}

export function ServerCard({ server, cfg, onChanged }: Props) {
  const { t } = useTranslation("servers");
  const [busy, setBusy] = useState<null | "wake" | "stop" | "claim">(null);
  const [error, setError] = useState<string | null>(null);
  const host = hostFor(server.subdomain, cfg);

  async function act(kind: "wake" | "stop" | "claim", fn: () => Promise<unknown>) {
    setBusy(kind);
    setError(null);
    try {
      await fn();
      onChanged();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(null);
    }
  }

  const running = server.phase === "Running";
  // Starting and Stopping both have (or had) a pod in flight — neither should
  // offer "Wake". Grouping them keeps the footer on the Stop side until the
  // server has fully settled into Stopped/Failed.
  const transitioning = server.phase === "Starting" || server.phase === "Stopping";

  return (
    <Card className="flex flex-col transition-colors hover:border-primary/40">
      <CardHeader className="flex-row items-start justify-between space-y-0">
        <div className="min-w-0">
          <div className="truncate font-semibold">
            {server.displayName || server.name}
          </div>
          <a
            href={`https://${host}`}
            target="_blank"
            rel="noreferrer"
            className="mt-0.5 inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            {host}
            <ExternalLink className="h-3 w-3" />
          </a>
        </div>
        <PhaseBadge phase={server.phase} />
      </CardHeader>

      <CardContent className="flex-1 text-sm text-muted-foreground">
        <div className="flex items-center gap-1.5">
          <Users className="h-3.5 w-3.5" />
          {running ? (
            <span>
              {server.players ?? 0}
              {server.maxPlayers ? ` / ${server.maxPlayers}` : ""} {t("online")}
            </span>
          ) : (
            <span>—</span>
          )}
        </div>
        {error && <p className="mt-2 text-xs text-destructive">{error}</p>}
      </CardContent>

      <CardFooter className="flex flex-wrap gap-2">
        {server.owned && (
          <Link to={`/servers/${server.name}`}>
            <Button variant="ghost" size="sm">
              <Terminal /> {t("console")}
            </Button>
          </Link>
        )}
        {server.claimable && !server.owned && (
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null}
            onClick={() => act("claim", () => api.claim(server.name))}
          >
            <Hand /> {busy === "claim" ? t("claiming") : t("claim")}
          </Button>
        )}
        {server.owned &&
          (running || transitioning ? (
            <Button
              size="sm"
              variant="outline"
              disabled={busy !== null}
              onClick={() => act("stop", () => api.stop(server.name))}
            >
              <Square /> {busy === "stop" ? t("stopping") : t("stop")}
            </Button>
          ) : (
            <Button
              size="sm"
              disabled={busy !== null}
              onClick={() => act("wake", () => api.wake(server.name))}
            >
              <Play /> {busy === "wake" ? t("waking") : t("wake")}
            </Button>
          ))}
      </CardFooter>
    </Card>
  );
}
