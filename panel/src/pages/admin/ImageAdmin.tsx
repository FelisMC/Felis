import { useState, useMemo } from "react";
import { Boxes, CheckCircle2, CircleSlash, Plus, Trash2, Loader2, Search, CheckCircle, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";

const PAGE_SIZE = 10;

function StatCard({
  icon: Icon,
  label,
  value,
  accentClass,
}: {
  icon: LucideIcon;
  label: string;
  value: number;
  accentClass?: string;
}) {
  return (
    <Card>
      <CardContent className="flex items-center gap-3 p-4">
        <div className={`rounded-md p-2 bg-muted/30 ${accentClass || "text-muted-foreground"}`}>
          <Icon className="h-5 w-5" />
        </div>
        <div>
          <div className="text-2xl font-bold font-mono leading-none">{value}</div>
          <div className="mt-1 text-xs text-muted-foreground font-medium">{label}</div>
        </div>
      </CardContent>
    </Card>
  );
}

export function ImageAdmin() {
  const { t } = useTranslation("admin");
  const { data, error, loading, reload } = useAsync(() => api.listImages(), []);
  const images = data ?? [];

  // Form & Dialog State
  const [dialogOpen, setDialogOpen] = useState(false);
  const [newImageRef, setNewImageRef] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [deletingRef, setDeletingRef] = useState<string | null>(null);

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

  // Filtered & Paginated Images
  const filteredImages = useMemo(() => {
    let list = [...images];
    
    // 1. Search Filter
    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter(
        (img) =>
          img.image_ref.toLowerCase().includes(q) ||
          (img.source && img.source.toLowerCase().includes(q))
      );
    }

    // 2. Status Filter
    if (statusFilter === "enabled") {
      list = list.filter((img) => img.enabled);
    } else if (statusFilter === "disabled") {
      list = list.filter((img) => !img.enabled);
    }

    return list;
  }, [images, search, statusFilter]);

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

  async function handleAdd(e: React.FormEvent) {
    e.preventDefault();
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
    if (!confirm(t("delete_confirm"))) return;
    setDeletingRef(imageRef);
    try {
      await api.removeImage(imageRef);
      reload();
    } catch (err) {
      alert(humanizeError(err));
    } finally {
      setDeletingRef(null);
    }
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <Boxes className="h-6 w-6 text-primary" />
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{t("images_title")}</h1>
            <p className="text-sm text-muted-foreground">
              {t("images_subtitle")}
            </p>
          </div>
        </div>

        {/* Dialog for Add External Image */}
        <Dialog open={dialogOpen} onOpenChange={(o) => {
          setDialogOpen(o);
          if (!o) {
            setNewImageRef("");
            setSubmitError(null);
          }
        }}>
          <DialogTrigger asChild>
            <Button className="gap-1.5 text-xs font-semibold shrink-0">
              <Plus className="h-4 w-4" /> {t("add_image_btn")}
            </Button>
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t("add_image_title")}</DialogTitle>
              <DialogDescription>
                将外部 Docker 镜像引用录入白名单，供后续创建服务器使用。
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
              {submitError && (
                <p className="text-xs text-destructive mt-1 font-medium bg-destructive/10 p-2.5 rounded-md">
                  {submitError}
                </p>
              )}
              <DialogFooter className="gap-2 pt-2">
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setDialogOpen(false)}
                  disabled={submitting}
                  className="text-xs"
                >
                  {t("common:cancel")}
                </Button>
                <Button
                  type="submit"
                  variant={newImageRef.trim() ? "default" : "outline"}
                  disabled={submitting || !newImageRef.trim()}
                  className="text-xs gap-1.5"
                >
                  {submitting ? (
                    <Loader2 className="h-4 w-4 animate-spin" />
                  ) : (
                    <Plus className="h-4 w-4" />
                  )}
                  {t("add_image_btn")}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </div>

      {/* Stats Cards Row */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <StatCard icon={Boxes} label="总镜像数" value={stats.total} />
        <StatCard icon={CheckCircle} label="已启用" value={stats.enabled} accentClass="text-primary bg-primary/10" />
        <StatCard icon={CircleSlash} label="已禁用" value={stats.disabled} accentClass="text-muted-foreground bg-muted/20" />
      </div>

      {/* Whitelist Table Card */}
      <Card className="w-full">
        <CardContent className="p-0">
          {/* Filters Bar */}
          <div className="flex flex-col sm:flex-row gap-3 p-4 border-b">
            <div className="relative flex-1 w-full">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                placeholder="搜索镜像名称或来源..."
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="pl-9"
              />
            </div>
            <div className="inline-flex h-9 items-center justify-center rounded-lg bg-muted p-1 text-muted-foreground shrink-0 select-none border border-border/40">
              <button
                type="button"
                onClick={() => {
                  setStatusFilter("all");
                  setPage(1);
                }}
                className={cn(
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                  statusFilter === "all"
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground"
                )}
              >
                {t("filter_all")} ({stats.total})
              </button>
              <button
                type="button"
                onClick={() => {
                  setStatusFilter("enabled");
                  setPage(1);
                }}
                className={cn(
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                  statusFilter === "enabled"
                    ? "bg-background text-primary shadow-sm"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground"
                )}
              >
                {t("filter_enabled")} ({stats.enabled})
              </button>
              <button
                type="button"
                onClick={() => {
                  setStatusFilter("disabled");
                  setPage(1);
                }}
                className={cn(
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                  statusFilter === "disabled"
                    ? "bg-background text-foreground/80 shadow-sm"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground"
                )}
              >
                {t("filter_disabled")} ({stats.disabled})
              </button>
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
                title={search.trim() || statusFilter !== "all" ? "无匹配结果" : t("no_images_title")}
                hint={search.trim() || statusFilter !== "all" ? "尝试更换搜索词或筛选条件" : t("no_images_hint")}
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
                          {img.source}
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
                        onClick={() => handleRemove(img.image_ref)}
                        disabled={deletingRef === img.image_ref}
                        className="h-8 w-8 text-muted-foreground hover:text-destructive hover:bg-destructive/10"
                        title={t("delete_image_tooltip")}
                      >
                        {deletingRef === img.image_ref ? (
                          <Loader2 className="h-3.5 w-3.5 animate-spin" />
                        ) : (
                          <Trash2 className="h-3.5 w-3.5" />
                        )}
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
    </div>
  );
}