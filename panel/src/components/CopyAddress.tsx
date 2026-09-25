import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";
import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";

const FEEDBACK_MS = 2000;

// CopyAddress shows the address a Minecraft client joins and copies it with one
// tap. Minecraft is not a web page, so the address is never a link. When the
// clipboard is refused (an insecure origin, an embedded browser) the text is
// selected instead, ready for the system copy menu.
export function CopyAddress({ address, className }: { address: string; className?: string }) {
  const { t } = useTranslation("common");
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const textRef = useRef<HTMLElement>(null);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  function selectText() {
    const node = textRef.current;
    const selection = window.getSelection();
    if (!node || !selection) return;
    const range = document.createRange();
    range.selectNodeContents(node);
    selection.removeAllRanges();
    selection.addRange(range);
  }

  async function copy() {
    window.clearTimeout(timer.current);
    try {
      // Without a clipboard (an insecure origin) this throws, and lands below too.
      await navigator.clipboard.writeText(address);
      setState("copied");
    } catch {
      selectText();
      setState("failed");
    }
    timer.current = window.setTimeout(() => setState("idle"), FEEDBACK_MS);
  }

  const feedback =
    state === "copied" ? t("address_copied") : state === "failed" ? t("address_copy_failed") : "";

  // After a refused copy the whole address shows, selected, with the hint on its
  // own line; truncated, the selection would hide what the menu is about to copy.
  const failed = state === "failed";
  return (
    <span
      className={cn(
        "inline-flex min-w-0 max-w-full flex-wrap items-center gap-x-1 text-xs",
        className,
      )}
    >
      <code
        ref={textRef}
        className={cn(
          "min-w-0 select-all font-mono text-muted-foreground",
          failed ? "break-all" : "truncate",
        )}
        title={address}
      >
        {address}
      </code>
      <button
        type="button"
        onClick={() => void copy()}
        aria-label={t("copy_address", { address })}
        title={t("copy_address", { address })}
        className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {state === "copied" ? (
          <Check className="h-3.5 w-3.5 text-emerald-500" />
        ) : (
          <Copy className="h-3.5 w-3.5" />
        )}
      </button>
      <span
        role="status"
        className={cn(
          "shrink-0",
          failed ? "basis-full" : "whitespace-nowrap",
          state === "copied" ? "text-emerald-600 dark:text-emerald-400" : "text-muted-foreground",
        )}
      >
        {feedback}
      </span>
    </span>
  );
}
