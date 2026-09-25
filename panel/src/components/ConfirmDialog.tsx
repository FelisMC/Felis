import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { MessageLine } from "@/components/MessageLine";
import { humanizeError } from "@/lib/api";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: ReactNode;
  confirmLabel: string;
  /** Defaults to "Cancel"; name what dismissing keeps when the action is itself a cancel. */
  cancelLabel?: string;
  confirmVariant?: "destructive" | "default";
  /** Runs the action. The dialog closes when it resolves; a rejection is shown in
   *  the dialog, which stays open so the action can be retried or dismissed. */
  onConfirm: () => Promise<void>;
}

// ConfirmDialog asks before an action that cannot be taken back, in the panel's
// own dialog rather than window.confirm (which some embedded browsers block, so
// the action could never be confirmed there). The failure stays next to the
// buttons that caused it.
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  cancelLabel,
  confirmVariant = "destructive",
  onConfirm,
}: Props) {
  const { t } = useTranslation("common");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function setOpen(next: boolean) {
    if (busy) return;
    if (!next) setError(null);
    onOpenChange(next);
  }

  async function confirm() {
    setBusy(true);
    setError(null);
    try {
      await onConfirm();
      setBusy(false);
      onOpenChange(false);
    } catch (err) {
      setError(humanizeError(err));
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description && <DialogDescription>{description}</DialogDescription>}
        </DialogHeader>
        {error && <MessageLine kind="error" message={error} compact />}
        <ConfirmFooter
          onCancel={() => setOpen(false)}
          onConfirm={() => void confirm()}
          loading={busy}
          cancelLabel={cancelLabel ?? t("cancel")}
          confirmLabel={confirmLabel}
          confirmVariant={confirmVariant}
        />
      </DialogContent>
    </Dialog>
  );
}
