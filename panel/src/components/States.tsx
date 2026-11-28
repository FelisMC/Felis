import { useState } from "react";
import { Loader2, AlertTriangle, Inbox, ShieldX, ShieldQuestion, Construction, Moon, SearchX, RefreshCw, Snowflake } from "lucide-react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { MAX_AUTO_RESTARTS, shownPhase, type StartFailure } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
import { Button } from "@/components/ui/button";
import { humanizeError } from "@/lib/api";
import { useTier } from "@/lib/tier";
import type { DesiredState, Phase, RetireState } from "@/lib/types";
import { cn } from "@/lib/utils";

export function Loading({ label }: { label?: string }) {
  const { t } = useTranslation("common");
  return (
    <div className="flex items-center justify-center gap-2 py-12 text-xs text-muted-foreground">
      <Loader2 className="h-3.5 w-3.5 animate-spin" />
      {label ?? t("loading")}
    </div>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const { t } = useTranslation("common");
  return (
    <div className="flex animate-fade-in flex-col items-center justify-center gap-2.5 py-12 text-center">
      <span className="inline-flex h-9 w-9 items-center justify-center rounded-full border border-destructive/25 bg-destructive/10 text-destructive">
        <AlertTriangle className="h-4 w-4" />
      </span>
      <p role="alert" className="max-w-sm text-xs text-muted-foreground">{humanizeError(error)}</p>
      {onRetry && (
        <Button size="sm" variant="outline" onClick={onRetry}>
          {t("try_again")}
        </Button>
      )}
    </div>
  );
}

export function RefreshError({ error, className }: { error: unknown; className?: string }) {
  const { t } = useTranslation("common");
  return (
    <div
      role="alert"
      className={cn(
        "flex animate-slide-down items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive",
        className,
      )}
    >
      <AlertTriangle className="h-3.5 w-3.5 shrink-0" />
      <span>
        {t("refresh_failed")} {humanizeError(error)}
      </span>
    </div>
  );
}

export function EmptyState({
  title,
  hint,
  children,
}: {
  title: string;
  hint?: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="flex animate-fade-in flex-col items-center justify-center gap-2.5 rounded-xl border border-dashed border-border/80 bg-card/40 px-4 py-12 text-center">
      <Inbox className="h-5 w-5 text-muted-foreground/75" />
      <div>
        <p className="text-[13px] font-medium text-foreground">{title}</p>
        {hint && <p className="mt-1 max-w-sm text-xs text-muted-foreground">{hint}</p>}
      </div>
      {children}
    </div>
  );
}

export function NotAuthorized() {
  const { t } = useTranslation("common");
  return (
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-20 text-center">
      <span className="inline-flex h-10 w-10 items-center justify-center rounded-full border border-destructive/25 bg-destructive/10 text-destructive">
        <ShieldX className="h-5 w-5" />
      </span>
      <div>
        <p className="text-sm font-medium">{t("not_authorized_title")}</p>
        <p className="mt-1 text-xs text-muted-foreground">{t("not_authorized_body")}</p>
      </div>
      <Link to="/" className="text-xs font-medium text-primary hover:underline">
        {t("back_to_dashboard")}
      </Link>
    </div>
  );
}

export function AccessUnknown({ error }: { error: unknown }) {
  const { t } = useTranslation("common");
  const { revalidate } = useTier();
  const [retrying, setRetrying] = useState(false);

  async function retry() {
    setRetrying(true);
    try {
      await revalidate();
    } finally {
      setRetrying(false);
    }
  }

  return (
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-20 text-center">
      <span className="inline-flex h-10 w-10 items-center justify-center rounded-full border border-amber-500/25 bg-amber-500/10 text-amber-500">
        <ShieldQuestion className="h-5 w-5" />
      </span>
      <div>
        <p className="text-sm font-medium">{t("access_unknown_title")}</p>
        <p className="mt-1 text-xs text-muted-foreground">{t("access_unknown_body")}</p>
        <p role="alert" className="mt-2 text-xs text-muted-foreground/80">
          {humanizeError(error)}
        </p>
      </div>
      <Button size="sm" variant="outline" onClick={() => void retry()} disabled={retrying}>
        {retrying ? <Loader2 className="animate-spin" /> : <RefreshCw />}
        {t("try_again")}
      </Button>
    </div>
  );
}

export interface NotFoundProps {
  title?: string;
  body?: string;
  linkTo?: string;
  linkLabel?: string;
}

export function NotFound({ title, body, linkTo = "/", linkLabel }: NotFoundProps) {
  const { t } = useTranslation("common");
  return (
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-20 text-center">
      <SearchX className="h-6 w-6 text-muted-foreground" />
      <div>
        <p className="text-sm font-medium">{title ?? t("not_found_title")}</p>
        <p className="mt-1 text-xs text-muted-foreground">{body ?? t("not_found_body")}</p>
      </div>
      <Link to={linkTo} className="text-xs font-medium text-primary hover:underline">
        {linkLabel ?? t("back_to_dashboard")}
      </Link>
    </div>
  );
}

export function PendingBackend({ endpoint, note }: { endpoint: string; note: string }) {
  const { t } = useTranslation("common");
  return (
    <div className="flex flex-col items-center justify-center gap-2.5 rounded-xl border border-dashed border-border/80 bg-card/40 px-4 py-12 text-center">
      <Construction className="h-5 w-5 text-muted-foreground" />
      <div>
        <p className="text-[13px] font-medium">{t("not_wired_yet")}</p>
        <p className="mt-1 max-w-md text-xs text-muted-foreground">{note}</p>
        <p className="mt-2 font-mono text-[11px] text-muted-foreground/80">
          {t("pending_backend_prefix")}{endpoint}
        </p>
      </div>
    </div>
  );
}

export interface NotYoursProps {
  title: string;
  body: string;
  linkTo?: string;
  linkLabel?: string;
}

export function NotYours({ title, body, linkTo = "/servers", linkLabel }: NotYoursProps) {
  const { t } = useTranslation("servers");
  return (
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-20 text-center">
      <span className="inline-flex h-10 w-10 items-center justify-center rounded-full border border-destructive/25 bg-destructive/10 text-destructive">
        <ShieldX className="h-5 w-5" />
      </span>
      <div>
        <p className="text-sm font-medium">{title}</p>
        <p className="mt-1 text-xs text-muted-foreground">{body}</p>
      </div>
      <Link to={linkTo} className="text-xs font-medium text-primary hover:underline">
        {linkLabel ?? t("my_servers_breadcrumb")}
      </Link>
    </div>
  );
}

export interface NotRunningProps {
  title: string;
  body: string;
  serverName: string;
  phase: Phase;
  desiredState?: DesiredState;
  failure?: StartFailure | null;
  autoRestarts?: number;
  retiring?: RetireState;
  diskFull?: boolean;
  onWoken: () => void;
}

export function NotRunning({
  title,
  body,
  serverName,
  phase,
  desiredState,
  failure = null,
  autoRestarts = 0,
  retiring,
  diskFull,
  onWoken,
}: NotRunningProps) {
  const { t } = useTranslation("servers");
  const shown = shownPhase({ phase, desiredState });
  const transitional = failure === null && (shown === "Starting" || shown === "Stopping" || shown === "Hibernating");

  let icon = <Moon className="h-6 w-6 text-muted-foreground/70" />;
  let copy = { title, body };
  if (failure !== null) {
    icon = <AlertTriangle className="h-6 w-6 text-destructive" />;
    copy =
      failure === "retrying"
        ? {
            title: t("server_failed_retrying_title"),
            body: t("server_failed_retrying_body", { used: autoRestarts, max: MAX_AUTO_RESTARTS }),
          }
        : { title: t("server_failed_title"), body: t("server_failed_body") };
  } else if (transitional) {
    icon = <Loader2 className="h-6 w-6 animate-spin text-muted-foreground/70" />;
    copy =
      shown === "Starting"
        ? { title: t("server_waking_title"), body: t("server_waking_body") }
        : shown === "Hibernating"
          ? { title: t("server_hibernating_title"), body: t("server_hibernating_body") }
          : { title: t("server_stopping_title"), body: t("server_stopping_body") };
  } else if (shown === "Hibernated") {
    icon = <Snowflake className="h-6 w-6 text-muted-foreground/70" />;
    copy = { title: t("server_hibernated_title"), body: t("server_hibernated_body") };
  }

  return (
    <div
      role="status"
      aria-live="polite"
      className="flex animate-fade-in flex-col items-center justify-center gap-3.5 rounded-xl border border-dashed border-border/80 bg-card/50 px-4 py-14 text-center"
    >
      {icon}
      <div>
        <p className="text-[13px] font-medium">{copy.title}</p>
        <p className="mx-auto mt-1 max-w-sm text-xs text-muted-foreground">{copy.body}</p>
      </div>
      {!transitional && (
        <PowerButton
          name={serverName}
          phase={phase}
          desiredState={desiredState}
          failed={failure !== null}
          retiring={retiring}
          diskFull={diskFull}
          onChanged={onWoken}
          className="items-center"
        />
      )}
    </div>
  );
}
