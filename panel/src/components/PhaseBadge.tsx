import type { Phase } from "@/lib/types";
import { cn } from "@/lib/utils";
import { useTranslation } from "react-i18next";

export type BadgeVariant = "default" | "muted" | "destructive" | "outline";

// Keep phase colors aligned with the voxel palette.
export const PHASE_COLOR: Record<Phase, string> = {
  Running: "#2eb86b",
  Starting: "#eab308",
  Stopping: "#f97316",
  Stopped: "#71717a",
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

const TRANSIENT: ReadonlySet<Phase> = new Set<Phase>(["Starting", "Stopping", "Hibernating"]);

export function phaseColor(phase: Phase): string {
  return PHASE_COLOR[phase] ?? PHASE_COLOR.Unknown;
}

export function phaseVariant(phase: Phase): BadgeVariant {
  return VARIANT[phase] ?? VARIANT.Unknown;
}

export const MAX_AUTO_RESTARTS = 3;

export type StartFailure = "retrying" | "gaveUp";

export function startFailure(s: {
  phase?: Phase;
  desiredState?: string;
  startGaveUp?: boolean;
}): StartFailure | null {
  if (s.phase !== "Failed" || s.desiredState !== "Running") return null;
  return s.startGaveUp ? "gaveUp" : "retrying";
}

export type PendingPower = "start" | "stop";

export function pendingPower(s: { phase: Phase; desiredState?: string }): PendingPower | null {
  if (s.desiredState === "Running" && (s.phase === "Stopped" || s.phase === "Stopping" || s.phase === "Unknown" || s.phase === "Hibernated")) {
    return "start";
  }
  if (s.desiredState === "Stopped" && (s.phase === "Running" || s.phase === "Starting" || s.phase === "Hibernated" || s.phase === "Hibernating")) {
    return "stop";
  }
  return null;
}

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
    <span
      className={cn(
        "inline-flex items-center gap-1.5 whitespace-nowrap text-xs",
        phase === "Failed" ? "text-destructive" : "text-muted-foreground",
      )}
      title={hint}
    >
      <span
        aria-hidden="true"
        className={cn(
          "h-[5.5px] w-[5.5px] shrink-0 rounded-full",
          (TRANSIENT.has(phase) || retrying) && "animate-pulse",
        )}
        style={{ backgroundColor: phaseColor(phase) }}
      />
      {retrying ? t("servers:phase_failed_retrying") : t(PHASE_KEY[phase] ?? PHASE_KEY.Unknown)}
    </span>
  );
}
