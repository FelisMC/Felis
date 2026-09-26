import { Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { DialogFooter } from "@/components/ui/dialog";

interface Props {
  onCancel: () => void;
  onConfirm: () => void;
  disabled?: boolean;
  loading?: boolean;
  cancelLabel: string;
  confirmLabel: string;
  confirmVariant?: "destructive" | "default" | "outline";
  /** Additional content rendered between the message and buttons */
  children?: React.ReactNode;
  className?: string;
}

export function ConfirmFooter({
  onCancel,
  onConfirm,
  disabled,
  loading,
  cancelLabel,
  confirmLabel,
  confirmVariant = "destructive",
  children,
  className,
}: Props) {
  return (
    <DialogFooter className={className}>
      {children}
      {/* Backing out stays open whenever nothing is in flight: a form with nothing to
          save yet, or a confirm that is not allowed, is still one to cancel. */}
      <Button variant="outline" size="sm" onClick={onCancel} disabled={loading}>
        {cancelLabel}
      </Button>
      <Button variant={confirmVariant} size="sm" onClick={onConfirm} disabled={disabled || loading}>
        {loading && <Loader2 className="h-4 w-4 animate-spin" />}
        {confirmLabel}
      </Button>
    </DialogFooter>
  );
}
