import { AlertTriangle, Package, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { softwareSummary } from "@/components/EditServerDialog";
import type { ServerSoftware } from "@/lib/types";

/** SoftwareCard names the server software a runtime-image server runs and
 *  whether the proxy's player identity reaches it. */
export function SoftwareCard({ software }: { software: ServerSoftware }) {
  const { t } = useTranslation("servers");
  return (
    <div className="flex items-start gap-3 rounded-xl border border-border bg-card p-4">
      <Package className="h-5 w-5 shrink-0 text-primary" />
      <div className="min-w-0 flex-1 space-y-1.5">
        <p className="text-sm font-medium">{t("software_card_title")}</p>
        <p className="text-xs text-muted-foreground">
          {softwareSummary(t, software)} · {t("software_java_option", { version: software.java })}
        </p>
        {software.forwarding === "velocity" ? (
          <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
            <ShieldCheck className="mt-px h-3.5 w-3.5 shrink-0 text-emerald-600 dark:text-emerald-400" />
            {t("software_identity_velocity")}
          </p>
        ) : (
          <p role="note" className="flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
            <span>
              <span className="font-medium">{t("software_identity_none_badge")}</span>
              {" · "}
              {t(software.type === "custom" ? "software_identity_none_custom" : "software_identity_none")}
            </span>
          </p>
        )}
      </div>
    </div>
  );
}
