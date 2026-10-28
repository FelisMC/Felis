import { useTranslation } from "react-i18next";
import { formatAbsolute } from "@/lib/format";
import type { AuditEntry } from "@/lib/types";
import { cn } from "@/lib/utils";

interface Props {
  entries: AuditEntry[];
  /** Makes each row open its entry; without it the rows are plain text. */
  onOpen?: (entry: AuditEntry) => void;
}

// Below md each row is a card: the table elements turn into blocks and the header row is hidden.
export function AuditTable({ entries, onOpen }: Props) {
  const { t, i18n } = useTranslation("admin");
  const head = "px-4 py-3 font-medium text-muted-foreground";
  const cell = "block md:table-cell md:px-4 md:py-3";
  return (
    <table className="block w-full text-sm md:table">
      <thead className="hidden md:table-header-group">
        <tr className="border-b border-border bg-muted/30 text-left">
          <th scope="col" className={head}>{t("audit_col_time")}</th>
          <th scope="col" className={head}>{t("audit_col_actor")}</th>
          <th scope="col" className={head}>{t("audit_col_action")}</th>
          <th scope="col" className={head}>{t("audit_col_server")}</th>
        </tr>
      </thead>
      <tbody className="block md:table-row-group">
        {entries.map((entry) => {
          const time = formatAbsolute(entry.created_at, i18n.language);
          return (
            <tr
              key={entry.id}
              onClick={onOpen && (() => onOpen(entry))}
              className={cn(
                "block space-y-0.5 border-b border-border/50 px-4 py-3 transition-colors md:table-row md:space-y-0 md:p-0",
                onOpen && "cursor-pointer hover:bg-muted/20",
              )}
            >
              <td className={cn(cell, "text-xs text-muted-foreground md:whitespace-nowrap")}>
                {onOpen ? (
                  <button
                    type="button"
                    aria-label={t("audit_open_detail", { action: entry.action, time })}
                    className="rounded-sm text-left hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  >
                    {time}
                  </button>
                ) : (
                  time
                )}
              </td>
              <td className={cn(cell, "break-all")}>
                <span className="text-xs text-muted-foreground md:hidden">{t("audit_col_actor")}: </span>
                {entry.actor}
              </td>
              <td className={cn(cell, "break-all font-mono text-xs font-medium")}>{entry.action}</td>
              <td className={cn(cell, "break-all text-muted-foreground")}>
                <span className="text-xs md:hidden">{t("audit_col_server")}: </span>
                {entry.server_name ?? "—"}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}
