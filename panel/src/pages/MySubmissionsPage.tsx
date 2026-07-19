import { useState, useMemo, useRef } from "react";
import { useTranslation } from "react-i18next";
import {
  Upload,
  Loader2,
  FileText,
  X,
  ChevronDown,
  ChevronUp,
  Plus,
  CheckCircle,
  CircleSlash,
  ClipboardCheck,
} from "lucide-react";
import { SearchInput } from "@/components/SearchInput";
import {
  Card,
  CardContent,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { MessageLine } from "@/components/MessageLine";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { StatCard } from "@/components/StatCard";
import { PageHeader } from "@/components/PageHeader";
import { SubmissionStatusBadge } from "@/components/SubmissionStatusBadge";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { formatRelative, formatAbsolute } from "@/lib/format";
import type { Submission, SubmissionStatus } from "@/lib/types";

const PAGE_SIZE = 10;

function formatBytes(bytes: number, decimals = 2) {
  if (bytes === 0) return "0 Bytes";
  const k = 1024;
  const dm = decimals < 0 ? 0 : decimals;
  const sizes = ["Bytes", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + " " + sizes[i];
}

export function MySubmissionsPage() {
  const { t, i18n } = useTranslation("submissions");
  const locale = i18n.language;
  const now = Date.now();

  // Async API hook
  const { data, error: fetchError, loading, reload } = useAsync(() => api.listMySubmissions(), []);
  const submissions: Submission[] = data ?? [];

  // Dialog State
  const [dialogOpen, setDialogOpen] = useState(false);

  // Form State
  const [displayName, setDisplayName] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [dragActive, setDragActive] = useState(false);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [submitStep, setSubmitStep] = useState<"create" | "upload" | null>(null);
  const [error, setError] = useState<string | null>(null);

  const fileInputRef = useRef<HTMLInputElement>(null);

  // Search & Filtering State
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<"all" | SubmissionStatus>("all");
  const [page, setPage] = useState(1);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  // Filtered & Paginated Submissions
  const filteredSubmissions = useMemo(() => {
    let list = [...submissions];

    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter(
        (s) =>
          s.display_name.toLowerCase().includes(q) ||
          s.id.toLowerCase().includes(q),
      );
    }

    if (statusFilter !== "all") {
      list = list.filter((s) => s.status === statusFilter);
    }

    return list;
  }, [submissions, search, statusFilter]);

  // Reset page when filter changes
  const lastFilterKey = `${search}-${statusFilter}`;
  const [prevFilterKey, setPrevFilterKey] = useState(lastFilterKey);
  if (prevFilterKey !== lastFilterKey) {
    setPage(1);
    setPrevFilterKey(lastFilterKey);
  }

  const paginatedSubmissions = useMemo(() => {
    const start = (page - 1) * PAGE_SIZE;
    return filteredSubmissions.slice(start, start + PAGE_SIZE);
  }, [filteredSubmissions, page]);

  // Stats
  const stats = useMemo(() => {
    const total = submissions.length;
    const pending = submissions.filter((s) => s.status === "pending_review").length;
    const approved = submissions.filter((s) => s.status === "approved").length;
    const rejected = submissions.filter((s) => s.status === "rejected").length;
    return { total, pending, approved, rejected };
  }, [submissions]);

  // Drag and drop event handlers
  const handleDrag = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    if (e.type === "dragenter" || e.type === "dragover") {
      setDragActive(true);
    } else if (e.type === "dragleave") {
      setDragActive(false);
    }
  };

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDragActive(false);
    if (e.dataTransfer.files && e.dataTransfer.files[0]) {
      validateAndSetFile(e.dataTransfer.files[0]);
    }
  };

  const handleFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const selectedFile = e.target.files?.[0] || null;
    validateAndSetFile(selectedFile);
  };

  const validateAndSetFile = (selectedFile: File | null) => {
    setError(null);
    if (!selectedFile) {
      setFile(null);
      return;
    }
    if (!selectedFile.name.endsWith(".tar.gz")) {
      setError(t("error_file_type"));
      setFile(null);
      return;
    }
    const maxBytes = 1024 * 1024 * 1024; // 1 GiB limit
    if (selectedFile.size > maxBytes) {
      setError(t("error_file_size"));
      setFile(null);
      return;
    }
    setFile(selectedFile);
  };

  const handleFormSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);

    const trimmedName = displayName.trim();
    if (!trimmedName) {
      setError(t("error_name_required"));
      return;
    }
    if (!file) {
      setError(t("error_file_required"));
      return;
    }

    setIsSubmitting(true);
    setSubmitStep("create");

    try {
      // 1. Create the submission metadata
      const sub = await api.createSubmission(trimmedName);

      // 2. Upload context file
      setSubmitStep("upload");
      await api.uploadSubmissionContext(sub.id, file);

      setDisplayName("");
      setFile(null);
      if (fileInputRef.current) {
        fileInputRef.current.value = "";
      }
      setDialogOpen(false);
      reload();
    } catch (err) {
      setError(humanizeError(err));
    } finally {
      setIsSubmitting(false);
      setSubmitStep(null);
    }
  };

  return (
    <div className="space-y-6">
      {/* Header */}
      <PageHeader
        icon={Upload}
        title={t("title")}
        subtitle={t("subtitle")}
        actions={
          <Button
            onClick={() => {
              setError(null);
              setDisplayName("");
              setFile(null);
              setDialogOpen(true);
            }}
            size="sm"
            className="gap-1.5"
          >
            <Plus className="h-4 w-4" />
            {t("submit_card_title")}
          </Button>
        }
        className="mb-6"
      />

      {/* Stats Cards Row */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-4">
        <StatCard icon={ClipboardCheck} label={t("filter_all")} value={stats.total} />
        <StatCard icon={Loader2} label={t("status_pending_review")} value={stats.pending} accentClass={cn(stats.pending > 0 && "text-amber-500 bg-amber-500/10")} />
        <StatCard icon={CheckCircle} label={t("status_approved")} value={stats.approved} accentClass="text-emerald-500 bg-emerald-500/10" />
        <StatCard icon={CircleSlash} label={t("status_rejected")} value={stats.rejected} accentClass="text-rose-500 bg-rose-500/10" />
      </div>

      {/* Submission History Card */}
      <Card className="w-full">
        <CardContent className="p-0">
          {/* Filters Bar */}
          <div className="flex flex-col sm:flex-row gap-3 p-4 border-b">
            <SearchInput value={search} onChange={setSearch} placeholder={t("search_placeholder")} />
            <div className="inline-flex h-9 items-center justify-center rounded-lg bg-muted p-1 text-muted-foreground shrink-0 select-none border border-border/40">
              <button
                type="button"
                onClick={() => {
                  setStatusFilter("all");
                  setPage(1);
                }}
                className={cn(
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none",
                  statusFilter === "all"
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("filter_all")} ({stats.total})
              </button>
              <button
                type="button"
                onClick={() => {
                  setStatusFilter("pending_review");
                  setPage(1);
                }}
                className={cn(
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none",
                  statusFilter === "pending_review"
                    ? "bg-background text-amber-500 shadow-sm"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("status_pending_review")} ({stats.pending})
              </button>
              <button
                type="button"
                onClick={() => {
                  setStatusFilter("approved");
                  setPage(1);
                }}
                className={cn(
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none",
                  statusFilter === "approved"
                    ? "bg-background text-emerald-500 shadow-sm"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("status_approved")} ({stats.approved})
              </button>
              <button
                type="button"
                onClick={() => {
                  setStatusFilter("rejected");
                  setPage(1);
                }}
                className={cn(
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none",
                  statusFilter === "rejected"
                    ? "bg-background text-rose-500 shadow-sm"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("status_rejected")} ({stats.rejected})
              </button>
            </div>
          </div>

          {/* List Content */}
          {loading && !data ? (
            <div className="py-12"><Loading /></div>
          ) : fetchError ? (
            <div className="py-12"><ErrorState error={fetchError} onRetry={reload} /></div>
          ) : filteredSubmissions.length === 0 ? (
            <div className="p-4 border-b-0">
              <EmptyState
                title={t("no_submissions_title")}
                hint={t("no_submissions_hint")}
              />
            </div>
          ) : (
            <div className="w-full">
              {/* Table Header */}
              <div className="hidden md:grid grid-cols-12 gap-3 px-4 py-2.5 text-[11px] font-semibold text-muted-foreground border-b select-none">
                <div className="col-span-6">{t("table_display_name")}</div>
                <div className="col-span-3 text-center">{t("table_status")}</div>
                <div className="col-span-3">{t("table_created_at")}</div>
              </div>

              {/* Table Rows */}
              <div className="divide-y divide-border/60">
                {paginatedSubmissions.map((sub) => {
                  const isExpanded = expandedId === sub.id;
                  return (
                    <div key={sub.id} className="flex flex-col">
                      {/* Row Header */}
                      <div
                        onClick={() => setExpandedId(isExpanded ? null : sub.id)}
                        className={cn(
                          "grid grid-cols-1 md:grid-cols-12 gap-3 px-4 py-3.5 items-center cursor-pointer transition-colors text-sm hover:bg-muted/30",
                          isExpanded && "bg-muted/20",
                        )}
                      >
                        {/* Display Name */}
                        <div className="col-span-1 md:col-span-6 flex items-center gap-2 min-w-0">
                          {isExpanded ? (
                            <ChevronUp className="h-4 w-4 shrink-0 text-muted-foreground/80" />
                          ) : (
                            <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground/80" />
                          )}
                          <div className="truncate flex flex-col">
                            <span className="font-medium text-foreground truncate" title={sub.display_name}>
                              {sub.display_name}
                            </span>
                            <span className="font-mono text-[10px] text-muted-foreground/75 truncate mt-0.5">
                              ID: {sub.id}
                            </span>
                          </div>
                        </div>

                        {/* Status */}
                        <div className="col-span-1 md:col-span-3 md:text-center">
                          <SubmissionStatusBadge status={sub.status} />
                        </div>

                        {/* Created At */}
                        <div
                          className="col-span-1 md:col-span-3 text-muted-foreground text-xs"
                          title={formatAbsolute(sub.created_at, locale)}
                        >
                          {formatRelative(sub.created_at, now, locale)}
                        </div>
                      </div>

                      {/* Expandable details block */}
                      {isExpanded && (
                        <div className="px-10 py-3 bg-muted/20 border-t border-b border-border/40 text-xs text-muted-foreground space-y-2">
                          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                            <div>
                              <p className="font-semibold text-foreground mb-1">{t("field_context_ref")}</p>
                              <pre className="font-mono bg-background border rounded p-1.5 truncate select-all">{sub.context_ref}</pre>
                            </div>
                            {sub.image_ref && (
                              <div>
                                <p className="font-semibold text-foreground mb-1">{t("field_image_ref")}</p>
                                <pre className="font-mono bg-background border rounded p-1.5 truncate select-all">{sub.image_ref}</pre>
                              </div>
                            )}
                          </div>

                          {sub.status !== "pending_review" && (
                            <div className="pt-1 border-t border-border/20 mt-2 flex flex-col gap-1.5">
                              {sub.reviewed_by && (
                                <div>
                                  <span className="font-semibold text-foreground mr-1.5">{t("table_reviewed_by")}:</span>
                                  <code className="font-mono text-muted-foreground">{sub.reviewed_by}</code>
                                  {sub.reviewed_at && (
                                    <span className="text-[10px] text-muted-foreground/60 ml-2">
                                      ({formatAbsolute(sub.reviewed_at, locale)})
                                    </span>
                                  )}
                                </div>
                              )}
                              {sub.status === "rejected" && sub.reject_reason && (
                                <div className="text-rose-500 font-medium">
                                  <span className="font-semibold text-foreground mr-1.5">{t("table_reject_reason")}:</span>
                                  {sub.reject_reason}
                                </div>
                              )}
                            </div>
                          )}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>

              {/* Pagination Footer */}
              {filteredSubmissions.length > PAGE_SIZE && (
                <div className="p-4 border-t">
                  <Pagination
                    page={page}
                    total={filteredSubmissions.length}
                    pageSize={PAGE_SIZE}
                    onChange={setPage}
                  />
                </div>
              )}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Submit Modpack Dialog */}
      <Dialog open={dialogOpen} onOpenChange={(open) => !isSubmitting && setDialogOpen(open)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("submit_card_title")}</DialogTitle>
            <p className="text-xs text-muted-foreground">
              {t("submit_card_desc")}
            </p>
          </DialogHeader>

          <form onSubmit={handleFormSubmit} className="space-y-4 pt-2">
            {/* Error Banner */}
            {error && <MessageLine kind="error" message={error} compact />}

            {/* Display Name Input */}
            <div className="space-y-1.5">
              <Label htmlFor="displayName" className="text-xs font-semibold text-foreground">
                {t("display_name_label")} <span className="text-destructive">*</span>
              </Label>
              <Input
                id="displayName"
                placeholder={t("display_name_placeholder")}
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                disabled={isSubmitting}
                required
              />
            </div>

            {/* File Dropzone */}
            <div className="space-y-1.5">
              <Label className="text-xs font-semibold text-foreground">
                {t("file_label")} <span className="text-destructive">*</span>
              </Label>

              <input
                type="file"
                ref={fileInputRef}
                onChange={handleFileChange}
                accept=".tar.gz"
                className="hidden"
                disabled={isSubmitting}
              />

              <div
                onDragEnter={handleDrag}
                onDragOver={handleDrag}
                onDragLeave={handleDrag}
                onDrop={handleDrop}
                onClick={() => !isSubmitting && fileInputRef.current?.click()}
                className={cn(
                  "flex flex-col items-center justify-center border border-dashed rounded-lg p-6 text-center cursor-pointer transition-colors min-h-[140px]",
                  dragActive ? "border-primary bg-primary/5" : "border-border hover:bg-muted/30",
                  isSubmitting && "opacity-50 cursor-not-allowed",
                )}
              >
                {file ? (
                  <div className="flex flex-col items-center gap-2">
                    <FileText className="h-8 w-8 text-primary" />
                    <p className="text-xs font-medium text-foreground max-w-[240px] truncate" title={file.name}>
                      {file.name}
                    </p>
                    <p className="text-[10px] text-muted-foreground">{formatBytes(file.size)}</p>
                    {!isSubmitting && (
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        className="h-6 px-2 text-[10px] text-rose-500 hover:text-rose-600 hover:bg-rose-500/10 mt-1"
                        onClick={(e) => {
                          e.stopPropagation();
                          setFile(null);
                          if (fileInputRef.current) fileInputRef.current.value = "";
                        }}
                      >
                        <X className="mr-1 h-3 w-3" />
                        {t("clear_btn")}
                      </Button>
                    )}
                  </div>
                ) : (
                  <>
                    <Upload className="h-8 w-8 text-muted-foreground/80 mb-2" />
                    <p className="text-xs font-medium text-foreground">{t("file_drag_hint")}</p>
                    <p className="text-[10px] text-muted-foreground/70 mt-1">{t("file_hint")}</p>
                  </>
                )}
              </div>
            </div>

            <DialogFooter className="gap-2 pt-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => setDialogOpen(false)}
                disabled={isSubmitting}
                className="text-xs"
              >
                {t("common:cancel")}
              </Button>
              <Button
                type="submit"
                disabled={isSubmitting || !displayName.trim() || !file}
                className="text-xs"
              >
                {isSubmitting ? (
                  <>
                    <Loader2 className="mr-1.5 h-4 w-4 animate-spin" />
                    {submitStep === "create" ? t("submitting_create") : t("submitting_upload")}
                  </>
                ) : (
                  <>
                    <Upload className="mr-1.5 h-4 w-4" />
                    {t("submit_btn")}
                  </>
                )}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}
