import { useTranslation } from "react-i18next";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import type { FileWorldConfirmation } from "./useFileWorldConfirmation";

export function LiveWorldDialog({ world }: { world: Pick<FileWorldConfirmation, "asking" | "accept" | "dismiss"> }) {
  const { t } = useTranslation("files");
  return (
    <ConfirmDialog
      open={world.asking}
      onOpenChange={(open) => { if (!open) world.dismiss(); }}
      title={t("live_world_ask_title")}
      description={t("live_world_ask_body")}
      confirmLabel={t("live_world_ask_confirm")}
      confirmVariant="default"
      onConfirm={async () => world.accept()}
    />
  );
}
