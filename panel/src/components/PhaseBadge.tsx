import { Badge } from "@/components/ui/badge";
import type { Phase } from "@/lib/types";
import { cn } from "@/lib/utils";

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

export function PhaseBadge({ phase }: { phase: Phase }) {
  return (
    <Badge variant={phaseVariant(phase)} className="gap-1.5">
      <span
        className={cn(
          "h-1.5 w-1.5 rounded-full",
          TRANSIENT.has(phase) && "animate-pulse",
        )}
        style={{ backgroundColor: phaseColor(phase) }}
      />
      {phase}
    </Badge>
  );
}
