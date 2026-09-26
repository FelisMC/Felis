import { useEffect, useRef, useState } from "react";
import { Loader2, Play, RotateCcw, Square } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { pendingPower } from "@/components/PhaseBadge";
import { RetiringBadge } from "@/components/Retirement";
import { api, humanizeError } from "@/lib/api";
import type { Phase, RetireState } from "@/lib/types";
import { cn } from "@/lib/utils";

/** How long a sent wake or stop shows as in progress when the view never
 *  reflects it (the parent stopped polling, or someone reversed it at once). */
export const SUBMITTED_HOLD_MS = 20_000;

/** How often a sent call asks the parent to refetch until its view shows it. */
export const SUBMITTED_RECHECK_MS = 2_000;

interface Props {
  name: string;
  phase: Phase;
  /** What the server was last asked to be. Absent in a view without it; the
   *  phase alone then decides. */
  desiredState?: "Running" | "Stopped";
  /** Its start Failed while meant to run: offer a retry and a stop. */
  failed?: boolean;
  playersOnline?: number;
  /** The operator cannot read the player count, so players may be online. */
  playerCountUnknown?: boolean;
  /** A pending retirement: nothing starts the server until it is cancelled, so
   *  the control gives way to a badge saying why. */
  retiring?: RetireState;
  /** Offer only ways down: a page that needs the server stopped (the file
   *  editor) never offers a wake or a retry, and a server it cannot place gets
   *  a Stop too. */
  stopOnly?: boolean;
  /** Called after the wake or stop was accepted, so the parent refetches. */
  onChanged: () => void;
  size?: "sm" | "default";
  className?: string;
}

function isUp(phase: Phase): boolean {
  return phase === "Running" || phase === "Starting";
}

// PowerButton starts or stops one server. It is busy while the call runs (no
// double send), shows why a call was refused (quota, cooldown, a phase that
// moved on), and asks before a stop that would disconnect players: the count
// is in the question, and an unreadable count asks too. A server whose start
// failed gets both ways out: retry (the wake, which felis-api turns into a fresh
// start) and stop. Nobody is on a server that never came up, so that stop does
// not ask.
//
// An accepted call keeps its spinner, and keeps asking the parent to reread,
// until the parent's view shows the server asked to move (the list and the
// console read a cache that lags the write by a moment), so nobody presses Wake
// again into a 429. A server on its way down offers nothing until it is down;
// one asked to run with no pod yet offers Stop, which is the way out when it
// never comes up.
// A server given up or being deleted offers nothing to press, only why.
// The file editor passes stopOnly: it needs the server down, so it offers Stop
// alone, with the same question before disconnecting players.
export function PowerButton({
  name,
  phase,
  desiredState,
  failed = false,
  playersOnline,
  playerCountUnknown,
  retiring,
  stopOnly = false,
  onChanged,
  size = "sm",
  className,
}: Props) {
  const { t } = useTranslation("servers");
  const [busy, setBusy] = useState<"wake" | "stop" | null>(null);
  const [submitted, setSubmitted] = useState<"wake" | "stop" | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const changed = useRef(onChanged);
  useEffect(() => {
    changed.current = onChanged;
  }, [onChanged]);

  const pending = pendingPower({ phase, desiredState });
  const on = desiredState ? desiredState === "Running" : isUp(phase);

  // A sent call is done once the view shows the lever moved: a wake when the
  // server is meant to run (a retry once it is no longer Failed), a stop when it
  // is meant to stop. The hold ends it anyway if the view never gets there.
  const caughtUp = submitted === "wake" ? on && !failed : submitted === "stop" ? !on : true;
  useEffect(() => {
    if (submitted === null) return;
    if (caughtUp) {
      setSubmitted(null);
      return;
    }
    const recheck = window.setInterval(() => changed.current(), SUBMITTED_RECHECK_MS);
    const hold = window.setTimeout(() => setSubmitted(null), SUBMITTED_HOLD_MS);
    return () => {
      window.clearInterval(recheck);
      window.clearTimeout(hold);
    };
  }, [submitted, caughtUp]);

  async function run(kind: "wake" | "stop") {
    if (busy) return;
    setBusy(kind);
    setError(null);
    try {
      await (kind === "wake" ? api.wake(name) : api.stop(name));
      setConfirming(false);
      setSubmitted(kind);
      onChanged();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(null);
    }
  }

  const players = playersOnline ?? 0;
  const askFirst = players > 0 || playerCountUnknown === true;
  const spinner = <Loader2 className="animate-spin" />;
  const stopButton = (variant: "destructive" | "outline", onClick: () => void) => (
    <Button size={size} variant={variant} onClick={onClick} disabled={busy !== null}>
      {busy === "stop" ? spinner : <Square />}
      {busy === "stop" ? t("stopping") : t("stop")}
    </Button>
  );
  // inProgress is a server on its way somewhere with nothing to press meanwhile.
  const inProgress = (kind: "wake" | "stop") => (
    <Button size={size} variant={kind === "stop" ? "outline" : "default"} disabled>
      {spinner}
      {kind === "wake" ? t("waking") : t("stopping")}
    </Button>
  );

  let control;
  if (retiring) {
    control = <RetiringBadge retiring={retiring} />;
  } else if (submitted !== null) {
    control = inProgress(submitted);
  } else if (failed && stopOnly) {
    control = stopButton("outline", () => void run("stop"));
  } else if (failed) {
    control = (
      <div className="flex flex-wrap items-center justify-end gap-2">
        <Button size={size} onClick={() => void run("wake")} disabled={busy !== null}>
          {busy === "wake" ? spinner : <RotateCcw />}
          {busy === "wake" ? t("retrying_start") : t("retry_start")}
        </Button>
        {stopButton("outline", () => void run("stop"))}
      </div>
    );
  } else if (pending === "stop" || (phase === "Stopping" && pending === null)) {
    control = inProgress("stop");
  } else if (!stopOnly && pending === "start" && phase === "Stopping") {
    // Woken while stopping: the operator brings it back up once it is down.
    control = inProgress("wake");
  } else if (!stopOnly && !on) {
    control = (
      <Button size={size} onClick={() => void run("wake")} disabled={busy !== null}>
        {busy === "wake" ? spinner : <Play />}
        {busy === "wake" ? t("waking") : t("wake")}
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
        <Button size={size} variant="ghost" onClick={() => setConfirming(false)} disabled={busy !== null}>
          {t("common:cancel")}
        </Button>
        {stopButton("destructive", () => void run("stop"))}
      </div>
    );
  } else {
    control = stopButton("destructive", () => (askFirst ? setConfirming(true) : void run("stop")));
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
