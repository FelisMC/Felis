import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
  Trash2,
} from "lucide-react";
import { SearchInput } from "@/components/SearchInput";
import {
  Card,
  CardContent,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { MessageLine } from "@/components/MessageLine";
import { InlineConfirm } from "@/components/InlineConfirm";
import { Loading, ErrorState, EmptyState, RefreshError } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { StatCard } from "@/components/StatCard";
import { PageHeader } from "@/components/PageHeader";
import { SubmissionStatusBadge } from "@/components/SubmissionStatusBadge";
import { api, humanizeError } from "@/lib/api";
import { uploadContext } from "@/lib/contextUpload";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS, useAsync, usePolling } from "@/lib/hooks";
import { formatRelative, formatAbsolute } from "@/lib/format";
import type { BuildStatus, Submission, SubmissionStatus } from "@/lib/types";

const PAGE_SIZE = 10;
const SEARCH_DEBOUNCE_MS = 300;
const DEFAULT_CONTEXT_LIMIT = 1024 * 1024 * 1024;

// The linked build's outcome as shown in a row's expanded details. Colors mirror
// the admin build page; the labels are player-facing, so they come from this
// page's namespace instead of the raw status string.
const BUILD_STATUS_STYLE: Record<BuildStatus, string> = {
  pending: "bg-amber-500/10 text-amber-500 border-amber-500/20 animate-pulse",
  building: "bg-blue-500/10 text-blue-500 border-blue-500/20 animate-pulse",
  succeeded: "bg-emerald-500/10 text-emerald-500 border-emerald-500/20",
  failed: "bg-rose-500/10 text-rose-500 border-rose-500/20",
  cancelled: "bg-zinc-500/10 text-zinc-400 border-zinc-500/20",
};

const BUILD_STATUS_I18N: Record<BuildStatus, string> = {
  pending: "build_status_pending",
  building: "build_status_building",
  succeeded: "build_status_succeeded",
  failed: "build_status_failed",
  cancelled: "build_status_cancelled",
};

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

  // Dialog State
  const [dialogOpen, setDialogOpen] = useState(false);

  // Form State
  const [displayName, setDisplayName] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [dragActive, setDragActive] = useState(false);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [submitStep, setSubmitStep] = useState<"create" | "upload" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [progress, setProgress] = useState<{ sent: number; total: number } | null>(null);
  // A submission whose upload stopped (a failure, or Pause): submitting again
  // sends to it, carrying on from its staged bytes when the file is the same,
  // instead of creating a second pending submission.
  const [pending, setPending] = useState<{ id: string; name: string; file: File; sent: number } | null>(null);
  const sentRef = useRef(0);
  const abortRef = useRef<AbortController | null>(null);

  // Row action state: withdraw arms a row (trash → confirm/cancel) before it
  // fires, and a failure lands in actionError above the list.
  const [confirmingWithdraw, setConfirmingWithdraw] = useState<string | null>(null);
  const [withdrawing, setWithdrawing] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const fileInputRef = useRef<HTMLInputElement>(null);

  // Search & Filtering State: the server filters and pages, the search box
  // settles for a moment before it asks.
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<"all" | SubmissionStatus>("all");
  const [page, setPage] = useState(1);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  // A search already asked (the first render's included) starts no timer, so
  // a page turned just after the list loads is not sent back to page one.
  useEffect(() => {
    const next = search.trim();
    if (next === query) return;
    const timer = setTimeout(() => {
      setQuery(next);
      setPage(1);
    }, SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [search, query]);

  const listMine = useCallback(
    () =>
      api.listMySubmissions({
        status: statusFilter === "all" ? undefined : statusFilter,
        query: query || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
    [statusFilter, query, page],
  );
  const { data, error: fetchError, loading, reload } = useAsync(listMine, [listMine], { keepPrevious: true });
  const submissions = useMemo<Submission[]>(() => data?.submissions ?? [], [data]);
  const matching = data?.total ?? 0;
  // A review lands whenever an admin gets to it and an approved pack builds on
  // its own, so the list keeps reading: every few seconds while a shown build
  // is still under way, at the slow pace otherwise.
  const building = submissions.some((s) => s.build_status === "pending" || s.build_status === "building");
  usePolling(reload, building ? STATUS_POLL_FAST_MS : STATUS_POLL_SLOW_MS);

  // The cards and filter chips count everything this player submitted, whatever is filtered.
  const stats = useMemo(() => {
    const c = data?.counts ?? { pending_review: 0, approved: 0, rejected: 0 };
    return {
      total: c.pending_review + c.approved + c.rejected,
      pending: c.pending_review,
      approved: c.approved,
      rejected: c.rejected,
    };
  }, [data]);
  const filtering = query !== "" || statusFilter !== "all";
  // A withdraw can empty the last page; step back to the one that now is.
  useEffect(() => {
    if (data && data.submissions.length === 0 && page > 1) setPage(Math.max(1, Math.ceil(data.total / PAGE_SIZE)));
  }, [data, page]);

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

  const [contextLimit, setContextLimit] = useState<number | null>(null);
  useEffect(() => {
    // A failed read keeps the 1 GiB default; the server still refuses past its cap.
    api.submissionLimits().then((l) => setContextLimit(l.max_context_bytes), () => undefined);
  }, []);

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
    // The server's cap; 1 GiB until it answers.
    const maxBytes = contextLimit ?? DEFAULT_CONTEXT_LIMIT;
    if (selectedFile.size > maxBytes) {
      setError(t("error_file_size", { size: formatBytes(selectedFile.size), limit: formatBytes(maxBytes) }));
      setFile(null);
      return;
    }
    setFile(selectedFile);
  };

  const handleFormSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);

    const trimmedName = pending ? pending.name : displayName.trim();
    if (!trimmedName) {
      setError(t("error_name_required"));
      return;
    }
    if (!file) {
      setError(t("error_file_required"));
      return;
    }

    setIsSubmitting(true);
    let target = pending;
    try {
      if (!target) {
        setSubmitStep("create");
        const sub = await api.createSubmission(trimmedName);
        target = { id: sub.id, name: trimmedName, file, sent: 0 };
      }

      // The context goes up in parts, so a large pack passes the edge's
      // per-request cap and a dropped connection resumes instead of restarting.
      setSubmitStep("upload");
      sentRef.current = 0;
      setProgress({ sent: 0, total: file.size });
      const controller = new AbortController();
      abortRef.current = controller;
      await uploadContext(target.id, file, {
        resume: pending !== null && pending.file === file,
        signal: controller.signal,
        onProgress: (sent, total, stored) => {
          // A stopped part is lost; the retry resumes from what the server holds.
          sentRef.current = stored;
          setProgress({ sent, total });
        },
      });

      setPending(null);
      setDisplayName("");
      setFile(null);
      if (fileInputRef.current) {
        fileInputRef.current.value = "";
      }
      setDialogOpen(false);
      reload();
    } catch (err) {
      if (target) {
        // The submission exists (and is in the list now); keep it for the retry.
        setPending({ ...target, file, sent: sentRef.current });
        if (!pending) reload();
      }
      const paused = err instanceof DOMException && err.name === "AbortError";
      setError(paused ? null : humanizeError(err));
    } finally {
      abortRef.current = null;
      setIsSubmitting(false);
      setSubmitStep(null);
      setProgress(null);
    }
  };

  const percent = progress && progress.total > 0 ? Math.floor((progress.sent * 100) / progress.total) : 0;

  // Withdraw retracts a still-pending submission and its uploaded context,
  // freeing the pending slot and the storage budget for a fresh submission.
  async function handleWithdraw(id: string) {
    if (withdrawing) return;
    setWithdrawing(id);
    setActionError(null);
    try {
      await api.withdrawSubmission(id);
      setConfirmingWithdraw(null);
      reload();
    } catch (err) {
      setActionError(humanizeError(err));
    } finally {
      setWithdrawing(null);
    }
  }

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
              // A stopped upload stays in the form so Submit carries it on.
              if (!pending) {
                setError(null);
                setDisplayName("");
                setFile(null);
              }
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

      {/* Row-action error (withdraw) */}
      {actionError && <MessageLine kind="error" message={actionError} compact />}
      {/* A later read that failed keeps the last list and says so above it. */}
      {!!fetchError && !!data && <RefreshError error={fetchError} />}

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
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("filter_all")} ({stats.total})
              </Button>
              <Button variant="ghost" size="sm"
                type="button"
                onClick={() => {
                  setStatusFilter("pending_review");
                  setPage(1);
                }}
                className={cn(
                  "font-medium",
                  statusFilter === "pending_review"
                    ? "bg-background text-amber-500"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("status_pending_review")} ({stats.pending})
              </Button>
              <Button variant="ghost" size="sm"
                type="button"
                onClick={() => {
                  setStatusFilter("approved");
                  setPage(1);
                }}
                className={cn(
                  "font-medium",
                  statusFilter === "approved"
                    ? "bg-background text-emerald-500"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("status_approved")} ({stats.approved})
              </Button>
              <Button variant="ghost" size="sm"
                type="button"
                onClick={() => {
                  setStatusFilter("rejected");
                  setPage(1);
                }}
                className={cn(
                  "font-medium",
                  statusFilter === "rejected"
                    ? "bg-background text-rose-500"
                    : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
                )}
              >
                {t("status_rejected")} ({stats.rejected})
              </Button>
            </div>
          </div>

          {/* List Content */}
          {loading && !data ? (
            <div className="py-12"><Loading /></div>
          ) : fetchError && !data ? (
            <div className="py-12"><ErrorState error={fetchError} onRetry={reload} /></div>
          ) : submissions.length === 0 ? (
            <div className="p-4 border-b-0">
              <EmptyState
                title={filtering ? t("search_no_results") : t("no_submissions_title")}
                hint={filtering ? t("search_no_results_hint") : t("no_submissions_hint")}
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
                {submissions.map((sub) => {
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
                            {sub.context_sha256 && (
                              <div>
                                <p className="font-semibold text-foreground mb-1">{t("field_context_sha256")}</p>
                                <pre className="font-mono bg-background border rounded p-1.5 truncate select-all" title={t("field_context_sha256_hint")}>{sub.context_sha256}</pre>
                              </div>
                            )}
                            {sub.image_ref && (
                              <div>
                                <p className="font-semibold text-foreground mb-1">{t("field_image_ref")}</p>
                                <pre className="font-mono bg-background border rounded p-1.5 truncate select-all">{sub.image_ref}</pre>
                              </div>
                            )}
                          </div>

                          {sub.status === "pending_review" && (
                            <div className="pt-2 border-t border-border/20 mt-2 flex flex-wrap items-center justify-between gap-2">
                              <span className="text-[11px] text-muted-foreground/80">{t("withdraw_hint")}</span>
                              {confirmingWithdraw === sub.id ? (
                                <InlineConfirm
                                  open={true}
                                  confirming={withdrawing === sub.id}
                                  onConfirm={() => handleWithdraw(sub.id)}
                                  onCancel={() => setConfirmingWithdraw(null)}
                                  confirmLabel={t("withdraw_confirm")}
                                  cancelLabel={t("withdraw_cancel")}
                                  className="flex shrink-0 items-center gap-1"
                                />
                              ) : (
                                <Button
                                  variant="outline"
                                  size="sm"
                                  className="h-7 shrink-0 border-destructive/30 text-destructive hover:bg-destructive/10 hover:text-destructive"
                                  onClick={() => setConfirmingWithdraw(sub.id)}
                                  disabled={withdrawing !== null}
                                  title={t("withdraw_hint")}
                                >
                                  <Trash2 className="mr-1.5 h-3.5 w-3.5" />
                                  {t("withdraw_btn")}
                                </Button>
                              )}
                            </div>
                          )}

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

                          {/* Linked build outcome — a submitter's only view of a
                              failed build (the build pages are admin-tier). */}
                          {sub.build_id && sub.build_status && (
                            <div className="pt-1 border-t border-border/20 mt-2 flex flex-col gap-1.5">
                              <div className="flex items-center gap-2 flex-wrap">
                                <span className="font-semibold text-foreground">{t("table_build_status")}:</span>
                                <Badge
                                  variant="outline"
                                  className={cn(
                                    "font-medium select-none pointer-events-none",
                                    BUILD_STATUS_STYLE[sub.build_status],
                                  )}
                                >
                                  {t(BUILD_STATUS_I18N[sub.build_status])}
                                </Badge>
                                <code className="font-mono text-[10px] text-muted-foreground/75">{sub.build_id}</code>
                              </div>
                              {sub.build_status === "failed" && sub.build_error && (
                                <div className="text-rose-500 font-medium break-all whitespace-pre-wrap">
                                  <span className="font-semibold text-foreground mr-1.5">{t("table_build_error")}:</span>
                                  {sub.build_error}
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
              {matching > PAGE_SIZE && (
                <div className="p-4 border-t">
                  <Pagination
                    page={page}
                    total={matching}
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

            {pending && !isSubmitting && (
              <p role="status" className="rounded-md border border-border bg-muted/40 p-2.5 text-xs text-muted-foreground">
                {file === pending.file
                  ? t("resume_hint", { name: pending.name, sent: formatBytes(pending.sent) })
                  : t("restart_hint", { name: pending.name })}
              </p>
            )}

            {/* Display Name Input */}
            <div className="space-y-1.5">
              <Label htmlFor="displayName" className="text-xs font-semibold text-foreground">
                {t("display_name_label")} <span className="text-destructive">*</span>
              </Label>
              <Input
                id="displayName"
                placeholder={t("display_name_placeholder")}
                value={pending ? pending.name : displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                disabled={isSubmitting || !!pending}
                required
              />
            </div>

            {/* File Dropzone */}
            <div className="space-y-1.5">
              <Label htmlFor="submission-file" className="text-xs font-semibold text-foreground">
                {t("file_label")} <span className="text-destructive">*</span>
              </Label>

              {/* The real file input stays in the tab order (visually hidden),
                  so a keyboard reaches the picker; the drop zone below shows
                  its focus ring. */}
              <input
                id="submission-file"
                type="file"
                ref={fileInputRef}
                onChange={handleFileChange}
                accept=".tar.gz"
                className="peer sr-only"
                disabled={isSubmitting}
              />

              <div
                onDragEnter={handleDrag}
                onDragOver={handleDrag}
                onDragLeave={handleDrag}
                onDrop={handleDrop}
                onClick={() => !isSubmitting && fileInputRef.current?.click()}
                className={cn(
                  "flex flex-col items-center justify-center border border-dashed rounded-lg p-6 text-center cursor-pointer transition-colors min-h-[140px] peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-focus-visible:ring-offset-2 peer-focus-visible:ring-offset-background",
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
                    <p className="text-[10px] text-muted-foreground/70 mt-1">
                      {t("file_hint", { limit: formatBytes(contextLimit ?? DEFAULT_CONTEXT_LIMIT, 0) })}
                    </p>
                  </>
                )}
              </div>

              {progress && (
                <div className="space-y-1.5 pt-1">
                  <div
                    role="progressbar"
                    aria-label={t("upload_progress_label")}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-valuenow={percent}
                    className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
                  >
                    <div
                      className="h-full rounded-full bg-primary transition-[width] duration-300 ease-out"
                      style={{ width: `${percent}%` }}
                    />
                  </div>
                  <p className="flex justify-between text-[10px] text-muted-foreground tabular-nums">
                    <span>{t("upload_progress", { sent: formatBytes(progress.sent), total: formatBytes(progress.total) })}</span>
                    <span>{percent}%</span>
                  </p>
                </div>
              )}
            </div>

            <DialogFooter className="gap-2 pt-2">
              {submitStep === "upload" ? (
                // Pausing keeps the staged parts; Submit carries on from them.
                <Button type="button" variant="outline" onClick={() => abortRef.current?.abort()} className="text-xs">
                  {t("pause_btn")}
                </Button>
              ) : (
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setDialogOpen(false)}
                  disabled={isSubmitting}
                  className="text-xs"
                >
                  {t("common:cancel")}
                </Button>
              )}
              <Button
                type="submit"
                disabled={isSubmitting || !(pending ? pending.name : displayName.trim()) || !file}
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
                    {pending && file === pending.file ? t("resume_btn") : t("submit_btn")}
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
