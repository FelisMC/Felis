import { useId } from "react";
import { useTranslation } from "react-i18next";

const HOUR_MS = 3600_000;
// Samples further apart than this belong to separate runs and are not joined.
const GAP_MS = 45_000;

/**
 * AreaChart draws the last hour of one series up to now. The time axis starts at the oldest sample
 * held, so a fresh start fills the width; max fixes the top of the scale, otherwise the peak sets it.
 */
export function AreaChart({ points, label, max }: { points: { at: string; value: number }[]; label: string; max?: number }) {
  const { t } = useTranslation("common");
  const gradient = useId();
  const now = Date.now();
  const shown = points
    .map((p) => ({ at: Date.parse(p.at), value: p.value }))
    .filter((p) => p.at >= now - HOUR_MS);
  const start = shown[0]?.at ?? now;
  const span = Math.max(now - start, 1);
  const top = Math.max(max ?? 0, ...shown.map((p) => p.value * 1.15), 1);
  const x = (at: number) => ((at - start) / span) * 300;
  const y = (value: number) => 58 - (Math.min(value, top) / top) * 52;

  const runs: { at: number; value: number }[][] = [];
  for (const p of shown) {
    const run = runs[runs.length - 1];
    if (!run || p.at - run[run.length - 1].at > GAP_MS) runs.push([p]);
    else run.push(p);
  }
  const minutes = Math.round(span / 60_000);

  return (
    <div>
      <div className="relative">
        <svg role="img" aria-label={label} viewBox="0 0 300 60" preserveAspectRatio="none" className="h-16 w-full text-primary">
          <defs>
            <linearGradient id={gradient} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="currentColor" stopOpacity="0.28" />
              <stop offset="100%" stopColor="currentColor" stopOpacity="0" />
            </linearGradient>
          </defs>
          <path d="M0 59.5H300" className="stroke-border" fill="none" vectorEffect="non-scaling-stroke" />
          {shown.length >= 2 && runs.map((run, i) => {
            const line = run.map((p, j) => `${j === 0 ? "M" : "L"}${x(p.at).toFixed(1)},${y(p.value).toFixed(1)}`).join(" ");
            return run.length < 2 ? (
              <circle key={i} cx={x(run[0].at)} cy={y(run[0].value)} r="2" fill="currentColor" />
            ) : (
              <g key={i}>
                <path d={`${line} L${x(run[run.length - 1].at).toFixed(1)},60 L${x(run[0].at).toFixed(1)},60 Z`} fill={`url(#${gradient})`} />
                <path d={line} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
              </g>
            );
          })}
        </svg>
        {shown.length < 2 && (
          <p className="absolute inset-0 flex items-center justify-center text-[11px] text-muted-foreground">{t("chart_collecting")}</p>
        )}
      </div>
      <div className="mt-1 flex justify-between text-[10px] text-muted-foreground">
        <span>{shown.length < 2 ? "" : minutes >= 59 ? t("chart_since_hour") : t("chart_since_minutes", { count: Math.max(minutes, 1) })}</span>
        <span>{t("chart_now")}</span>
      </div>
    </div>
  );
}
