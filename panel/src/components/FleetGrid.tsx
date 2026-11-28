import { Link } from "react-router-dom";
import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";
import { PHASE_KEY, phaseColor } from "@/components/PhaseBadge";
import { cn } from "@/lib/utils";
import type { Phase, MyServerView } from "@/lib/types";

const TRANSIENT: ReadonlySet<Phase> = new Set<Phase>(["Starting", "Stopping", "Hibernating"]);

export type FleetGridReason = "webgl" | "error";

export function FleetGrid({
  servers,
  reason,
}: {
  servers: Pick<MyServerView, "name" | "displayName" | "phase">[];
  reason?: FleetGridReason;
}) {
  const { t } = useTranslation(["dashboard", "servers"]);

  return (
    <div className="flex flex-col gap-3 p-3.5">
      {servers.length === 0 ? (
        <p className="py-4 text-center text-xs text-muted-foreground">{t("dashboard:fleet_flat_empty")}</p>
      ) : (
        <div className="flex flex-wrap gap-1.5">
          {servers.map((s, idx) => {
            const phase = s.phase ?? "Unknown";
            const color = phaseColor(phase);
            const label = `${s.displayName || s.name} · ${t(PHASE_KEY[phase] ?? PHASE_KEY.Unknown)}`;
            const transient = TRANSIENT.has(phase);
            return (
              <Link
                key={s.name}
                to={`/servers/${s.name}`}
                title={label}
                aria-label={label}
                className={cn(
                  "h-4 w-4 rounded-[3.5px] ring-1 ring-inset ring-black/10 transition-[transform,box-shadow,filter] duration-150 ease-out hover:z-10 hover:scale-125 hover:brightness-110 hover:ring-foreground/40 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring dark:ring-white/10",
                  transient && "animate-status-breathe",
                )}
                style={{
                  backgroundColor: color,
                  animationDelay: transient ? `${(idx % 5) * 320}ms` : undefined,
                }}
              />
            );
          })}
        </div>
      )}
      {reason && (
        <p className="flex items-center gap-1.5 text-[11px] leading-snug text-muted-foreground">
          <Info className="h-3.5 w-3.5 shrink-0" />
          {reason === "webgl" ? t("dashboard:fleet_flat_webgl") : t("dashboard:fleet_flat_error")}
        </p>
      )}
    </div>
  );
}
