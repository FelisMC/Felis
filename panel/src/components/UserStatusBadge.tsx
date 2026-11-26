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
        "inline-flex items-center gap-1.5 text-xs",
        disabled ? "text-destructive" : "text-muted-foreground",
        className,
      )}
    >
      <span
        aria-hidden="true"
        className={cn("h-[5.5px] w-[5.5px] shrink-0 rounded-full", disabled ? "bg-destructive" : "bg-primary")}
      />
      {disabled ? t("users_status_disabled") : t("users_status_active")}
    </span>
  );
}
