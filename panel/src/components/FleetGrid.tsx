import { Link } from "react-router-dom";
import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";
import { PHASE_KEY, phaseColor } from "@/components/PhaseBadge";
import { cn } from "@/lib/utils";
import type { Phase, MyServerView } from "@/lib/types";

// FleetGrid is the flat stand-in for VoxelFleet when WebGL is unavailable or the
// 3D chunk failed: the same square layout and phase palette seen from above, so
// the legend under the card still reads correctly. It lives outside the three.js
// chunk so it renders even when that chunk cannot load.

const TRANSIENT: ReadonlySet<Phase> = new Set<Phase>(["Starting", "Stopping"]);

export type FleetGridReason = "webgl" | "error";

export function FleetGrid({
  servers,
  reason,
}: {
  servers: Pick<MyServerView, "name" | "displayName" | "phase">[];
  reason: FleetGridReason;
}) {
  const { t } = useTranslation(["dashboard", "servers"]);
  const cols = Math.max(1, Math.ceil(Math.sqrt(servers.length)));
  const n = servers.length;
  const tile = n <= 9 ? "3rem" : n <= 36 ? "1.75rem" : n <= 100 ? "1.25rem" : "0.875rem";

  return (
    <div className="absolute inset-0 flex flex-col items-center justify-center gap-4 overflow-auto p-4">
      {servers.length === 0 ? (
        <p className="text-xs text-muted-foreground">{t("dashboard:fleet_flat_empty")}</p>
      ) : (
        <div
          className="grid gap-2"
          style={{ gridTemplateColumns: `repeat(${cols}, ${tile})`, gridAutoRows: tile }}
        >
          {servers.map((s) => {
            const phase = s.phase ?? "Unknown";
            const color = phaseColor(phase);
            const label = `${s.displayName || s.name} · ${t(PHASE_KEY[phase] ?? PHASE_KEY.Unknown)}`;
            return (
              <Link
                key={s.name}
                to={`/servers/${s.name}`}
                title={label}
                aria-label={label}
                className={cn(
                  "rounded-md ring-1 ring-inset ring-white/10 transition-transform hover:scale-110 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  TRANSIENT.has(phase) && "animate-pulse",
                )}
                style={{
                  backgroundColor: color,
                  backgroundImage: "linear-gradient(145deg, rgb(255 255 255 / 0.28), rgb(0 0 0 / 0.18))",
                  boxShadow: s.phase === "Running" ? `0 0 14px -2px ${color}` : undefined,
                }}
              />
            );
          })}
        </div>
      )}
      <p className="flex max-w-xs items-center gap-1.5 text-center text-[11px] leading-snug text-muted-foreground">
        <Info className="h-3.5 w-3.5 shrink-0" />
        {reason === "webgl" ? t("dashboard:fleet_flat_webgl") : t("dashboard:fleet_flat_error")}
      </p>
    </div>
  );
}
