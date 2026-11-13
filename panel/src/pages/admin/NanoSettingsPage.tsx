import { RefreshCw, Settings } from "lucide-react";
import { useTranslation } from "react-i18next";
import { APITokenSettingsCard } from "@/components/APITokenSettingsCard";
import { BrandingCard } from "@/components/BrandingCard";
import { DefaultLanguageCard } from "@/components/DefaultLanguageCard";
import { MailSettingsCard } from "@/components/MailSettingsCard";
import { PageHeader } from "@/components/PageHeader";
import { Button } from "@/components/ui/button";
import { RefreshContext, usePageRefresh } from "@/lib/hooks";

/** NanoSettingsPage is the Felis-nano Owner's settings: what sign-in needs, with no server or host cards. */
export function NanoSettingsPage() {
  const { t } = useTranslation("admin");
  const cards = usePageRefresh();
  return <RefreshContext.Provider value={cards.scope}><div className="space-y-6">
    <PageHeader icon={Settings} title={t("platform_title")} subtitle={t("nano_settings_subtitle")}
      actions={<Button variant="outline" size="sm" disabled={cards.dirty} onClick={cards.refresh}><RefreshCw />{t("platform_reload")}</Button>} />
    <MailSettingsCard />
    <DefaultLanguageCard />
    <APITokenSettingsCard />
    <BrandingCard />
  </div></RefreshContext.Provider>;
}
