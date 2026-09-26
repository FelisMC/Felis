import { useState } from "react";
import { Loader2, AlertTriangle, Inbox, ShieldX, ShieldQuestion, Construction, Moon, SearchX, RefreshCw } from "lucide-react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { MAX_AUTO_RESTARTS, shownPhase, type StartFailure } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
import { Button } from "@/components/ui/button";
import { humanizeError } from "@/lib/api";
import { useTier } from "@/lib/tier";
import type { Phase, RetireState } from "@/lib/types";
import { cn } from "@/lib/utils";

export function Loading({ label }: { label?: string }) {
  const { t } = useTranslation("common");
  return (
    <div className="flex items-center justify-center gap-2 py-16 text-sm text-muted-foreground">
      <Loader2 className="h-4 w-4 animate-spin" />
      {label ?? t("loading")}
    </div>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const { t } = useTranslation("common");
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-16 text-center">
      <AlertTriangle className="h-6 w-6 text-destructive" />
      <p role="alert" className="text-sm text-muted-foreground">{humanizeError(error)}</p>
      {onRetry && (
        <button
          onClick={onRetry}
          className="text-sm font-medium text-primary hover:underline"
        >
          {t("try_again")}
        </button>
      )}
    </div>
  );
}

// RefreshError sits above a page whose last read still shows while a later one
// failed: the page stays usable, and says the status may be out of date.
export function RefreshError({ error, className }: { error: unknown; className?: string }) {
  const { t } = useTranslation("common");
  return (
    <div
      role="alert"
      className={cn(
        "flex items-center gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive",
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
    <div className="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-border py-16 text-center">
      <Inbox className="h-7 w-7 text-muted-foreground" />
      <div>
        <p className="font-medium">{title}</p>
        {hint && <p className="mt-1 text-sm text-muted-foreground">{hint}</p>}
      </div>
      {children}
    </div>
  );
}

export function NotAuthorized() {
  const { t } = useTranslation("common");
  return (
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-24 text-center">
      <ShieldX className="h-8 w-8 text-destructive" />
      <div>
        <p className="font-medium">{t("not_authorized_title")}</p>
        <p className="mt-1 text-sm text-muted-foreground">
          {t("not_authorized_body")}
        </p>
      </div>
      <Link to="/" className="text-sm font-medium text-primary hover:underline">
        {t("back_to_dashboard")}
      </Link>
    </div>
  );
}

// AccessUnknown stands in for an admin or owner page while /me has failed with
// anything but a 401: whether this account may see the page is unknown, so it
// says the check failed and why, and retries in place (the app stays mounted).
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
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-24 text-center">
      <ShieldQuestion className="h-8 w-8 text-amber-500" />
      <div>
        <p className="font-medium">{t("access_unknown_title")}</p>
        <p className="mt-1 text-sm text-muted-foreground">{t("access_unknown_body")}</p>
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
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-24 text-center">
      <SearchX className="h-8 w-8 text-muted-foreground" />
      <div>
        <p className="font-medium">{title ?? t("not_found_title")}</p>
        <p className="mt-1 text-sm text-muted-foreground">{body ?? t("not_found_body")}</p>
      </div>
      <Link to={linkTo} className="text-sm font-medium text-primary hover:underline">
        {linkLabel ?? t("back_to_dashboard")}
      </Link>
    </div>
  );
}

/**
 * PendingBackend is an HONEST placeholder for a surface whose backend read does
 * not exist yet (DESIGN-WEB-3SIDES non-goal #3: "a page with no real backend
 * source is an honest placeholder, never a mock chart presented as live"). It
 * names the missing endpoint so the gap is visible, not papered over with fake
 * data.
 */
export function PendingBackend({ endpoint, note }: { endpoint: string; note: string }) {
  const { t } = useTranslation("common");
  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-border py-16 text-center">
      <Construction className="h-7 w-7 text-muted-foreground" />
      <div>
        <p className="font-medium">{t("not_wired_yet")}</p>
        <p className="mt-1 max-w-md text-sm text-muted-foreground">{note}</p>
        <p className="mt-2 font-mono text-xs text-muted-foreground/80">
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
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-24 text-center">
      <ShieldX className="h-8 w-8 text-destructive" />
      <div>
        <p className="font-medium">{title}</p>
        <p className="mt-1 text-sm text-muted-foreground">{body}</p>
      </div>
      <Link to={linkTo} className="text-sm font-medium text-primary hover:underline">
        {linkLabel ?? t("my_servers_breadcrumb")}
      </Link>
    </div>
  );
}

export interface NotRunningProps {
  /** What the page says about a server that is down, and why it needs it up. */
  title: string;
  body: string;
  /** The server to wake from here. */
  serverName: string;
  phase: Phase;
  desiredState?: "Running" | "Stopped";
  /** From startFailure: a Failed start gets its own copy and a retry. */
  failure?: StartFailure | null;
  autoRestarts?: number;
  /** A pending retirement: the page offers no wake, only why. */
  retiring?: RetireState;
  /** Called once the wake was accepted, so the page refetches its status. */
  onWoken: () => void;
}

// NotRunning stands in for a page that needs the server up. It follows where the
// server is heading, so a wake from here reads as starting until the page's poll
// finds it running and switches over, a server going down says so, and a failed
// start offers the retry.
export function NotRunning({
  title,
  body,
  serverName,
  phase,
  desiredState,
  failure = null,
  autoRestarts = 0,
  retiring,
  onWoken,
}: NotRunningProps) {
  const { t } = useTranslation("servers");
  const shown = shownPhase({ phase, desiredState });
  const transitional = failure === null && (shown === "Starting" || shown === "Stopping");

  let icon = <Moon className="h-8 w-8 text-muted-foreground/70" />;
  let copy = { title, body };
  if (failure !== null) {
    icon = <AlertTriangle className="h-8 w-8 text-destructive" />;
    copy =
      failure === "retrying"
        ? {
            title: t("server_failed_retrying_title"),
            body: t("server_failed_retrying_body", { used: autoRestarts, max: MAX_AUTO_RESTARTS }),
          }
        : { title: t("server_failed_title"), body: t("server_failed_body") };
  } else if (transitional) {
    icon = <Loader2 className="h-8 w-8 animate-spin text-muted-foreground/70" />;
    copy =
      shown === "Starting"
        ? { title: t("server_waking_title"), body: t("server_waking_body") }
        : { title: t("server_stopping_title"), body: t("server_stopping_body") };
  }

  return (
    <div
      role="status"
      aria-live="polite"
      className="flex flex-col items-center justify-center gap-4 rounded-lg border border-dashed border-border bg-muted/20 py-16 text-center"
    >
      {icon}
      <div>
        <p className="font-medium">{copy.title}</p>
        <p className="mx-auto mt-1 max-w-sm text-sm text-muted-foreground">{copy.body}</p>
      </div>
      {!transitional && (
        <PowerButton
          name={serverName}
          phase={phase}
          desiredState={desiredState}
          failed={failure !== null}
          retiring={retiring}
          onChanged={onWoken}
          className="items-center"
        />
      )}
    </div>
  );
}
