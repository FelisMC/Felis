import { Boxes, CheckCircle2, CircleSlash } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { api } from "@/lib/api";
import { useAsync } from "@/lib/hooks";

// ImageAdmin is the Admin-Side view of the platform image whitelist (spec §14 —
// "镜像白名单(Postgres, Web 动态改可审计)"). The READ is real (GET /images); the
// add / enable / disable WRITE routes are an admin-tier mutation slice still to be
// added, so this page surfaces the whitelist honestly and labels the missing
// mutation rather than shipping dead buttons.
export function ImageAdmin() {
  const { t } = useTranslation("admin");
  const { data, error, loading, reload } = useAsync(() => api.listImages(), []);
  const images = data ?? [];

  return (
    <>
      <div className="flex items-center gap-3">
        <Boxes className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t("images_title")}</h1>
          <p className="text-sm text-muted-foreground">
            {t("images_subtitle")}
          </p>
        </div>
      </div>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : images.length === 0 ? (
        <EmptyState
          title={t("no_images_title")}
          hint={t("no_images_hint")}
        />
      ) : (
        <div className="space-y-2">
          {images.map((img) => (
            <Card key={img.image_ref}>
              <CardContent className="flex items-center justify-between gap-3 p-4">
                <div className="flex min-w-0 items-center gap-3">
                  <Boxes className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <span className="truncate font-mono text-sm">{img.image_ref}</span>
                  {img.source && (
                    <Badge variant="outline" className="shrink-0">
                      {img.source}
                    </Badge>
                  )}
                </div>
                {img.enabled ? (
                  <span className="inline-flex shrink-0 items-center gap-1 text-xs text-primary">
                    <CheckCircle2 className="h-3.5 w-3.5" /> {t("enabled")}
                  </span>
                ) : (
                  <span className="inline-flex shrink-0 items-center gap-1 text-xs text-muted-foreground">
                    <CircleSlash className="h-3.5 w-3.5" /> {t("disabled")}
                  </span>
                )}
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </>
  );
}
