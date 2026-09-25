import { useState } from "react";
import { Link } from "react-router-dom";
import { Hand, Users, ExternalLink, Terminal } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { PhaseBadge } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
import { api, humanizeError } from "@/lib/api";
import { hostFor, type RuntimeConfig } from "@/lib/config";
import type { MyServerView } from "@/lib/types";

interface Props {
  server: MyServerView;
  cfg: RuntimeConfig;
  /** Called after a successful mutation so the parent can refetch. */
  onChanged: () => void;
}

export function ServerCard({ server, cfg, onChanged }: Props) {
  const { t } = useTranslation("servers");
  const [busy, setBusy] = useState<null | "claim">(null);
  const [error, setError] = useState<string | null>(null);
  const host = hostFor(server.subdomain, cfg);

  async function act(kind: "claim", fn: () => Promise<unknown>) {
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
    <Card className="flex flex-col p-4 transition-colors hover:border-primary/40">
      {/* Header Row */}
      <div className="flex items-center justify-between gap-2">
        <span className="truncate text-sm font-semibold text-foreground">
          {server.displayName || server.name}
        </span>
        <PhaseBadge phase={server.phase ?? "Unknown"} />
      </div>

      {/* Info Row */}
      <div className="mt-2 flex items-center justify-between text-xs text-muted-foreground">
        <a
          href={`https://${host}`}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-1 hover:text-foreground truncate max-w-[70%]"
        >
          {host}
          <ExternalLink className="h-3 w-3 shrink-0" />
        </a>
        <div className="flex items-center gap-1.5 shrink-0 mr-1">
          <span>
            {running
              ? `${server.playersOnline ?? 0}${server.playersMax ? `/${server.playersMax}` : ""}`
              : "—"}
          </span>
          <Users className="h-3.5 w-3.5" />
        </div>
      </div>

      {error && <p className="mt-2 text-xs text-destructive">{error}</p>}

      {/* Action Row */}
      <div className="mt-4 flex flex-wrap gap-2 border-t border-border/50 pt-3">
        {server.owned && (
          <Link to={`/servers/${server.name}`}>
            <Button variant="outline" size="sm">
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
        {server.owned && (
          <PowerButton
            name={server.name}
            live={running || transitioning}
            playersOnline={server.playersOnline}
            playerCountUnknown={server.playerCountUnknown}
            onChanged={onChanged}
          />
        )}
      </div>
    </Card>
  );
}
