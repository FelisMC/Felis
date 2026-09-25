import { useCallback, useState } from "react";
import { Download, ShieldAlert, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import { cn } from "@/lib/utils";
import type { Build, BuildScan, ScanFinding, ScanSeverity } from "@/lib/types";

const SEVERITIES: ScanSeverity[] = ["CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"];

const SEVERITY_STYLE: Record<ScanSeverity, string> = {
  CRITICAL: "bg-rose-500/10 text-rose-500 border-rose-500/25",
  HIGH: "bg-orange-500/10 text-orange-500 border-orange-500/25",
  MEDIUM: "bg-amber-500/10 text-amber-500 border-amber-500/25",
  LOW: "bg-sky-500/10 text-sky-500 border-sky-500/25",
  UNKNOWN: "bg-zinc-500/10 text-zinc-400 border-zinc-500/25",
};

const GRID = "md:grid md:grid-cols-[minmax(0,10rem)_5.5rem_minmax(0,1fr)_minmax(0,11rem)_minmax(0,1fr)] md:gap-3";

/** The scan a finished build's scan gate kept: its verdict under the policy it
 *  ran with, the findings, and the full Trivy report and SBOM to download. A
 *  build still running or cancelled has none, so nothing is fetched for it. */
export function BuildScanPanel({ build }: { build: Build }) {
  const scanned = build.status === "succeeded" || build.status === "failed";
  if (!scanned) return null;
  return <ScanView buildId={build.id} />;
}

function ScanView({ buildId }: { buildId: string }) {
  const { t } = useTranslation("admin");
  const load = useCallback(() => api.getBuildScan(buildId), [buildId]);
  const { data: scan, error, loading, reload } = useAsync(load, [load]);

  if (scan === null && loading) {
    return <p className="mt-3 text-[11px] text-muted-foreground">{t("scan_loading")}</p>;
  }
  if (scan === null && error) {
    if ((error as { code?: string }).code === "scan_not_found") {
      return (
        <p className="mt-3 rounded-md border border-dashed border-border px-3 py-2 text-[11px] text-muted-foreground">
          {t("scan_none")}
        </p>
      );
    }
    return (
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <MessageLine kind="error" message={t("scan_load_failed", { reason: humanizeError(error) })} className="flex-1" />
        <Button variant="outline" size="sm" onClick={reload}>
          {t("common:try_again")}
        </Button>
      </div>
    );
  }
  if (scan === null) return null;
  return <ScanReport scan={scan} />;
}

function ScanReport({ scan }: { scan: BuildScan }) {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const [downloadError, setDownloadError] = useState<string | null>(null);
  const [downloading, setDownloading] = useState<"report" | "sbom" | null>(null);
  const s = scan.summary;
  const total = SEVERITIES.reduce((n, sev) => n + (s.counts[sev] ?? 0), 0);
  const Icon = s.blocked ? ShieldAlert : ShieldCheck;

  const download = async (doc: "report" | "sbom") => {
    setDownloading(doc);
    setDownloadError(null);
    try {
      await api.downloadBuildScanDocument(scan.build_id, doc);
    } catch (e) {
      setDownloadError(t("scan_download_failed", { reason: humanizeError(e) }));
    } finally {
      setDownloading(null);
    }
  };

  return (
    <section
      aria-label={t("scan_title")}
      className={cn(
        "mt-3 rounded-lg border p-3 space-y-3",
        s.blocked ? "border-rose-500/30 bg-rose-500/[0.03]" : "border-emerald-500/25 bg-emerald-500/[0.03]",
      )}
    >
      <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
        <Icon className={cn("h-4 w-4 mt-0.5 shrink-0", s.blocked ? "text-rose-500" : "text-emerald-500")} />
        <div className="min-w-0 flex-1 space-y-0.5">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="text-xs font-semibold text-foreground">{t("scan_title")}</h3>
            <Badge
              variant="outline"
              className={cn(
                "px-1.5 py-0 text-[10px] font-semibold uppercase tracking-wider",
                s.blocked
                  ? "bg-rose-500/10 text-rose-500 border-rose-500/20"
                  : "bg-emerald-500/10 text-emerald-500 border-emerald-500/20",
              )}
            >
              {s.blocked ? t("scan_blocked") : t("scan_passed")}
            </Badge>
          </div>
          <p className="text-[11px] text-muted-foreground">
            <span title={formatAbsolute(scan.scanned_at, locale)}>
              {t("scan_meta", { when: formatRelative(scan.scanned_at, Date.now(), locale), count: s.packages })}
            </span>
            {" · "}
            {t(s.policy.fail_unfixed ? "scan_policy_unfixed" : "scan_policy", {
              severities: s.policy.fail_on.join(", "),
            })}
            {s.policy.accept && s.policy.accept.length > 0 && (
              <>
                {" · "}
                <span title={s.policy.accept.join(", ")} className="underline decoration-dotted underline-offset-2">
                  {t("scan_policy_accepts", { count: s.policy.accept.length })}
                </span>
              </>
            )}
          </p>
        </div>
        {/* Narrow screens: the downloads take their own line under the text. */}
        <div className="flex w-full flex-wrap items-center gap-1.5 pl-7 sm:w-auto sm:pl-0">
          <DownloadButton
            label={t("scan_download_report")}
            kept={scan.has_report}
            busy={downloading === "report"}
            onClick={() => download("report")}
          />
          <DownloadButton
            label={t("scan_download_sbom")}
            kept={scan.has_sbom}
            busy={downloading === "sbom"}
            onClick={() => download("sbom")}
          />
        </div>
      </div>

      <ul className="flex flex-wrap gap-1.5" aria-label={t("scan_counts_label")}>
        {SEVERITIES.map((sev) => {
          const n = s.counts[sev] ?? 0;
          const blocking = s.blocking_counts[sev] ?? 0;
          return (
            <li
              key={sev}
              title={blocking > 0 ? t("scan_blocking_count", { count: blocking }) : undefined}
              className={cn(
                "flex items-center gap-1.5 rounded border px-2 py-0.5 font-mono text-[10px] font-semibold",
                n > 0 ? SEVERITY_STYLE[sev] : "border-border text-muted-foreground/60",
                blocking > 0 && "ring-1 ring-current",
              )}
            >
              <span>{sev}</span>
              <span>{n}</span>
            </li>
          );
        })}
      </ul>

      {downloadError && <MessageLine kind="error" message={downloadError} compact />}

      {total === 0 ? (
        <p className="text-[11px] text-muted-foreground">{t("scan_clean")}</p>
      ) : (
        <div className="rounded-md border border-border bg-background/60">
          <div className={cn("hidden px-3 py-2 text-[10px] font-semibold text-muted-foreground border-b", GRID)}>
            <div>{t("scan_col_id")}</div>
            <div>{t("scan_col_severity")}</div>
            <div>{t("scan_col_package")}</div>
            <div>{t("scan_col_version")}</div>
            <div>{t("scan_col_target")}</div>
          </div>
          <ul className="max-h-72 overflow-y-auto divide-y divide-border">
            {s.findings.map((f, i) => (
              <FindingRow key={`${f.id}-${f.package ?? ""}-${f.target}-${i}`} finding={f} />
            ))}
          </ul>
          {total > s.findings.length && (
            <p className="border-t border-border px-3 py-2 text-[10px] text-muted-foreground">
              {t("scan_showing", { shown: s.findings.length, total })}
            </p>
          )}
        </div>
      )}
    </section>
  );
}

function FindingRow({ finding: f }: { finding: ScanFinding }) {
  const { t } = useTranslation("admin");
  const secret = f.kind === "secret";
  return (
    <li className={cn("px-3 py-2 text-[11px] flex flex-wrap items-center gap-x-3 gap-y-1", GRID, f.blocking && "bg-rose-500/[0.04]")}>
      <div className="flex min-w-0 items-center gap-1.5">
        <span className="font-mono font-medium text-foreground select-all truncate" title={f.id}>{f.id}</span>
        {f.blocking && (
          <span className="shrink-0 rounded bg-rose-500/15 px-1 text-[9px] font-bold uppercase tracking-wide text-rose-500">
            {t("scan_blocks")}
          </span>
        )}
        {f.accepted && (
          <span
            title={t("scan_accepted_title")}
            className="shrink-0 rounded bg-zinc-500/15 px-1 text-[9px] font-bold uppercase tracking-wide text-muted-foreground"
          >
            {t("scan_accepted")}
          </span>
        )}
      </div>
      <div>
        <span className={cn("rounded border px-1.5 py-px font-mono text-[9px] font-semibold", SEVERITY_STYLE[f.severity])}>
          {f.severity}
        </span>
      </div>
      <div className="w-full min-w-0 truncate md:w-auto" title={secret ? f.title : f.package}>
        {secret ? t("scan_secret", { title: f.title ?? f.id }) : <span className="font-mono">{f.package}</span>}
      </div>
      <div className="min-w-0 truncate font-mono text-muted-foreground">
        {secret ? "—" : (
          <>
            {f.installed}
            {" → "}
            {f.fixed ? <span className="text-emerald-500">{f.fixed}</span> : <span className="italic">{t("scan_no_fix")}</span>}
          </>
        )}
      </div>
      <div className="w-full min-w-0 truncate font-mono text-[10px] text-muted-foreground md:w-auto" title={f.target}>
        {f.target}
      </div>
    </li>
  );
}

function DownloadButton({ label, kept, busy, onClick }: {
  label: string;
  kept: boolean;
  busy: boolean;
  onClick: () => void;
}) {
  const { t } = useTranslation("admin");
  return (
    <Button
      variant="outline"
      size="sm"
      className="h-7 gap-1 px-2 text-[11px]"
      disabled={!kept || busy}
      title={kept ? undefined : t("scan_not_kept")}
      onClick={onClick}
    >
      <Download className="h-3 w-3" />
      {label}
    </Button>
  );
}
