import { useEffect, useRef, useState } from "react";
import { Loader2, Play, RotateCcw, Square } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { pendingPower } from "@/components/PhaseBadge";
import { RetiringBadge } from "@/components/Retirement";
import { api, humanizeError } from "@/lib/api";
import type { DesiredState, Phase, RetireState } from "@/lib/types";
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
  desiredState?: DesiredState;
  /** Its start Failed while meant to run: offer a retry and a stop. */
  failed?: boolean;
  playersOnline?: number;
  /** The operator cannot read the player count, so players may be online. */
  playerCountUnknown?: boolean;
  /** A pending retirement: nothing starts the server until it is cancelled, so
   *  the control gives way to a badge saying why. */
  retiring?: RetireState;
  diskFull?: boolean;
  /** Offer only ways down: a page that needs the server stopped (the file
   *  editor) never offers a wake or a retry, and a server it cannot place gets
   *  a Stop too. */
  stopOnly?: boolean;
  allowRestart?: boolean;
  /** Called after the wake or stop was accepted, so the parent refetches. */
  onChanged: () => void;
  size?: "sm" | "default";
  className?: string;
}

function isUp(phase: Phase): boolean {
  return phase === "Running" || phase === "Starting";
}

// Confirm stops when players may be disconnected, including when the count is unknown.
// Keep accepted actions pending until polling sees the state change or the hold expires.
export function PowerButton({
  name,
  phase,
  desiredState,
  failed = false,
  playersOnline,
  playerCountUnknown,
  retiring,
  diskFull = false,
  stopOnly = false,
  allowRestart = false,
  onChanged,
  size = "sm",
  className,
}: Props) {
  const { t } = useTranslation("servers");
  const [busy, setBusy] = useState<"wake" | "stop" | "restart" | null>(null);
  const [submitted, setSubmitted] = useState<"wake" | "stop" | "restart" | null>(null);
  const [confirming, setConfirming] = useState<"stop" | "restart" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const changed = useRef(onChanged);
  useEffect(() => {
    changed.current = onChanged;
  }, [onChanged]);

  const pending = pendingPower({ phase, desiredState });
  const on = desiredState ? desiredState === "Running" : isUp(phase);

  const caughtUp = (diskFull && submitted !== "stop") || (submitted === "wake" ? on && !failed : submitted === "stop" ? !on : submitted === "restart" ? phase !== "Running" : true);
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

  async function run(kind: "wake" | "stop" | "restart") {
    if (busy || (diskFull && kind !== "stop")) return;
    setBusy(kind);
    setError(null);
    try {
      await api[kind](name);
      setConfirming(null);
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
  const inProgress = (kind: "wake" | "stop" | "restart") => (
    <Button size={size} variant={kind === "stop" ? "outline" : "default"} disabled>
      {spinner}
      {kind === "wake" ? t("waking") : kind === "restart" ? t("restarting") : t("stopping")}
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
    // Retry and stop stay side by side: a row of actions that wraps moves them as one.
    control = (
      <div className="flex items-center justify-end gap-2">
        <Button size={size} onClick={() => void run("wake")} disabled={busy !== null || diskFull}>
          {busy === "wake" ? spinner : <RotateCcw />}
          {busy === "wake" ? t("retrying_start") : t("retry_start")}
        </Button>
        {stopButton("outline", () => void run("stop"))}
      </div>
    );
  } else if (desiredState === "Hibernated" && phase !== "Hibernated") {
    control = (
      <Button size={size} variant="outline" disabled>
        {spinner}
        {t("hibernating")}
      </Button>
    );
  } else if (desiredState === "Hibernated") {
    control = (
      <div className="flex items-center justify-end gap-2">
        {!stopOnly && (
          <Button size={size} onClick={() => void run("wake")} disabled={busy !== null || diskFull}>
            {busy === "wake" ? spinner : <Play />}
            {busy === "wake" ? t("resuming") : t("resume")}
          </Button>
        )}
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
      <Button size={size} onClick={() => void run("wake")} disabled={busy !== null || diskFull}>
        {busy === "wake" ? spinner : <Play />}
        {busy === "wake" ? t("waking") : t("wake")}
      </Button>
    );
  } else if (confirming) {
    control = (
      <div role="alert" className="flex flex-wrap items-center justify-end gap-2">
        <span className="text-xs font-medium text-foreground">
          {players > 0
            ? t(`${confirming}_confirm_players`, { count: players })
            : t(`${confirming}_confirm_unknown`)}
        </span>
        <Button size={size} variant="ghost" onClick={() => setConfirming(null)} disabled={busy !== null}>
          {t("common:cancel")}
        </Button>
        <Button size={size} variant="destructive" onClick={() => void run(confirming)} disabled={busy !== null || (diskFull && confirming === "restart")}>
          {busy === confirming ? spinner : confirming === "restart" ? <RotateCcw /> : <Square />}
          {busy === confirming ? t(confirming === "restart" ? "restarting" : "stopping") : t(confirming)}
        </Button>
      </div>
    );
  } else {
    control = (
      <div className="flex flex-wrap items-center justify-end gap-2">
        {allowRestart && !stopOnly && phase === "Running" && (
          <Button size={size} variant="outline" onClick={() => (askFirst ? setConfirming("restart") : void run("restart"))} disabled={busy !== null || diskFull}>
            {busy === "restart" ? spinner : <RotateCcw />}
            {busy === "restart" ? t("restarting") : t("restart")}
          </Button>
        )}
        {stopButton("destructive", () => (askFirst ? setConfirming("stop") : void run("stop")))}
      </div>
    );
  }

  return (
    <div className={cn("flex flex-col items-end gap-1", className)}>
      {control}
      {diskFull && !stopOnly && !retiring && <p role="status" className="max-w-xs text-right text-xs text-destructive">{t("disk_full_blocked")}</p>}
      {error && (
        <p role="alert" className="max-w-xs text-right text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}
