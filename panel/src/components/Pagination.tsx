import { ChevronLeft, ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";

interface Props {
  page: number;
  pageSize: number;
  total: number;
  onChange: (page: number) => void;
}

export function Pagination({ page, pageSize, total, onChange }: Props) {
  const { t } = useTranslation("common");
  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  return (
    <div className="mt-auto flex items-center justify-center gap-1.5">
      <Button
        variant="outline"
        size="sm"
        disabled={page <= 1}
        onClick={() => onChange(page - 1)}
      >
        <ChevronLeft className="h-3.5 w-3.5" />
        {t("pagination_prev")}
      </Button>
      <span className="min-w-[5.5rem] text-center font-mono text-[11px] text-muted-foreground tabular-nums">
        {t("pagination_of", { current: page, total: totalPages })}
      </span>
      <Button
        variant="outline"
        size="sm"
        disabled={page >= totalPages}
        onClick={() => onChange(page + 1)}
      >
        {t("pagination_next")}
        <ChevronRight className="h-3.5 w-3.5" />
      </Button>
    </div>
  );
}
