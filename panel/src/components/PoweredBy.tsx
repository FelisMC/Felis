import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";
import { useBranding } from "@/lib/branding";

/** PoweredBy is the Felis credit; the Owner can switch it off in the platform settings. */
export function PoweredBy({ className }: { className?: string }) {
  const { t } = useTranslation("common");
  const { show_powered_by } = useBranding();
  if (!show_powered_by) return null;
  return (
    <a
      href="https://felismc.com"
      target="_blank"
      rel="noreferrer"
      className={cn("text-center text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring", className)}
    >
      {t("powered_by")}
    </a>
  );
}
