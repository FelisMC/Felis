import { useId } from "react";
import { Network } from "lucide-react";
import { useTranslation } from "react-i18next";
import { CopyAddress } from "@/components/CopyAddress";
import { Card, CardContent } from "@/components/ui/card";
import type { ServerExtraPort } from "@/lib/types";

export function ExtraPortsCard({ ports, pending = false }: { ports: ServerExtraPort[]; pending?: boolean }) {
  const { t } = useTranslation("servers");
  const titleId = useId();
  return <Card role="region" aria-labelledby={titleId} className="min-w-0 shrink-0">
    <CardContent className="space-y-3 p-4">
      <h2 id={titleId} className="flex items-center gap-2 text-sm font-medium"><Network className="h-4 w-4 text-primary" />{t("extra_ports_title")}</h2>
      <p className="text-xs leading-relaxed text-muted-foreground">{t("extra_ports_direct_hint")}</p>
      {pending && <p role="status" className="text-xs text-amber-700 dark:text-amber-400">{t("extra_ports_pending")}</p>}
      {ports.length === 0 ? <p className="text-xs text-muted-foreground">{t("extra_ports_empty")}</p> : <ul className="space-y-3">
        {ports.map((entry) => <li key={`${entry.protocol}:${entry.port}`} className="min-w-0 space-y-1 border-t border-border pt-3">
          <p className="text-xs font-medium">{t("extra_ports_mapping", { protocol: entry.protocol, port: entry.port })}</p>
          {entry.nodePort !== undefined && <p className="text-xs text-muted-foreground">{t("extra_ports_external", { port: entry.nodePort })}</p>}
          {entry.address ? <CopyAddress address={entry.address} /> : <p className="text-xs text-muted-foreground">{t("extra_ports_address_unavailable")}</p>}
        </li>)}
      </ul>}
    </CardContent>
  </Card>;
}
