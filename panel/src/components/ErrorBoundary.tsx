import { Component, useState, type ErrorInfo, type ReactNode } from "react";
import { AlertTriangle, Check, Copy, Home, RefreshCw, Sparkles } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { isChunkLoadError, reloadForNewDeploy } from "@/lib/chunk";
import { cn } from "@/lib/utils";

// ErrorBoundary keeps one broken render from unmounting the whole panel. The
// root boundary (main.tsx) covers the providers and the shell; the one around
// AppShell's Outlet keeps the navigation alive and resets on route change, so a
// crash on one page never strands the user there.

interface Props {
  children: ReactNode;
  /** Changing it clears a caught error (the Outlet passes the pathname). */
  resetKey?: unknown;
  /** Replaces the crash page, e.g. a flat fleet grid for the 3D card. */
  fallback?: (error: unknown, reset: () => void) => ReactNode;
  /** "screen" fills the viewport (root); "page" sits inside the shell. */
  variant?: "screen" | "page";
}

interface State {
  failed: boolean;
  error: unknown;
}

export class ErrorBoundary extends Component<Props, State> {
  state: State = { failed: false, error: null };

  static getDerivedStateFromError(error: unknown): State {
    return { failed: true, error };
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    console.error("felis panel: render failed", error, info.componentStack);
    if (isChunkLoadError(error)) reloadForNewDeploy();
  }

  componentDidUpdate(prev: Props) {
    if (this.state.failed && !Object.is(prev.resetKey, this.props.resetKey)) {
      this.setState({ failed: false, error: null });
    }
  }

  reset = () => this.setState({ failed: false, error: null });

  render() {
    if (!this.state.failed) return this.props.children;
    if (this.props.fallback) return this.props.fallback(this.state.error, this.reset);
    return <CrashPage error={this.state.error} onRetry={this.reset} screen={this.props.variant === "screen"} />;
  }
}

function errorText(error: unknown): string {
  if (error instanceof Error) return `${error.name}: ${error.message}`;
  try {
    return typeof error === "string" ? error : JSON.stringify(error);
  } catch {
    return String(error);
  }
}

function CrashPage({ error, onRetry, screen }: { error: unknown; onRetry: () => void; screen: boolean }) {
  const { t } = useTranslation("common");
  const [copied, setCopied] = useState(false);
  const updated = isChunkLoadError(error);
  const detail = errorText(error);

  async function copy() {
    try {
      await navigator.clipboard.writeText(`${detail}\n${window.location.href}`);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard denied: the text stays selectable in the details block.
    }
  }

  return (
    <div
      role="alert"
      className={cn(
        "flex items-center justify-center px-4",
        screen ? "min-h-screen bg-background text-foreground" : "flex-1 py-16",
      )}
    >
      <div className="w-full max-w-md space-y-5 text-center">
        <div
          className={cn(
            "mx-auto flex h-12 w-12 items-center justify-center rounded-full",
            updated ? "bg-primary/10 text-primary" : "bg-destructive/10 text-destructive",
          )}
        >
          {updated ? <Sparkles className="h-6 w-6" /> : <AlertTriangle className="h-6 w-6" />}
        </div>
        <div className="space-y-2">
          <h1 className="text-lg font-semibold">{updated ? t("crash_updated_title") : t("crash_title")}</h1>
          <p className="text-sm leading-relaxed text-muted-foreground">
            {updated ? t("crash_updated_body") : t("crash_body")}
          </p>
        </div>
        <div className="flex flex-wrap items-center justify-center gap-2">
          <Button onClick={() => window.location.reload()} className="gap-1.5">
            <RefreshCw className="h-4 w-4" />
            {t("crash_reload")}
          </Button>
          {!updated && (
            <Button variant="outline" onClick={onRetry}>
              {t("try_again")}
            </Button>
          )}
          {/* A plain anchor: the root boundary renders outside the router. */}
          <Button variant="ghost" asChild className="gap-1.5">
            <a href="/">
              <Home className="h-4 w-4" />
              {t("back_to_dashboard")}
            </a>
          </Button>
        </div>
        {!updated && (
          <details className="group rounded-md border border-border/60 bg-muted/30 text-left text-xs">
            <summary className="cursor-pointer select-none px-3 py-2 text-muted-foreground hover:text-foreground">
              {t("crash_details")}
            </summary>
            <div className="flex items-start gap-2 border-t border-border/60 px-3 py-2">
              <code className="min-w-0 flex-1 whitespace-pre-wrap break-all font-mono text-[11px] text-foreground">
                {detail}
              </code>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="h-7 w-7 shrink-0"
                onClick={copy}
                aria-label={t("crash_copy")}
                title={t("crash_copy")}
              >
                {copied ? <Check className="h-3.5 w-3.5 text-emerald-500" /> : <Copy className="h-3.5 w-3.5" />}
              </Button>
            </div>
          </details>
        )}
      </div>
    </div>
  );
}
