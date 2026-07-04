import { Crown, Shield, UserRound } from "lucide-react";
import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";

const ICON_MAP = { owner: Crown, admin: Shield, user: UserRound };

const STYLE_MAP = {
  owner: "bg-yellow-500/10 text-yellow-600",
  admin: "bg-primary/10 text-primary",
  user: "bg-muted text-muted-foreground",
};

const I18N_KEY = {
  owner: "users_role_owner",
  admin: "users_role_admin",
  user: "users_role_user",
};

export function RoleBadge({ role }: { role: "owner" | "admin" | "user" }) {
  const { t } = useTranslation("admin");
  const Icon = ICON_MAP[role];
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full px-3 py-1 text-xs font-medium",
        STYLE_MAP[role],
      )}
    >
      <Icon className="h-3 w-3" />
      {t(I18N_KEY[role])}
    </span>
  );
}
