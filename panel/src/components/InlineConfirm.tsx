import { Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";

interface Props {
  open: boolean;
  confirming: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  confirmLabel: string;
  cancelLabel: string;
  confirmVariant?: "destructive" | "default";
  size?: "default" | "sm";
  className?: string;
}

export function InlineConfirm({
  open,
  confirming,
  onConfirm,
  onCancel,
  confirmLabel,
  cancelLabel,
  confirmVariant = "destructive",
  size = "sm",
  className,
}: Props) {
  if (!open) return null;
  return (
    <div className={className}>
      <Button
        variant="ghost"
        size={size}
        onClick={onCancel}
        disabled={confirming}
      >
        {cancelLabel}
      </Button>
      <Button
        variant={confirmVariant}
        size={size}
        onClick={onConfirm}
        disabled={confirming}
      >
        {confirming ? (
          <Loader2 className="h-4 w-4 animate-spin" />
        ) : (
          confirmLabel
        )}
      </Button>
    </div>
  );
}
