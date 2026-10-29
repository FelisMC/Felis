import { Badge } from "@/components/ui/badge";
import type { Phase } from "@/lib/types";
import { cn } from "@/lib/utils";
import { useTranslation } from "react-i18next";

export type BadgeVariant = "default" | "muted" | "destructive" | "outline";

// Keep phase colors aligned with the voxel palette.
export const PHASE_COLOR: Record<Phase, string> = {
  Running: "#22c55e",
  Starting: "#eab308",
  Stopping: "#f97316",
  Stopped: "#64748b",
  Failed: "#ef4444",
  Unknown: "#94a3b8",
  Hibernating: "#818cf8",
  Hibernated: "#818cf8",
};

export const PHASE_KEY: Record<Phase, string> = {
  Running: "servers:phase_running",
  Starting: "servers:phase_starting",
  Stopping: "servers:phase_stopping",
  Stopped: "servers:phase_stopped",
  Failed: "servers:phase_failed",
  Unknown: "servers:phase_unknown",
  Hibernating: "servers:phase_hibernating",
  Hibernated: "servers:phase_hibernated",
};

const VARIANT: Record<Phase, BadgeVariant> = {
  Running: "default",
  Starting: "outline",
  Stopping: "outline",
  Stopped: "muted",
  Failed: "destructive",
  Unknown: "muted",
  Hibernating: "outline",
  Hibernated: "muted",
};

// Phases that should pulse: the in-flight transitions a pod is moving through.
const TRANSIENT: ReadonlySet<Phase> = new Set<Phase>(["Starting", "Stopping", "Hibernating"]);

// Unvalidated phases fall back to neutral Unknown styling.
export function phaseColor(phase: Phase): string {
  return PHASE_COLOR[phase] ?? PHASE_COLOR.Unknown;
}

export function phaseVariant(phase: Phase): BadgeVariant {
  return VARIANT[phase] ?? VARIANT.Unknown;
}

/** MAX_AUTO_RESTARTS mirrors the operator's v1alpha1.MaxAutoRestarts: how often it
 *  recreates the pod of a start that timed out before leaving it Failed. */
export const MAX_AUTO_RESTARTS = 3;

export type StartFailure = "retrying" | "gaveUp";

// Hide start-failure detail for public views and servers requested to stop.
export function startFailure(s: {
  phase?: Phase;
  desiredState?: string;
  startGaveUp?: boolean;
}): StartFailure | null {
  if (s.phase !== "Failed" || s.desiredState !== "Running") return null;
  return s.startGaveUp ? "gaveUp" : "retrying";
}

/** PendingPower is a server asked to move that has not got there yet: asked to
 *  run with no pod up ("start"), or asked to stop while still up ("stop"). */
export type PendingPower = "start" | "stop";

// Compare phase with desiredState; public views without desiredState show no pending transition.
export function pendingPower(s: { phase: Phase; desiredState?: string }): PendingPower | null {
  if (s.desiredState === "Running" && (s.phase === "Stopped" || s.phase === "Stopping" || s.phase === "Unknown" || s.phase === "Hibernated")) {
    return "start";
  }
  if (s.desiredState === "Stopped" && (s.phase === "Running" || s.phase === "Starting" || s.phase === "Hibernated" || s.phase === "Hibernating")) {
    return "stop";
  }
  return null;
}

// Display accepted start/stop intent before the operator updates phase.
export function shownPhase(s: { phase: Phase; desiredState?: string }): Phase {
  const pending = pendingPower(s);
  return pending === "start" ? "Starting" : pending === "stop" ? "Stopping" : s.phase;
}

export function PhaseBadge({
  phase,
  failure = null,
  autoRestarts = 0,
}: {
  phase: Phase;
  /** From startFailure: a Failed start between automatic retries pulses and says so. */
  failure?: StartFailure | null;
  autoRestarts?: number;
}) {
  const { t } = useTranslation();
  const retrying = phase === "Failed" && failure === "retrying";
  const hint =
    phase !== "Failed" || failure === null
      ? undefined
      : retrying
        ? t("servers:server_failed_retrying_body", { used: autoRestarts, max: MAX_AUTO_RESTARTS })
        : t("servers:server_failed_body");
  return (
    <Badge variant={phaseVariant(phase)} className="gap-1.5 whitespace-nowrap" title={hint}>
      <span
        className={cn(
          "h-1.5 w-1.5 rounded-full",
          (TRANSIENT.has(phase) || retrying) && "animate-pulse",
        )}
        style={{ backgroundColor: phaseColor(phase) }}
      />
      {retrying ? t("servers:phase_failed_retrying") : t(PHASE_KEY[phase] ?? PHASE_KEY.Unknown)}
    </Badge>
  );
}
