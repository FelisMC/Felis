import { useState } from "react";
import { Loader2, Play, Square } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { api, humanizeError } from "@/lib/api";
import { cn } from "@/lib/utils";

interface Props {
  name: string;
  /** A pod is up or on its way (Starting/Running/Stopping): offer Stop. */
  live: boolean;
  playersOnline?: number;
  /** The operator cannot read the player count, so players may be online. */
  playerCountUnknown?: boolean;
  /** Called after the wake or stop was accepted, so the parent refetches. */
  onChanged: () => void;
  size?: "sm" | "default";
  className?: string;
}

// PowerButton starts or stops one server. It is busy while the call runs (no
// double send), shows why a call was refused (quota, cooldown, a phase that
// moved on), and asks before a stop that would disconnect players: the count
// is in the question, and an unreadable count asks too.
export function PowerButton({
  name,
  live,
  playersOnline,
  playerCountUnknown,
  onChanged,
  size = "sm",
  className,
}: Props) {
  const { t } = useTranslation("servers");
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function run(kind: "wake" | "stop") {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await (kind === "wake" ? api.wake(name) : api.stop(name));
      setConfirming(false);
      onChanged();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }

  const players = playersOnline ?? 0;
  const askFirst = players > 0 || playerCountUnknown === true;
  const spinner = <Loader2 className="animate-spin" />;

  let control;
  if (!live) {
    control = (
      <Button size={size} onClick={() => void run("wake")} disabled={busy}>
        {busy ? spinner : <Play />}
        {busy ? t("waking") : t("wake")}
      </Button>
    );
  } else if (confirming) {
    control = (
      <div role="alert" className="flex flex-wrap items-center justify-end gap-2">
        <span className="text-xs font-medium text-foreground">
          {players > 0
            ? t("stop_confirm_players", { count: players })
            : t("stop_confirm_unknown")}
        </span>
        <Button size={size} variant="ghost" onClick={() => setConfirming(false)} disabled={busy}>
          {t("common:cancel")}
        </Button>
        <Button size={size} variant="destructive" onClick={() => void run("stop")} disabled={busy}>
          {busy ? spinner : <Square />}
          {busy ? t("stopping") : t("stop")}
        </Button>
      </div>
    );
  } else {
    control = (
      <Button
        size={size}
        variant="destructive"
        onClick={() => (askFirst ? setConfirming(true) : void run("stop"))}
        disabled={busy}
      >
        {busy ? spinner : <Square />}
        {busy ? t("stopping") : t("stop")}
      </Button>
    );
  }

  return (
    <div className={cn("flex flex-col items-end gap-1", className)}>
      {control}
      {error && (
        <p role="alert" className="max-w-xs text-right text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}
