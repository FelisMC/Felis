import { Badge } from "@/components/ui/badge";
import type { Phase } from "@/lib/types";
import { cn } from "@/lib/utils";
import { useTranslation } from "react-i18next";

export type BadgeVariant = "default" | "muted" | "destructive" | "outline";

/** PHASE_COLOR maps a lifecycle Phase to a foreground hue. Kept in sync with the
 *  voxel palette (VoxelFleet reads it through phaseColor) so the 3D scene and the
 *  list read identically. "Stopping" is orange — a teardown sibling to Starting's
 *  amber, distinct from Stopped's slate so the transient state is legible. */
export const PHASE_COLOR: Record<Phase, string> = {
  Running: "#22c55e",
  Starting: "#eab308",
  Stopping: "#f97316",
  Stopped: "#64748b",
  Failed: "#ef4444",
  Unknown: "#94a3b8",
};

export const PHASE_KEY: Record<Phase, string> = {
  Running: "servers:phase_running",
  Starting: "servers:phase_starting",
  Stopping: "servers:phase_stopping",
  Stopped: "servers:phase_stopped",
  Failed: "servers:phase_failed",
  Unknown: "servers:phase_unknown",
};

const VARIANT: Record<Phase, BadgeVariant> = {
  Running: "default",
  Starting: "outline",
  Stopping: "outline",
  Stopped: "muted",
  Failed: "destructive",
  Unknown: "muted",
};

// Phases that should pulse: the two in-flight transitions a pod is moving through.
const TRANSIENT: ReadonlySet<Phase> = new Set<Phase>(["Starting", "Stopping"]);

/** phaseColor / phaseVariant resolve a Phase to its hue / Badge variant, degrading
 *  to the Unknown styling for any value outside the modelled union. `phase` arrives
 *  from felis-api as parsed-but-unvalidated JSON, so a raw map index can be
 *  undefined when the backend emits a phase the panel hasn't modelled yet; the
 *  fallback keeps the badge and the voxel scene rendering a sane neutral instead. */
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

/** startFailure reads how a Failed start stands from the owner detail. "retrying"
 *  means the operator's next automatic attempt is coming; "gaveUp" means nothing
 *  will start it again until a person does. A Failed server meant to stop, or a
 *  public view without the owner detail, reads as null: there is no retry to
 *  offer there, and the badge stays the plain Failed one. */
export function startFailure(s: {
  phase?: Phase;
  desiredState?: string;
  startGaveUp?: boolean;
}): StartFailure | null {
  if (s.phase !== "Failed" || s.desiredState !== "Running") return null;
  return s.startGaveUp ? "gaveUp" : "retrying";
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
