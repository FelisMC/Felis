import { useState, useMemo } from "react";
import { ClipboardCheck, CheckCircle2, CircleSlash, ChevronDown, ChevronUp, Check, X, Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { StatCard } from "@/components/StatCard";
import { PageHeader } from "@/components/PageHeader";
import { SubmissionStatusBadge } from "@/components/SubmissionStatusBadge";
import { SearchInput } from "@/components/SearchInput";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { MessageLine } from "@/components/MessageLine";
import { Loading, ErrorState, EmptyState } from "@/components/States";
import { Pagination } from "@/components/Pagination";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { formatRelative, formatAbsolute } from "@/lib/format";
import type { Submission, SubmissionStatus } from "@/lib/types";

const PAGE_SIZE = 10;

export function SubmissionsPage() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const now = Date.now();

  const { data, error, loading, reload } = useAsync(() => api.listSubmissions(), []);
  const submissions: Submission[] = data ?? [];

  // Dialog State
  const [rejectDialogOpen, setRejectDialogOpen] = useState(false);
  const [rejectingId, setRejectingId] = useState<string | null>(null);
  const [rejectReason, setRejectReason] = useState("");
  const [actionError, setActionError] = useState<string | null>(null);
  
  // Pending actions (for button spinners)
  const [busyId, setBusyId] = useState<string | null>(null);
  const [busyType, setBusyType] = useState<"approve" | "reject" | null>(null);

  // Search & Filtering State
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<"all" | SubmissionStatus>("all");
  const [page, setPage] = useState(1);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  // Stats
  const stats = useMemo(() => {
    const total = submissions.length;
    const pending = submissions.filter((s) => s.status === "pending_review").length;
    const approved = submissions.filter((s) => s.status === "approved").length;
    const rejected = submissions.filter((s) => s.status === "rejected").length;
    return { total, pending, approved, rejected };
  }, [submissions]);

  // Filtered & Paginated Submissions
  const filteredSubmissions = useMemo(() => {
    let list = [...submissions];

    // 1. Search Filter
    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter(
        (s) =>
          s.display_name.toLowerCase().includes(q) ||
          s.submitted_by.toLowerCase().includes(q) ||
          s.id.toLowerCase().includes(q),
      );
    }

    // 2. Status Filter
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

  async function handleApprove(id: string) {
    if (busyId) return;
    setBusyId(id);
    setBusyType("approve");
    setActionError(null);
    try {
      await api.approveSubmission(id);
      reload();
    } catch (err) {
      setActionError(humanizeError(err));
    } finally {
      setBusyId(null);
      setBusyType(null);
    }
  }

  function openRejectDialog(id: string) {
    setRejectingId(id);
    setRejectReason("");
    setActionError(null);
    setRejectDialogOpen(true);
  }

  async function handleRejectSubmit(e?: React.FormEvent) {
    e?.preventDefault();
    if (!rejectingId || !rejectReason.trim()) return;
    setBusyId(rejectingId);
    setBusyType("reject");
    setActionError(null);
    try {
      await api.rejectSubmission(rejectingId, rejectReason.trim());
      setRejectDialogOpen(false);
      reload();
    } catch (err) {
      setActionError(humanizeError(err));
    } finally {
      setBusyId(null);
      setBusyType(null);
    }
  }

  return (
    <div className="space-y-6">
      <PageHeader icon={ClipboardCheck} title={t("submissions_title")} subtitle={t("submissions_subtitle")} className="mb-6" />

      {/* Action Error Alert */}
      {actionError && <MessageLine kind="error" message={actionError} compact />}

      {/* Stats Cards Row */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-4">
        <StatCard icon={ClipboardCheck} label={t("filter_all")} value={stats.total} />
        <StatCard icon={Loader2} label={t("status_pending_review")} value={stats.pending} accentClass={cn(stats.pending > 0 && "text-amber-500 bg-amber-500/10")} />
        <StatCard icon={CheckCircle2} label={t("status_approved")} value={stats.approved} accentClass="text-emerald-500 bg-emerald-500/10" />
        <StatCard icon={CircleSlash} label={t("status_rejected")} value={stats.rejected} accentClass="text-rose-500 bg-rose-500/10" />
      </div>

      {/* Main Table Card */}
      <Card className="w-full">
        <CardContent className="p-0">
          {/* Filters Bar */}
          <div className="flex flex-col sm:flex-row gap-3 p-4 border-b">
            <SearchInput value={search} onChange={setSearch} placeholder="搜索模组包名称或提交人..." />
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
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
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
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
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
                  "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-xs font-semibold transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
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
          ) : error ? (
            <div className="py-12"><ErrorState error={error} onRetry={reload} /></div>
          ) : filteredSubmissions.length === 0 ? (
            <div className="p-4 border-b-0">
              <EmptyState
                title={search.trim() || statusFilter !== "all" ? "无匹配结果" : t("no_submissions_title")}
                hint={search.trim() || statusFilter !== "all" ? "尝试更换搜索词或筛选条件" : t("no_submissions_hint")}
              />
            </div>
          ) : (
            <div className="w-full">
              {/* Table Header */}
              <div className="hidden md:grid grid-cols-12 gap-3 px-4 py-2.5 text-[11px] font-semibold text-muted-foreground border-b select-none">
                <div className="col-span-4">{t("table_display_name")}</div>
                <div className="col-span-3">{t("table_submitted_by")}</div>
                <div className="col-span-2 text-center">{t("table_status")}</div>
                <div className="col-span-2">{t("table_created_at")}</div>
                <div className="col-span-1 text-right">{t("table_action")}</div>
              </div>

              {/* Table Body */}
              <div className="divide-y divide-border select-text">
                {paginatedSubmissions.map((sub) => {
                  const isExpanded = expandedId === sub.id;
                  const isBusyApprove = busyId === sub.id && busyType === "approve";
                  const isBusyReject = busyId === sub.id && busyType === "reject";
                  return (
                    <div key={sub.id} className="flex flex-col">
                      <div
                        className={cn(
                          "grid grid-cols-1 md:grid-cols-12 gap-3 items-center px-4 py-3 text-xs transition-colors hover:bg-accent/25 cursor-pointer",
                          isExpanded && "bg-muted/10",
                        )}
                        onClick={() => setExpandedId(isExpanded ? null : sub.id)}
                      >
                        {/* Name */}
                        <div className="col-span-4 flex items-center gap-2 min-w-0">
                          {isExpanded ? (
                            <ChevronUp className="h-4 w-4 shrink-0 text-muted-foreground/60" />
                          ) : (
                            <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground/60" />
                          )}
                          <span className="font-semibold truncate">{sub.display_name}</span>
                          <span className="text-[10px] text-muted-foreground font-mono bg-muted/40 px-1 py-0.2 rounded shrink-0">
                            {sub.id}
                          </span>
                        </div>

                        {/* Submitter */}
                        <div className="col-span-3 truncate text-muted-foreground" title={sub.submitted_by}>
                          {sub.submitted_by}
                        </div>

                        {/* Status */}
                        <div className="col-span-2 md:text-center py-0.5">
                          <SubmissionStatusBadge status={sub.status} />
                        </div>

                        {/* Created At */}
                        <div
                          className="col-span-2 text-muted-foreground"
                          title={formatAbsolute(sub.created_at, locale)}
                        >
                          {formatRelative(sub.created_at, now, locale)}
                        </div>

                        {/* Action buttons (only in table row if NOT pending, else show triggers) */}
                        <div className="col-span-1 text-right" onClick={(e) => e.stopPropagation()}>
                          {sub.status === "pending_review" ? (
                            <div className="flex items-center justify-end gap-1.5">
                              <Button
                                size="icon"
                                variant="outline"
                                className="h-7 w-7 text-emerald-500 hover:text-emerald-600 border-emerald-500/20 hover:bg-emerald-500/10 focus-visible:ring-emerald-500"
                                onClick={() => handleApprove(sub.id)}
                                disabled={!!busyId}
                                title={t("approve_btn")}
                              >
                                {isBusyApprove ? (
                                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                                ) : (
                                  <Check className="h-3.5 w-3.5" />
                                )}
                              </Button>
                              <Button
                                size="icon"
                                variant="outline"
                                className="h-7 w-7 text-rose-500 hover:text-rose-600 border-rose-500/20 hover:bg-rose-500/10 focus-visible:ring-rose-500"
                                onClick={() => openRejectDialog(sub.id)}
                                disabled={!!busyId}
                                title={t("reject_btn")}
                              >
                                {isBusyReject ? (
                                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                                ) : (
                                  <X className="h-3.5 w-3.5" />
                                )}
                              </Button>
                            </div>
                          ) : (
                            <span className="text-[10px] text-muted-foreground/60 font-medium">
                              —
                            </span>
                          )}
                        </div>
                      </div>

                      {/* Expandable details block */}
                      {isExpanded && (
                        <div className="px-10 py-3 bg-muted/20 border-t border-b border-border/40 text-xs text-muted-foreground space-y-2">
                          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                            <div>
                              <p className="font-semibold text-foreground mb-1">构建上下文引用 (Context Ref)</p>
                              <pre className="font-mono bg-background border rounded p-1.5 truncate select-all">{sub.context_ref}</pre>
                            </div>
                            {sub.image_ref && (
                              <div>
                                <p className="font-semibold text-foreground mb-1">目标镜像引用 (Image Ref)</p>
                                <pre className="font-mono bg-background border rounded p-1.5 truncate select-all">{sub.image_ref}</pre>
                              </div>
                            )}
                          </div>

                          {sub.build_id && (
                            <div>
                              <p className="font-semibold text-foreground">关联构建任务 (Build ID)</p>
                              <code className="font-mono bg-background border rounded px-1.5 py-0.5">{sub.build_id}</code>
                            </div>
                          )}

                          {sub.status !== "pending_review" && (
                            <div className="pt-1 border-t border-border/20 mt-2 flex flex-col gap-1.5">
                              <div>
                                <span className="font-semibold text-foreground">{t("reviewer")}:</span>{" "}
                                <span>{sub.reviewed_by}</span>
                              </div>
                              {sub.reviewed_at && (
                                <div>
                                  <span className="font-semibold text-foreground">{t("reviewed_at")}:</span>{" "}
                                  <span>{formatAbsolute(sub.reviewed_at, locale)} ({formatRelative(sub.reviewed_at, now, locale)})</span>
                                </div>
                              )}
                              {sub.status === "rejected" && sub.reject_reason && (
                                <div className="bg-rose-500/5 border border-rose-500/10 rounded-md p-2 mt-1">
                                  <span className="font-semibold text-rose-500 block mb-1">{t("reject_reason")}:</span>
                                  <p className="text-foreground whitespace-pre-wrap leading-relaxed">{sub.reject_reason}</p>
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

              {/* Pagination */}
              {filteredSubmissions.length > PAGE_SIZE && (
                <div className="p-4 border-t">
                  <Pagination
                    page={page}
                    pageSize={PAGE_SIZE}
                    total={filteredSubmissions.length}
                    onChange={setPage}
                  />
                </div>
              )}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Reject Modal Dialog */}
      <Dialog open={rejectDialogOpen} onOpenChange={(o) => {
        setRejectDialogOpen(o);
        if (!o) {
          setRejectingId(null);
          setRejectReason("");
          setActionError(null);
        }
      }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("reject_dialog_title")}</DialogTitle>
            <DialogDescription>
              {t("reject_dialog_desc")}
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleRejectSubmit} className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="rejectReasonInput" className="text-xs font-medium text-muted-foreground">
                {t("reject_reason_label")} *
              </Label>
              <textarea
                id="rejectReasonInput"
                placeholder={t("reject_reason_placeholder")}
                value={rejectReason}
                onChange={(e) => setRejectReason(e.target.value)}
                maxLength={1000}
                disabled={!!busyId}
                required
                className="flex min-h-[100px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
              />
            </div>
            <ConfirmFooter
              onCancel={() => setRejectDialogOpen(false)}
              onConfirm={() => handleRejectSubmit()}
              disabled={!!busyId || !rejectReason.trim()}
              loading={!!busyId && busyType === "reject"}
              cancelLabel={t("common:cancel")}
              confirmLabel={t("reject_dialog_submit")}
              confirmVariant="destructive"
            />
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}