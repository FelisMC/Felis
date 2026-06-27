import { Link } from "react-router-dom";
import { Gauge, Network, ServerOff } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { PendingBackend } from "@/components/States";

// OpsOverview is the SysAdmin-Side landing: a platform-observability cockpit. It is
// admin-tier (same gate as Admin-Side) but a separate *concern* — read-mostly
// visibility across the whole platform, not content/server administration.
//
// The cluster-wide fleet rollup needs a fleet-wide read (GET /admin/servers) that
// does not exist yet — that is a separate Oracle-verifiable backend slice. Until
// then this surface is honest about the gap rather than charting fake data.
export function OpsOverview() {
  const { t } = useTranslation("ops");
  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <div className="flex items-center gap-3">
        <Gauge className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t("title")}</h1>
          <p className="text-sm text-muted-foreground">
            {t("subtitle")}
          </p>
        </div>
      </div>

      <Card>
        <CardContent className="space-y-3 p-4">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Network className="h-4 w-4 text-primary" /> {t("cluster_wide_fleet")}
          </div>
          <PendingBackend
            endpoint="GET /api/v1/admin/servers"
            note={t("ops_overview_pending_note")}
          />
          <Link
            to="/ops/fleet"
            className="inline-block text-sm font-medium text-primary hover:underline"
          >
            {t("open_fleet_table")}
          </Link>
        </CardContent>
      </Card>

      <Card className="border-dashed bg-transparent">
        <CardContent className="flex items-start gap-3 p-4 text-xs text-muted-foreground">
          <ServerOff className="mt-0.5 h-4 w-4 shrink-0" />
          <span>
            {t("footer_pre")}
            <span className="font-medium">{t("footer_kubectl")}</span>
            {t("footer_post")}
          </span>
        </CardContent>
      </Card>
    </div>
  );
}
