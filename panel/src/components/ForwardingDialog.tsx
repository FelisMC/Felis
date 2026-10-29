import { useEffect, useState, type ReactNode } from "react";
import { ChevronRight, Loader2, Save, Shuffle } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { HostTaskStatus, useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";

// Legacy forwarding cannot authenticate player identities reaching the backend port.
export function ForwardingDialog({ serverName, legacy, onUpdated, trigger }: { serverName: string; legacy: boolean; onUpdated: () => void; trigger?: ReactNode }) {
  const { t } = useTranslation("servers");
  const host = useHostTask(["server-forwarding"]);
  const [open, setOpen] = useState(false);
  const [wanted, setWanted] = useState(legacy);
  useEffect(() => { if (!open) setWanted(legacy); }, [open, legacy]);
  const task = host.task?.request.params?.server === serverName ? host.task : undefined;
  useEffect(() => { if (task?.state === "succeeded") onUpdated(); }, [task?.state, onUpdated]);
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {trigger ?? <button type="button" className="group flex w-full items-center gap-3 rounded-xl border border-border bg-card p-4 text-left transition-colors hover:border-primary/40 hover:bg-accent focus:outline-none">
          <Shuffle className="h-5 w-5 shrink-0 text-primary" />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium">{t("forwarding_title")}</p>
            <p className="mt-0.5 text-xs text-muted-foreground">{t(legacy ? "forwarding_legacy" : "forwarding_modern")}</p>
          </div>
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
        </button>}
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("forwarding_title")}</DialogTitle>
          <DialogDescription>{t("forwarding_description")}</DialogDescription>
        </DialogHeader>
        <div className="flex items-start justify-between gap-4 rounded-lg border border-border p-4">
          <div className="space-y-1"><Label htmlFor="forwarding-legacy">{t("forwarding_use_legacy")}</Label><p className="text-xs leading-relaxed text-muted-foreground">{t("forwarding_use_legacy_hint")}</p></div>
          <Switch id="forwarding-legacy" checked={wanted} disabled={host.busy} onCheckedChange={setWanted} />
        </div>
        {wanted && !legacy && <MessageLine kind="error" message={t("forwarding_risk")} />}
        {host.error && <MessageLine kind="error" message={host.error} />}
        {task && <HostTaskStatus task={task} />}
        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={() => setOpen(false)}>{t("common:close_sr")}</Button>
          <Button disabled={!host.available || host.busy || wanted === legacy}
            onClick={() => void host.start({ action: "server-forwarding", params: { server: serverName, legacy: String(wanted) }, confirmMaintenance: wanted })}>
            {host.starting ? <Loader2 className="animate-spin" /> : <Save />}{t("forwarding_save")}
          </Button>
        </div>
        {host.reauth.dialog}
      </DialogContent>
    </Dialog>
  );
}
