import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";

export const ROLE_LABEL_KEY = {
  owner: "users_role_owner",
  admin: "users_role_admin",
  user: "users_role_user",
};

export function RoleBadge({ role }: { role: "owner" | "admin" | "user" }) {
  const { t } = useTranslation("admin");
  return (
    <span
      className={cn(
        "inline-flex items-center text-xs",
        role === "owner" ? "font-medium text-foreground" : "text-muted-foreground",
      )}
    >
      {t(ROLE_LABEL_KEY[role])}
    </span>
  );
}
