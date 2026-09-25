import { useEffect, useState, useMemo, Fragment } from "react";
import { Cpu, Terminal, Loader2, XCircle, AlertCircle, Plus, ChevronDown } from "lucide-react";
import { useTranslation } from "react-i18next";
import { SearchInput } from "@/components/SearchInput";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { LogConsole } from "@/components/LogConsole";
import { cn } from "@/lib/utils";
import { PageHeader } from "@/components/PageHeader";
import { MessageLine } from "@/components/MessageLine";
import { Loading, EmptyState } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { api, buildLogsStreamURL, humanizeError } from "@/lib/api";
import { formatRelative, formatAbsolute } from "@/lib/format";
import { useConfig } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import type { Build, BuildStatus, Submission } from "@/lib/types";

const LOCAL_STORAGE_KEY = "felis_triggered_builds";
const PAGE_SIZE = 10;

const STATUS_BADGE_STYLE: Record<BuildStatus, string> = {
  pending: "bg-amber-500/10 text-amber-500 border-amber-500/20 animate-pulse",
  building: "bg-blue-500/10 text-blue-500 border-blue-500/20 animate-pulse",
  succeeded: "bg-emerald-500/10 text-emerald-500 border-emerald-500/20",
  failed: "bg-rose-500/10 text-rose-500 border-rose-500/20",
  cancelled: "bg-zinc-500/10 text-zinc-400 border-zinc-500/20",
};

function formatDuration(
  createdAt: string,
  finishedAt: string | undefined,
  t: (key: string, opts?: Record<string, unknown>) => string
): string {
  const start = new Date(createdAt).getTime();
  if (!Number.isFinite(start)) return "";
  const end = finishedAt ? new Date(finishedAt).getTime() : Date.now();
  if (!Number.isFinite(end) || end < start) return "";
  const diffSec = Math.round((end - start) / 1000);
  if (diffSec < 60) {
    return t("build_duration_seconds", { s: diffSec });
  }
  return t("build_duration_minutes", { m: Math.floor(diffSec / 60), s: diffSec % 60 });
}

export function ImageBuildPage() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const now = Date.now();
  const config = useConfig();
  // The owner tier comes from the server-side /me verdict (TierProvider), never
  // from guessing at the address: the old mock-era email heuristic misread the
  // real owner (felis-owner@example.com) as non-owner while treating any
  // "owner@…" address as one.
  const { isOwner } = useTier();

  // Form & Dialog State
  const [dialogOpen, setDialogOpen] = useState(false);
  const [imageRef, setImageRef] = useState("");
  const [contextRef, setContextRef] = useState("");
  const [dockerfile, setDockerfile] = useState("");
  const [baseImage, setBaseImage] = useState("");
  const [triggering, setTriggering] = useState(false);
  const [triggerError, setTriggerError] = useState<string | null>(null);

  const [submissions, setSubmissions] = useState<Submission[]>([]);
  const [loadingSubmissions, setLoadingSubmissions] = useState(false);
  const [selectedSub, setSelectedSub] = useState<Submission | null>(null);

  const visibleSubmissions = useMemo(() => {
    if (isOwner) {
      return submissions;
    }
    return submissions.filter((s) => s.status === "approved");
  }, [submissions, isOwner]);

  useEffect(() => {
    if (dialogOpen) {
      setLoadingSubmissions(true);
      api.listSubmissions()
        .then(setSubmissions)
        .catch(() => {})
        .finally(() => {
          setLoadingSubmissions(false);
        });
    }
  }, [dialogOpen]);

  const handleSelectSubmission = (subId: string) => {
    if (!subId || subId.startsWith("_")) return;
    const sub = submissions.find((s) => s.id === subId);
    if (!sub) return;

    setSelectedSub(sub);

    const derivedImageRef = sub.image_ref || `registry.felis.svc:5000/user-uploads/${sub.id}:latest`;
    setImageRef(derivedImageRef);
    setContextRef(sub.context_ref);
    setDockerfile(
      `# felis user-modpack submission ${sub.id}\n` +
      `# The executed Dockerfile is provided by the uploaded build context:\n` +
      `#   ${sub.context_ref}\n` +
      `# Built in the isolated felis-build sandbox and Trivy-gated (spec §16).\n`
    );
    setBaseImage("");
  };

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
  useEffect(() => {
    setPage(1);
  }, [search]);

  const paginatedBuilds = useMemo(() => {
    const start = (page - 1) * PAGE_SIZE;
    return filteredBuilds.slice(start, start + PAGE_SIZE);
  }, [filteredBuilds, page]);

  // Form submission handler
  const handleTrigger = async (e?: React.FormEvent) => {
    e?.preventDefault();
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
      <PageHeader icon={Cpu} title={t("builds_title")} subtitle={t("builds_subtitle")} actions={
        <Dialog open={dialogOpen} onOpenChange={(o) => {
          setDialogOpen(o);
          if (!o) {
            setTriggerError(null);
            setSelectedSub(null);
          }
        }}>
          <DialogTrigger asChild>
            <Button size="sm" className="gap-1.5 shrink-0">
              <Plus className="h-4 w-4" /> {t("trigger_build_title")}
            </Button>
          </DialogTrigger>
          <DialogContent className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>{t("trigger_build_title")}</DialogTitle>
              <DialogDescription>
                {t("trigger_build_desc")}
              </DialogDescription>
            </DialogHeader>
            <form onSubmit={handleTrigger} className="space-y-4">
              <div className="space-y-1.5 p-3 rounded-lg border border-border bg-muted/20">
                <Label className="text-xs font-semibold text-muted-foreground">
                  {t("build_import_submission_label")}
                </Label>
                <Select onValueChange={handleSelectSubmission} disabled={triggering}>
                  <SelectTrigger className="w-full text-xs h-9 bg-background [&>span]:flex [&>span]:w-full [&>span]:items-center [&>span]:justify-between [&>span]:gap-2 pr-2">
                    <SelectValue placeholder={t("build_import_submission_placeholder")} />
                  </SelectTrigger>
                  <SelectContent className="max-h-60 overflow-y-auto">
                    {loadingSubmissions ? (
                      <SelectItem value="_loading" disabled>
                        {t("common:loading_config")}...
                      </SelectItem>
                    ) : visibleSubmissions.length === 0 ? (
                      <SelectItem value="_none" disabled>
                        {t("build_import_submission_none")}
                      </SelectItem>
                    ) : (
                      visibleSubmissions.map((sub) => (
                        <SelectItem
                          key={sub.id}
                          value={sub.id}
                          className="w-full pr-4 [&>span:not(.absolute)]:flex-1 [&>span:not(.absolute)]:flex [&>span:not(.absolute)]:items-center [&>span:not(.absolute)]:justify-between"
                        >
                          <div className="flex items-center gap-2 min-w-0">
                            <span className="font-semibold truncate">{sub.display_name}</span>
                            <span className="text-[10px] text-muted-foreground font-mono bg-muted/40 px-1.5 py-0.2 rounded shrink-0">
                              {sub.id}
                            </span>
                          </div>
                          <span className={cn(
                            "text-[10px] font-semibold px-1.5 py-0.5 rounded border shrink-0",
                            sub.status === "pending_review" && "bg-amber-500/10 text-amber-500 border-amber-500/20",
                            sub.status === "approved" && "bg-emerald-500/10 text-emerald-500 border-emerald-500/20",
                            sub.status === "rejected" && "bg-rose-500/10 text-rose-500 border-rose-500/20"
                          )}>
                            {sub.status === "pending_review" ? t("status_pending_review") : sub.status === "approved" ? t("status_approved") : t("status_rejected")}
                          </span>
                        </SelectItem>
                      ))
                    )}
                  </SelectContent>
                </Select>
                <p className="text-[10px] text-muted-foreground/80 leading-normal">
                  {t("build_import_submission_hint")}
                </p>
              </div>

              {selectedSub && selectedSub.status !== "approved" && (
                <div className="flex items-start gap-2.5 text-xs text-amber-500 bg-amber-500/10 border border-amber-500/30 p-3 rounded-md font-medium">
                  <AlertCircle className="h-4 w-4 shrink-0 mt-0.5 animate-bounce" />
                  <div className="space-y-1">
                    <p className="font-bold text-amber-400">
                      {t("build_import_submission_warning_title", { status: selectedSub.status === "pending_review" ? t("status_pending_review") : t("status_rejected") })}
                    </p>
                    <p className="text-[10px] text-muted-foreground leading-normal">
                      {t("build_import_submission_warning_desc")}
                    </p>
                  </div>
                </div>
              )}

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
                <p className="text-[10px] text-muted-foreground/80 leading-relaxed">{t("dockerfile_audit_hint")}</p>
              </div>

              {triggerError && <MessageLine kind="error" message={triggerError} compact />}

              <ConfirmFooter
                onCancel={() => setDialogOpen(false)}
                onConfirm={() => handleTrigger()}
                disabled={triggering || !imageRef.trim() || !contextRef.trim() || !dockerfile.trim()}
                loading={triggering}
                cancelLabel={t("common:cancel")}
                confirmLabel={t("trigger_build_btn")}
              />
            </form>
          </DialogContent>
        </Dialog>
      } className="mb-6" />

      {/* Build History Table Card */}
      <Card className="w-full">
        <CardContent className="p-0">
          {/* Filters Bar */}
          <div className="p-4 border-b">
            <SearchInput value={search} onChange={setSearch} placeholder={t("builds_search_placeholder")} />
          </div>

          {/* List Content */}
          {loadingBuilds && builds.length === 0 ? (
            <div className="py-12"><Loading /></div>
          ) : filteredBuilds.length === 0 ? (
            <div className="p-4">
              <EmptyState
                title={search.trim() ? t("search_no_results") : t("no_builds_title")}
                hint={search.trim() ? t("search_no_results_hint") : t("no_builds_hint")}
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
                                title={`${t("image_ref_label")}: ${b.image_ref}${b.base_image ? `\n${t("base_image_label")}: ${b.base_image}` : ""}${b.context_ref ? `\n${t("context_ref_label")}: ${b.context_ref}` : ""}`}
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
                                title={b.status === "pending" ? t("build_queued_hint") : undefined}
                                className={cn(
                                  "text-[10px] py-0.5 font-semibold uppercase tracking-wider w-[80px] justify-center px-0 shrink-0",
                                  STATUS_BADGE_STYLE[b.status]
                                )}
                              >
                                {t(`build_status_${b.status}`)}
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
                            {formatDuration(b.created_at, b.finished_at, t) || "—"}
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
