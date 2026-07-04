import { Circle } from "lucide-react";
import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";

interface Props {
  disabled: boolean;
  className?: string;
}

export function UserStatusBadge({ disabled, className }: Props) {
  const { t } = useTranslation("admin");
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full px-3 py-1 text-xs font-medium",
        disabled
          ? "bg-destructive/10 text-destructive"
          : "bg-emerald-500/10 text-emerald-500",
        className,
      )}
    >
      <Circle className={cn("h-2 w-2", disabled ? "fill-destructive" : "fill-emerald-500")} />
      {disabled ? t("users_status_disabled") : t("users_status_active")}
    </span>
  );
}
