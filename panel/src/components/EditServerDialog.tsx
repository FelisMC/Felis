import { useState, type ReactNode } from "react";
import { ChevronRight, Settings } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { ServerConfigForm, type ServerConfigProps } from "@/components/ServerConfigForm";

/** EditServerDialog opens the server's parameters in a dialog, for pages without a settings tab of their own. */
export function EditServerDialog({ trigger, ...props }: ServerConfigProps & { trigger?: ReactNode }) {
  const { t } = useTranslation("servers");
  const { systemService = false, mode = "admin", serverName } = props;
  // Opened for one server, so moving to another closes it.
  const [openFor, setOpenFor] = useState<string | null>(null);
  const open = openFor === serverName;
  return (
    <Dialog open={open} onOpenChange={(next) => setOpenFor(next ? serverName : null)}>
      <DialogTrigger asChild>
        {trigger ?? <button
          type="button"
          className="group flex w-full text-left items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent focus:outline-none"
        >
          <Settings className="h-5 w-5 shrink-0 text-primary" />
          <div className="min-w-0 flex-1 text-left">
            <p className="text-sm font-medium">{t("edit_server_title")}</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              {t(systemService ? "edit_system_desc" : mode === "admin" ? "edit_server_desc" : "edit_owner_desc")}
            </p>
          </div>
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
        </button>}
      </DialogTrigger>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("edit_server_title")}</DialogTitle>
          <DialogDescription>
            {t(systemService ? "edit_system_desc" : "edit_server_desc_long")}
          </DialogDescription>
        </DialogHeader>
        {/* Keyed by server, so each opening starts from the server's current values. */}
        {open && <ServerConfigForm key={serverName} {...props} inDialog onDone={() => setOpenFor(null)} />}
      </DialogContent>
    </Dialog>
  );
}
