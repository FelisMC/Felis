import { useState, useMemo } from "react";
import { Link } from "react-router-dom";
import { Boxes, CheckCircle2, CircleSlash, Plus, Trash2, Wrench } from "lucide-react";
import { SearchInput } from "@/components/SearchInput";
import { StatCard } from "@/components/StatCard";
import { PageHeader } from "@/components/PageHeader";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { MessageLine } from "@/components/MessageLine";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";

const PAGE_SIZE = 10;

// image_whitelist.source is plain text (build.Source* in Go); the values Felis
// writes itself get a name in the UI language, anything else shows as stored.
const SOURCE_KEYS: Record<string, string> = {
  built: "image_source_built",
  external: "image_source_external",
  recommended: "image_source_recommended",
};

function sourceLabel(source: string, t: TFunction): string {
  const key = SOURCE_KEYS[source];
  return key ? t(key) : source;
}

export function ImageAdmin() {
  const { t } = useTranslation("admin");
  const { data, error, loading, reload } = useAsync(() => api.listImages(), []);
  const images = useMemo(() => data ?? [], [data]);

  // Form & Dialog State
  const [dialogOpen, setDialogOpen] = useState(false);
  const [newImageRef, setNewImageRef] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [removeRef, setRemoveRef] = useState<string | null>(null);

  // Search & Filtering State
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<"all" | "enabled" | "disabled">("all");
  const [page, setPage] = useState(1);

  // Stats
  const stats = useMemo(() => {
    const total = images.length;
    const enabled = images.filter((img) => img.enabled).length;
    const disabled = total - enabled;
    return { total, enabled, disabled };
  }, [images]);
  const { total, enabled, disabled } = stats;
  // The pipeline card counts builds, which live in their own history: one row is
  // enough, the answer carries the total. A read that failed shows a dash.
  const buildsQ = useAsync(() => api.listBuilds({ limit: 1 }), []);

  // Filtered & Paginated Images
  const filteredImages = useMemo(() => {
    let list = [...images];
    
    // 1. Search Filter
    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter(
        (img) =>
          img.image_ref.toLowerCase().includes(q) ||
          (img.source &&
            (img.source.toLowerCase().includes(q) ||
              sourceLabel(img.source, t).toLowerCase().includes(q)))
      );
    }

    // 2. Status Filter
    if (statusFilter === "enabled") {
      list = list.filter((img) => img.enabled);
    } else if (statusFilter === "disabled") {
      list = list.filter((img) => !img.enabled);
    }

    return list;
  }, [images, search, statusFilter, t]);

  // Reset page when filter changes
  const lastFilterKey = `${search}-${statusFilter}`;
  const [prevFilterKey, setPrevFilterKey] = useState(lastFilterKey);
  if (prevFilterKey !== lastFilterKey) {
    setPage(1);
    setPrevFilterKey(lastFilterKey);
  }

  const paginatedImages = useMemo(() => {
    const start = (page - 1) * PAGE_SIZE;
    return filteredImages.slice(start, start + PAGE_SIZE);
  }, [filteredImages, page]);

  async function handleAdd(e?: React.FormEvent) {
    e?.preventDefault();
    if (!newImageRef.trim()) return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      await api.addImage(newImageRef.trim());
      setNewImageRef("");
      setDialogOpen(false);
      reload();
    } catch (err) {
      setSubmitError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  async function handleRemove(imageRef: string) {
    await api.removeImage(imageRef);
    reload();
  }

  return (
    <div className="space-y-6">
      <PageHeader icon={Boxes} title={t("images_title")} subtitle={t("images_subtitle")} actions={
        <Dialog open={dialogOpen} onOpenChange={(o) => {
          setDialogOpen(o);
          if (!o) {
            setNewImageRef("");
            setSubmitError(null);
          }
        }}>
          <DialogTrigger asChild>
            <Button size="sm" className="gap-1.5 shrink-0">
              <Plus className="h-4 w-4" /> {t("add_image_btn")}
            </Button>
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t("add_image_title")}</DialogTitle>
              <DialogDescription>
                {t("add_image_desc")}
              </DialogDescription>
            </DialogHeader>
            <form onSubmit={handleAdd} className="space-y-4">
              <div className="space-y-1.5">
                <Label htmlFor="imageRefInput" className="text-xs font-medium text-muted-foreground">
                  {t("image_ref_label")} *
                </Label>
                <Input
                  id="imageRefInput"
                  placeholder={t("image_ref_placeholder")}
                  value={newImageRef}
                  onChange={(e) => setNewImageRef(e.target.value)}
                  disabled={submitting}
                  required
                />
              </div>
              {submitError && <MessageLine kind="error" message={submitError} compact />}
              <ConfirmFooter
                onCancel={() => setDialogOpen(false)}
                onConfirm={() => handleAdd()}
                disabled={submitting || !newImageRef.trim()}
                loading={submitting}
                cancelLabel={t("common:cancel")}
                confirmLabel={t("add_image_btn")}
              />
            </form>
          </DialogContent>
        </Dialog>
      } className="mb-6" />

      {/* Stats Cards Row */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-4">
        <StatCard icon={Boxes} label={t("filter_all")} value={total} accentClass="text-primary bg-primary/10" />
        <StatCard icon={CheckCircle2} label={t("enabled")} value={enabled} accentClass="text-emerald-500 bg-emerald-500/10" />
        <StatCard icon={CircleSlash} label={t("disabled")} value={disabled} accentClass="text-zinc-500 bg-zinc-500/10" />
        <Link
          to="/admin/builds"
          className="block rounded-lg transition-shadow hover:shadow-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <StatCard
            icon={Wrench}
            label={t("builds_title")}
            value={buildsQ.data ? buildsQ.data.total : "—"}
            accentClass="text-primary bg-primary/10"
          />
        </Link>
      </div>

      {/* Whitelist Table Card */}
      <Card className="w-full">
        <CardContent className="p-0">
          {/* Filters Bar */}
          <div className="flex flex-col sm:flex-row gap-3 p-4 border-b">
            <SearchInput value={search} onChange={setSearch} placeholder={t("images_search_placeholder")} />
            <div className="inline-flex h-10 items-center justify-center rounded-lg bg-muted p-1 text-muted-foreground shrink-0 select-none border border-border/40">
              <Button variant="ghost" size="sm"
                type="button"
                onClick={() => {
                  setStatusFilter("all");
                  setPage(1);
                }}
                className={cn(
                  "font-medium",
                  statusFilter === "all"
                    ? "bg-background text-foreground"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground"
                )}
              >
                {t("filter_all")} ({stats.total})
              </Button>
              <Button variant="ghost" size="sm"
                type="button"
                onClick={() => {
                  setStatusFilter("enabled");
                  setPage(1);
                }}
                className={cn(
                  "font-medium",
                  statusFilter === "enabled"
                    ? "bg-background text-primary"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground"
                )}
              >
                {t("filter_enabled")} ({stats.enabled})
              </Button>
              <Button variant="ghost" size="sm"
                type="button"
                onClick={() => {
                  setStatusFilter("disabled");
                  setPage(1);
                }}
                className={cn(
                  "font-medium",
                  statusFilter === "disabled"
                    ? "bg-background text-foreground/80"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground"
                )}
              >
                {t("filter_disabled")} ({stats.disabled})
              </Button>
            </div>
          </div>

          {/* List Content */}
          {loading && !data ? (
            <div className="py-12"><Loading /></div>
          ) : error ? (
            <div className="py-12"><ErrorState error={error} onRetry={reload} /></div>
          ) : filteredImages.length === 0 ? (
            <div className="p-4 border-b-0">
              <EmptyState
                title={search.trim() || statusFilter !== "all" ? t("search_no_results") : t("no_images_title")}
                hint={search.trim() || statusFilter !== "all" ? t("search_no_results_hint") : t("no_images_hint")}
              />
            </div>
          ) : (
            <div className="w-full">
              {/* Table Header */}
              <div className="hidden md:grid grid-cols-12 gap-3 px-4 py-2.5 text-[11px] font-semibold text-muted-foreground border-b select-none">
                <div className="col-span-7">{t("table_ref")}</div>
                <div className="col-span-2">{t("table_source")}</div>
                <div className="col-span-2 text-center">{t("table_status")}</div>
                <div className="col-span-1 text-right">{t("table_action")}</div>
              </div>

              {/* Table Body */}
              <div className="divide-y divide-border select-text">
                {paginatedImages.map((img) => (
                  <div
                    key={img.image_ref}
                    className={`grid grid-cols-1 md:grid-cols-12 gap-3 items-center px-4 py-2 text-xs transition-colors ${
                      img.enabled 
                        ? "hover:bg-accent/40 text-foreground" 
                        : "bg-muted/10 text-muted-foreground/80 opacity-60 hover:bg-muted/20"
                    }`}
                  >
                    {/* Reference */}
                    <div className="col-span-7 flex items-center gap-2.5 min-w-0 py-1">
                      <Boxes className="h-3.5 w-3.5 shrink-0 text-muted-foreground/60" />
                      <span className="font-mono font-medium truncate select-all" title={img.image_ref}>
                        {img.image_ref}
                      </span>
                    </div>

                    {/* Source */}
                    <div className="col-span-2 py-1">
                      {img.source ? (
                        <Badge variant="outline" className="text-[10px] px-1.5 py-0 font-medium capitalize">
                          {sourceLabel(img.source, t)}
                        </Badge>
                      ) : (
                        <span className="text-muted-foreground/60">—</span>
                      )}
                    </div>

                    {/* Status */}
                    <div className="col-span-2 md:text-center py-1">
                      {img.enabled ? (
                        <Badge variant="default" className="text-[10px] px-1.5 py-0 font-semibold">
                          <CheckCircle2 className="h-3 w-3 mr-1 shrink-0" /> {t("enabled")}
                        </Badge>
                      ) : (
                        <Badge variant="muted" className="text-[10px] px-1.5 py-0 font-semibold">
                          <CircleSlash className="h-3 w-3 mr-1 shrink-0" /> {t("disabled")}
                        </Badge>
                      )}
                    </div>

                    {/* Action */}
                    <div className="col-span-1 text-right py-1">
                      <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => setRemoveRef(img.image_ref)}
                        className="h-8 w-8 text-muted-foreground hover:text-destructive hover:bg-destructive/10"
                        aria-label={t("delete_image_tooltip")}
                        title={t("delete_image_tooltip")}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </div>
                  </div>
                ))}
              </div>

              {/* Pagination */}
              {filteredImages.length > PAGE_SIZE && (
                <div className="p-4 border-t">
                  <Pagination
                    page={page}
                    pageSize={PAGE_SIZE}
                    total={filteredImages.length}
                    onChange={setPage}
                  />
                </div>
              )}
            </div>
          )}
        </CardContent>
      </Card>

      <ConfirmDialog
        open={removeRef !== null}
        onOpenChange={(o) => !o && setRemoveRef(null)}
        title={t("delete_image_title")}
        description={
          <>
            {t("delete_image_desc")}{" "}
            <code className="break-all font-mono text-foreground">{removeRef}</code>
          </>
        }
        confirmLabel={t("delete_image_confirm")}
        onConfirm={() => handleRemove(removeRef!)}
      />
    </div>
  );
}