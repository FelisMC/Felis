import i18next from "i18next";
import { humanizeError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import type { FileOp, FileOpError } from "@/lib/types";

/** opErrorText says why an op ended failed. The codes an extraction refuses an
 *  archive with name the entry at fault; the rest are the codes the file routes
 *  answer, worded as they are there. */
export function opErrorText(op: FileOp["op"], e: FileOpError): string {
  const t = i18next.getFixedT(null, "files");
  const entry = e.entry ?? "";
  switch (e.code) {
    case "file_exists":
      return op === "upload"
        ? t("op_upload_exists")
        : t("op_unzip_conflicts", { count: e.conflict_count ?? e.conflicts?.length ?? 0 });
    case "volume_full":
      return e.need !== undefined && e.avail !== undefined
        ? t("op_volume_full", { need: formatBytes(e.need), avail: formatBytes(e.avail) })
        : humanizeError({ status: 0, code: e.code, message: e.message });
    case "archive_invalid":
      return entry ? t("archive_invalid_entry", { entry }) : t("archive_invalid");
    case "archive_unsafe":
    case "archive_symlink":
    case "type_conflict":
      return t(e.code, { entry });
    // The Job's own condition reason rides in the message; a deadline is the
    // one worth its own words.
    case "job_failed":
      return e.message.includes("DeadlineExceeded") ? t("job_timed_out") : t("job_failed");
    default:
      return humanizeError({ status: 0, code: e.code, message: e.message });
  }
}
