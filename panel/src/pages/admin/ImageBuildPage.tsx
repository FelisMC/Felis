import { useEffect, useState, useMemo, Fragment } from "react";
import { Cpu, Play, Terminal, Loader2, XCircle, AlertCircle, Plus, Search, ChevronDown } from "lucide-react";
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
import { LogConsole } from "@/components/LogConsole";
import { cn } from "@/lib/utils";
import { Loading, EmptyState } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { api, buildLogsStreamURL, humanizeError } from "@/lib/api";
import { formatRelative, formatAbsolute } from "@/lib/format";
import { useConfig } from "@/lib/hooks";
import type { Build, BuildStatus } from "@/lib/types";

const LOCAL_STORAGE_KEY = "felis_triggered_builds";
const PAGE_SIZE = 10;

const STATUS_BADGE_STYLE: Record<BuildStatus, string> = {
  pending: "bg-amber-500/10 text-amber-500 border-amber-500/20 animate-pulse",
  building: "bg-blue-500/10 text-blue-500 border-blue-500/20 animate-pulse",
  succeeded: "bg-emerald-500/10 text-emerald-500 border-emerald-500/20",
  failed: "bg-rose-500/10 text-rose-500 border-rose-500/20",
  cancelled: "bg-zinc-500/10 text-zinc-400 border-zinc-500/20",
};

function formatDuration(createdAt: string, finishedAt?: string, isZh?: boolean): string {
  const start = new Date(createdAt).getTime();
  if (!Number.isFinite(start)) return "";
  const end = finishedAt ? new Date(finishedAt).getTime() : Date.now();
  if (!Number.isFinite(end) || end < start) return "";
  const diffSec = Math.round((end - start) / 1000);
  if (diffSec < 60) {
    return isZh ? `${diffSec}秒` : `${diffSec}s`;
  }
  const m = Math.floor(diffSec / 60);
  const s = diffSec % 60;
  return isZh ? `${m}分${s}秒` : `${m}m ${s}s`;
}

export function ImageBuildPage() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const now = Date.now();
  const isZh = locale.startsWith("zh");
  const config = useConfig();

  // Form & Dialog State
  const [dialogOpen, setDialogOpen] = useState(false);
  const [imageRef, setImageRef] = useState("");
  const [contextRef, setContextRef] = useState("");
  const [dockerfile, setDockerfile] = useState("");
  const [baseImage, setBaseImage] = useState("");
  const [triggering, setTriggering] = useState(false);
  const [triggerError, setTriggerError] = useState<string | null>(null);

  // Build List State
  const [buildIds, setBuildIds] = useState<string[]>([]);
  const [builds, setBuilds] = useState<Build[]>([]);
  const [loadingBuilds, setLoadingBuilds] = useState(true);
  const [activeLogBuildId, setActiveLogBuildId] = useState<string | null>(null);
  const [cancellingId, setCancellingId] = useState<string | null>(null);

  // Search & Pagination State
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);

  // Load build IDs from localStorage on mount
  useEffect(() => {
    try {
      const stored = localStorage.getItem(LOCAL_STORAGE_KEY);
      if (stored) {
        setBuildIds(JSON.parse(stored));
      } else {
        // Seed default IDs so mock builds are loaded on first visit.
        // In production, querying these will 404 and safely show the empty state.
        const defaultIds = ["bld-1", "bld-2"];
        localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify(defaultIds));
        setBuildIds(defaultIds);
      }
    } catch {
      setLoadingBuilds(false);
    }
  }, []);

  // Fetch full details of builds
  const fetchBuilds = async (ids: string[]) => {
    if (ids.length === 0) {
      setBuilds([]);
      setLoadingBuilds(false);
      return;
    }
    try {
      const list = await Promise.all(
        ids.map(async (id) => {
          try {
            return await api.getBuild(id);
          } catch {
            return null; // ignore individual failures
          }
        })
      );
      // Filter out nulls and sort by creation time (newest first)
      const validBuilds = list.filter((b): b is Build => b !== null);
      validBuilds.sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime());
      setBuilds(validBuilds);
    } catch {
      // ignore
    } finally {
      setLoadingBuilds(false);
    }
  };

  // Fetch builds when IDs change
  useEffect(() => {
    fetchBuilds(buildIds);
  }, [buildIds]);

  // Poll active builds
  useEffect(() => {
    const active = builds.some((b) => b.status === "pending" || b.status === "building");
    if (!active) return;

    const timer = setInterval(() => {
      fetchBuilds(buildIds);
    }, 4000);

    return () => clearInterval(timer);
  }, [builds, buildIds]);

  // Filtered & Paginated Builds
  const filteredBuilds = useMemo(() => {
    let list = [...builds];
    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter(
        (b) =>
          b.id.toLowerCase().includes(q) ||
          b.image_ref.toLowerCase().includes(q) ||
          b.status.toLowerCase().includes(q)
      );
    }
    return list;
  }, [builds, search]);

  // Reset page when search changes
  useMemo(() => {
    setPage(1);
  }, [search]);

  const paginatedBuilds = useMemo(() => {
    const start = (page - 1) * PAGE_SIZE;
    return filteredBuilds.slice(start, start + PAGE_SIZE);
  }, [filteredBuilds, page]);

  // Form submission handler
  const handleTrigger = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!imageRef.trim() || !contextRef.trim() || !dockerfile.trim()) return;

    setTriggering(true);
    setTriggerError(null);
    try {
      const newBuild = await api.buildImage({
        image_ref: imageRef.trim(),
        context_ref: contextRef.trim(),
        dockerfile: dockerfile,
        base_image: baseImage.trim() || undefined,
      });

      // Save build ID to localStorage and update state
      const nextIds = [newBuild.id, ...buildIds];
      localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify(nextIds));
      setBuildIds(nextIds);
      setActiveLogBuildId(newBuild.id);
      setDialogOpen(false);

      // Clear form
      setImageRef("");
      setContextRef("");
      setDockerfile("");
      setBaseImage("");
    } catch (err) {
      setTriggerError(humanizeError(err));
    } finally {
      setTriggering(false);
    }
  };

  // Cancel build handler
  const handleCancel = async (id: string) => {
    if (!confirm(t("cancel_confirm"))) return;
    setCancellingId(id);
    try {
      await api.cancelBuild(id);
      fetchBuilds(buildIds);
    } catch (err) {
      alert(humanizeError(err));
    } finally {
      setCancellingId(null);
    }
  };

  if (!config) {
    return <Loading label={t("common:loading_config")} />;
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <Cpu className="h-6 w-6 text-primary" />
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{t("builds_title")}</h1>
            <p className="text-sm text-muted-foreground">{t("builds_subtitle")}</p>
          </div>
        </div>

        {/* Dialog for Trigger Build */}
        <Dialog open={dialogOpen} onOpenChange={(o) => {
          setDialogOpen(o);
          if (!o) {
            setTriggerError(null);
          }
        }}>
          <DialogTrigger asChild>
            <Button className="gap-1.5 text-xs font-semibold shrink-0">
              <Plus className="h-4 w-4" /> {t("trigger_build_title")}
            </Button>
          </DialogTrigger>
          <DialogContent className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>{t("trigger_build_title")}</DialogTitle>
              <DialogDescription>
                输入镜像构建参数，在隔离命名空间中启动 Kaniko 流水线任务。
              </DialogDescription>
            </DialogHeader>
            <form onSubmit={handleTrigger} className="space-y-4">
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div className="space-y-1.5">
                  <Label htmlFor="imageRef" className="text-xs font-medium text-muted-foreground">
                    {t("image_ref_label")} *
                  </Label>
                  <Input
                    id="imageRef"
                    placeholder="registry.felis.svc:5000/my-modpack:1.0"
                    value={imageRef}
                    onChange={(e) => setImageRef(e.target.value)}
                    disabled={triggering}
                    required
                  />
                </div>

                <div className="space-y-1.5">
                  <Label htmlFor="contextRef" className="text-xs font-medium text-muted-foreground">
                    {t("context_ref_label")} *
                  </Label>
                  <Input
                    id="contextRef"
                    placeholder={t("context_ref_placeholder")}
                    value={contextRef}
                    onChange={(e) => setContextRef(e.target.value)}
                    disabled={triggering}
                    required
                  />
                </div>
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="baseImage" className="text-xs font-medium text-muted-foreground">
                  {t("base_image_label")}
                </Label>
                <Input
                  id="baseImage"
                  placeholder={t("base_image_placeholder")}
                  value={baseImage}
                  onChange={(e) => setBaseImage(e.target.value)}
                  disabled={triggering}
                />
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="dockerfile" className="text-xs font-medium text-muted-foreground">
                  {t("dockerfile_label")} *
                </Label>
                <textarea
                  id="dockerfile"
                  placeholder={t("dockerfile_placeholder")}
                  value={dockerfile}
                  onChange={(e) => setDockerfile(e.target.value)}
                  disabled={triggering}
                  required
                  rows={8}
                  className="w-full rounded-md border border-input bg-zinc-950 px-3 py-2 text-xs font-mono text-zinc-200 shadow-sm placeholder:text-muted-foreground/60 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring focus-visible:ring-offset-0 disabled:cursor-not-allowed disabled:opacity-50 resize-y"
                />
              </div>

              {triggerError && (
                <div className="flex items-center gap-2 text-xs text-destructive bg-destructive/10 p-2.5 rounded-md font-medium">
                  <AlertCircle className="h-4 w-4 shrink-0" />
                  <span>{triggerError}</span>
                </div>
              )}

              <DialogFooter className="gap-2 pt-2">
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setDialogOpen(false)}
                  disabled={triggering}
                  className="text-xs"
                >
                  {t("common:cancel")}
                </Button>
                <Button
                  type="submit"
                  variant={imageRef.trim() && contextRef.trim() && dockerfile.trim() ? "default" : "outline"}
                  disabled={triggering || !imageRef.trim() || !contextRef.trim() || !dockerfile.trim()}
                  className="text-xs gap-1.5"
                >
                  {triggering ? (
                    <Loader2 className="h-4 w-4 animate-spin" />
                  ) : (
                    <Play className="h-4 w-4" />
                  )}
                  {t("trigger_build_btn")}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </div>

      {/* Build History Table Card */}
      <Card className="w-full">
        <CardContent className="p-0">
          {/* Filters Bar */}
          <div className="p-4 border-b">
            <div className="relative w-full">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                placeholder="搜索构建 ID、镜像引用或状态..."
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="pl-9"
              />
            </div>
          </div>

          {/* List Content */}
          {loadingBuilds && builds.length === 0 ? (
            <div className="py-12"><Loading /></div>
          ) : filteredBuilds.length === 0 ? (
            <div className="p-4">
              <EmptyState
                title={search.trim() ? "无匹配构建任务" : t("no_builds_title")}
                hint={search.trim() ? "尝试更换搜索词" : t("no_builds_hint")}
              />
            </div>
          ) : (
            <div className="w-full">
              <div className="overflow-x-auto">
                <table className="w-full border-collapse text-xs">
                  <thead>
                    <tr className="border-b border-border bg-muted/40 text-left text-[11px] uppercase tracking-wider text-muted-foreground select-none">
                      <th className="px-4 py-2.5 font-medium text-left">{t("table_build_id")}</th>
                      <th className="px-4 py-2.5 font-medium text-left">{t("table_ref")}</th>
                      <th className="px-4 py-2.5 font-medium text-center">{t("table_status")}</th>
                      <th className="px-4 py-2.5 font-medium text-left">{t("table_requester")}</th>
                      <th className="px-4 py-2.5 font-medium text-center">{t("table_created_at")}</th>
                      <th className="px-4 py-2.5 font-medium text-center">{t("table_duration")}</th>
                      <th className="px-4 py-2.5 font-medium text-right">{t("table_action")}</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border">
                    {paginatedBuilds.map((b) => (
                      <Fragment key={b.id}>
                        <tr className={cn(
                          "hover:bg-accent/40 transition-colors",
                          activeLogBuildId === b.id && "bg-primary/5 hover:bg-primary/5"
                        )}>
                          {/* Column 1: Build ID */}
                          <td className="px-4 py-3 align-middle font-mono font-bold text-muted-foreground select-all text-left">
                            {b.id}
                          </td>

                          {/* Column 2: Image Reference & Error */}
                          <td className="px-4 py-3 align-middle text-left">
                            <div className="flex flex-col min-w-0">
                              <span 
                                className="font-mono font-medium text-foreground select-all block max-w-xl truncate" 
                                title={`镜像引用: ${b.image_ref}${b.base_image ? `\n基础镜像: ${b.base_image}` : ""}${b.context_ref ? `\n构建上下文: ${b.context_ref}` : ""}`}
                              >
                                {b.image_ref}
                              </span>
                              {b.error && (
                                <div className="flex items-center gap-1.5 text-[11px] text-destructive font-medium mt-1 select-text" title={b.error}>
                                  <AlertCircle className="h-3.5 w-3.5 shrink-0 text-destructive/80" />
                                  <span className="break-all whitespace-pre-wrap">{b.error}</span>
                                </div>
                              )}
                            </div>
                          </td>

                          {/* Column 3: Status */}
                          <td className="px-4 py-3 align-middle text-center">
                            <div className="flex justify-center">
                              <Badge 
                                variant="outline" 
                                className={cn(
                                  "text-[10px] py-0.5 font-semibold uppercase tracking-wider w-[80px] justify-center px-0 shrink-0",
                                  STATUS_BADGE_STYLE[b.status]
                                )}
                              >
                                {b.status}
                              </Badge>
                            </div>
                          </td>

                          {/* Column 4: Requester */}
                          <td className="px-4 py-3 align-middle text-left text-muted-foreground truncate max-w-[12rem]" title={b.requested_by}>
                            {b.requested_by}
                          </td>

                          {/* Column 5: Created At */}
                          <td className="px-4 py-3 align-middle text-center text-muted-foreground whitespace-nowrap" title={formatAbsolute(b.created_at, locale)}>
                            {formatRelative(b.created_at, now, locale)}
                          </td>

                          {/* Column 6: Duration */}
                          <td className="px-4 py-3 align-middle text-center font-mono text-muted-foreground whitespace-nowrap">
                            {formatDuration(b.created_at, b.finished_at, isZh) || "—"}
                          </td>

                          {/* Column 7: Action */}
                          <td className="px-4 py-3 align-middle text-right whitespace-nowrap">
                            <div className="flex items-center justify-end gap-1.5 py-0.5">
                              {/* Cancel Button */}
                              {(b.status === "pending" || b.status === "building") && (
                                <Button
                                  variant="ghost"
                                  size="sm"
                                  onClick={() => handleCancel(b.id)}
                                  disabled={cancellingId === b.id}
                                  className="h-7 px-2 text-[11px] gap-1 font-semibold text-rose-500 hover:text-rose-600 hover:bg-rose-500/10 transition-colors"
                                >
                                  {cancellingId === b.id ? (
                                    <Loader2 className="h-3 w-3 animate-spin" />
                                  ) : (
                                    <XCircle className="h-3 w-3" />
                                  )}
                                  {t("cancel_build_btn")}
                                </Button>
                              )}
                              {/* Log Button */}
                              <Button
                                variant="ghost"
                                size="sm"
                                onClick={() => setActiveLogBuildId(activeLogBuildId === b.id ? null : b.id)}
                                className={cn(
                                  "h-7 px-2 text-[11px] gap-1 font-semibold transition-colors",
                                  activeLogBuildId === b.id 
                                    ? "bg-primary/10 text-primary hover:bg-primary/15" 
                                    : "text-muted-foreground hover:text-foreground hover:bg-accent"
                                )}
                              >
                                <Terminal className="h-3 w-3" />
                                <span>{t("view_logs_btn")}</span>
                                <ChevronDown className={cn("h-3 w-3 transition-transform duration-200", activeLogBuildId === b.id && "rotate-180")} />
                              </Button>
                            </div>
                          </td>
                        </tr>

                        {/* Inline SSE Log Console */}
                        {activeLogBuildId === b.id && (
                          <tr className="bg-muted/5 hover:bg-muted/5">
                            <td colSpan={7} className="p-0 border-t border-border">
                              <div className="px-4 pb-4 pt-1 bg-muted/10">
                                <div className="h-[300px] flex flex-col mt-2">
                                  <LogConsole url={buildLogsStreamURL(config.apiBase, b.id)} />
                                </div>
                              </div>
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    ))}
                  </tbody>
                </table>
              </div>

              {/* Pagination */}
              {filteredBuilds.length > PAGE_SIZE && (
                <div className="p-4 border-t">
                  <Pagination
                    page={page}
                    pageSize={PAGE_SIZE}
                    total={filteredBuilds.length}
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