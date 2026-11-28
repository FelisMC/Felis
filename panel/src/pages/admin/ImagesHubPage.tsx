import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { Boxes, ClipboardCheck, Cpu } from "lucide-react";
import { useTranslation } from "react-i18next";
import { PageHeader } from "@/components/PageHeader";
import { PageTabs } from "@/components/PageTabs";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { api, humanizeError } from "@/lib/api";
import { useFeatures } from "@/lib/features";
import { useTier } from "@/lib/tier";
import { ImageAdmin } from "./ImageAdmin";
import { ImageBuildPage } from "./ImageBuildPage";
import { SubmissionsPage } from "./SubmissionsPage";

const HUB_TABS = [
  { key: "images", icon: Boxes },
  { key: "builds", icon: Cpu },
  { key: "submissions", icon: ClipboardCheck },
] as const;

type HubTab = (typeof HUB_TABS)[number]["key"];

export function ImagesHubPage() {
  const { t } = useTranslation("admin");
  const { features, loaded, apply } = useFeatures();
  const { isOwner } = useTier();
  const reauth = useReauth();
  const [params, setParams] = useSearchParams();
  const requested = params.get("tab");
  const tab: HubTab = HUB_TABS.some((x) => x.key === requested) ? (requested as HubTab) : "images";
  const [savingToggle, setSavingToggle] = useState(false);
  const [toggleError, setToggleError] = useState<string | null>(null);

  async function toggleSubmissions(checked: boolean) {
    if (!isOwner || savingToggle) return;
    setSavingToggle(true);
    setToggleError(null);
    try {
      const latest = await api.getFeatures();
      const next = await reauth.guard(() =>
        api.setFeatures({
          mode: latest.mode,
          useLobby: latest.lobbyAvailable ? latest.useLobby : false,
          customImages: latest.customImages,
          allowSubmissions: checked,
          revision: latest.revision,
        }),
      );
      apply(next);
    } catch (err) {
      if (!isReauthCancelled(err)) setToggleError(humanizeError(err));
    } finally {
      setSavingToggle(false);
    }
  }

  if (loaded && !features.customImages) {
    return (
      <div className="space-y-4">
        <PageHeader icon={Boxes} title={t("images_hub_title")} subtitle={t("images_hub_subtitle")} />
        <Card>
          <CardContent className="flex flex-col items-start gap-3 p-6 text-sm">
            <p className="font-medium text-foreground">{t("images_hub_disabled_title")}</p>
            <p className="text-muted-foreground">{t("images_hub_disabled_desc")}</p>
            {isOwner && (
              <Button asChild size="sm" variant="outline">
                <Link to="/admin/platform">{t("images_hub_open_settings")}</Link>
              </Button>
            )}
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <PageHeader
        icon={Boxes}
        title={t("images_hub_title")}
        subtitle={t("images_hub_subtitle")}
        tabs={
          <PageTabs
            label={t("images_hub_tabs")}
            idPrefix="images-hub"
            value={tab}
            tabs={HUB_TABS.map(({ key, icon }) => ({ key, icon, label: t(`images_hub_tab_${key}`) }))}
            onChange={(key) =>
              setParams(
                (p) => {
                  p.set("tab", key);
                  return p;
                },
                { replace: true },
              )
            }
          />
        }
      />

      {tab === "images" && <ImageAdmin embedded />}
      {tab === "builds" && <ImageBuildPage embedded />}
      {tab === "submissions" && (
        <div className="space-y-4">
          {isOwner && (
            <Card>
              <CardContent className="space-y-2 p-4">
                <div className="flex items-start justify-between gap-4">
                  <div className="space-y-0.5">
                    <Label htmlFor="hub-allow-submissions">{t("features_allow_submissions_label")}</Label>
                    <p className="text-xs leading-relaxed text-muted-foreground">
                      {t("features_allow_submissions_hint")}
                    </p>
                  </div>
                  <Switch
                    id="hub-allow-submissions"
                    checked={features.allowSubmissions}
                    disabled={savingToggle}
                    onCheckedChange={(checked) => void toggleSubmissions(checked)}
                  />
                </div>
                {toggleError && <MessageLine kind="error" message={toggleError} compact />}
              </CardContent>
            </Card>
          )}
          <SubmissionsPage embedded />
        </div>
      )}
      {reauth.dialog}
    </div>
  );
}
