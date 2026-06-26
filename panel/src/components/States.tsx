import { Loader2, AlertTriangle, Inbox, ShieldX, Construction } from "lucide-react";
import { Link } from "react-router-dom";
import { humanizeError } from "@/lib/api";

export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 py-16 text-sm text-muted-foreground">
      <Loader2 className="h-4 w-4 animate-spin" />
      {label}
    </div>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-16 text-center">
      <AlertTriangle className="h-6 w-6 text-destructive" />
      <p className="text-sm text-muted-foreground">{humanizeError(error)}</p>
      {onRetry && (
        <button
          onClick={onRetry}
          className="text-sm font-medium text-primary hover:underline"
        >
          Try again
        </button>
      )}
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
  return (
    <div className="mx-auto flex max-w-md flex-col items-center justify-center gap-3 py-24 text-center">
      <ShieldX className="h-8 w-8 text-destructive" />
      <div>
        <p className="font-medium">Not authorized</p>
        <p className="mt-1 text-sm text-muted-foreground">
          This area is for platform administrators. If you reached it by typing the
          URL, your account does not have admin access — and the API would refuse
          the request anyway.
        </p>
      </div>
      <Link to="/" className="text-sm font-medium text-primary hover:underline">
        Back to your dashboard
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
  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-border py-16 text-center">
      <Construction className="h-7 w-7 text-muted-foreground" />
      <div>
        <p className="font-medium">Not wired yet</p>
        <p className="mt-1 max-w-md text-sm text-muted-foreground">{note}</p>
        <p className="mt-2 font-mono text-xs text-muted-foreground/80">
          pending backend: {endpoint}
        </p>
      </div>
    </div>
  );
}
