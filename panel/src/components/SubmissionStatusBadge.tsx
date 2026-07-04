import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import type { SubmissionStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

const STATUS_STYLE: Record<SubmissionStatus, string> = {
  pending_review: "bg-amber-500/10 text-amber-500 border-amber-500/20",
  approved: "bg-emerald-500/10 text-emerald-500 border-emerald-500/20",
  rejected: "bg-rose-500/10 text-rose-500 border-rose-500/20",
};

const STATUS_I18N_KEY: Record<SubmissionStatus, string> = {
  pending_review: "status_pending_review",
  approved: "status_approved",
  rejected: "status_rejected",
};

interface Props {
  status: SubmissionStatus;
}

export function SubmissionStatusBadge({ status }: Props) {
  const { t } = useTranslation("submissions");
  return (
    <Badge
      variant="outline"
      className={cn("font-medium select-none pointer-events-none", STATUS_STYLE[status])}
    >
      {t(STATUS_I18N_KEY[status])}
    </Badge>
  );
}
