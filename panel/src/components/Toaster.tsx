import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { AlertCircle, CheckCircle2, X } from "lucide-react";
import { cn } from "@/lib/utils";

type Kind = "success" | "error";

interface ToastApi {
  success(message: string): void;
  error(message: string): void;
}

interface Toast {
  id: number;
  kind: Kind;
  message: string;
}

const SUCCESS_MS = 5000;
const MAX_VISIBLE = 3;

const ICON = { success: CheckCircle2, error: AlertCircle };

const STYLE = {
  success: "border-emerald-500/20 bg-emerald-500/10 text-emerald-500",
  error: "border-destructive/20 bg-destructive/10 text-destructive",
};

const ToastContext = createContext<ToastApi>({ success: () => {}, error: () => {} });

export function useToast(): ToastApi {
  return useContext(ToastContext);
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const { t } = useTranslation("common");
  const [toasts, setToasts] = useState<Toast[]>([]);
  const nextId = useRef(0);
  const show = useCallback((kind: Kind, message: string) => {
    const id = ++nextId.current;
    setToasts((all) => [...all, { id, kind, message }].slice(-MAX_VISIBLE));
  }, []);
  const api = useMemo<ToastApi>(
    () => ({ success: (message) => show("success", message), error: (message) => show("error", message) }),
    [show],
  );
  const dismiss = useCallback((id: number) => setToasts((all) => all.filter((x) => x.id !== id)), []);
  return (
    <ToastContext.Provider value={api}>
      {children}
      <section
        aria-label={t("notifications")}
        className="pointer-events-none fixed bottom-4 right-4 left-4 z-[60] max-w-sm sm:left-auto"
      >
        {/* Both regions stay mounted so screen readers register them before the first notice. */}
        <div role="status" aria-live="polite" className="flex flex-col gap-2">
          {toasts.filter((x) => x.kind === "success").map((x) => (
            <ToastItem key={x.id} toast={x} onDismiss={dismiss} />
          ))}
        </div>
        <div role="alert" aria-live="assertive" className="flex flex-col gap-2">
          {toasts.filter((x) => x.kind === "error").map((x) => (
            <ToastItem key={x.id} toast={x} onDismiss={dismiss} />
          ))}
        </div>
      </section>
    </ToastContext.Provider>
  );
}

function ToastItem({ toast, onDismiss }: { toast: Toast; onDismiss: (id: number) => void }) {
  const { t } = useTranslation("common");
  useEffect(() => {
    if (toast.kind !== "success") return;
    const timer = setTimeout(() => onDismiss(toast.id), SUCCESS_MS);
    return () => clearTimeout(timer);
  }, [toast.id, toast.kind, onDismiss]);
  const Icon = ICON[toast.kind];
  // The outer card is opaque, so the tinted row does not let page content show through.
  return (
    <div className="pointer-events-auto animate-in fade-in-0 slide-in-from-bottom-2 rounded-md bg-card shadow-lg motion-reduce:animate-none">
      <div className={cn("flex items-start gap-2 rounded-md border p-3 text-sm", STYLE[toast.kind])}>
        <Icon className="mt-0.5 h-4 w-4 shrink-0" />
        <p className="min-w-0 flex-1 break-words">{toast.message}</p>
        <button
          type="button"
          aria-label={t("dismiss_notification")}
          onClick={() => onDismiss(toast.id)}
          className="shrink-0 rounded-sm opacity-70 transition-opacity hover:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
    </div>
  );
}
