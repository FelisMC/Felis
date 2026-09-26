import { useState } from "react";
import { Hourglass, Loader2, PackageX } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { formatAbsolute, formatRelative } from "@/lib/format";
import type { RetireState } from "@/lib/types";
import { cn } from "@/lib/utils";

// A server leaves its owner, or the platform, through a retirement
// (internal/api/handlers_retire.go): the owner gives it up, or an admin deletes
// it. felis-api stops the server and records the request; the reaper archives the
// world and carries it out on its next daily run. Until then the server cannot be
// started or claimed, and the request can be cancelled: a give-up by its owner or
// an admin, a deletion by an admin only.

/** RetiringBadge stands in for the start/stop control of a server with a pending
 *  retirement: nothing can start it until the request is cancelled or done. */
export function RetiringBadge({ retiring, className }: { retiring: RetireState; className?: string }) {
  const { t } = useTranslation("servers");
  return (
    <span
      title={t("retiring_badge_hint")}
      className={cn(
        "inline-flex items-center gap-1 rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-xs font-medium text-amber-700 dark:text-amber-300",
        className,
      )}
    >
      <Hourglass className="h-3 w-3" />
      {retiring.delete ? t("retiring_badge_delete") : t("retiring_badge_release")}
    </span>
  );
}

/** RetireNotice heads the console of a server with a pending retirement: what
 *  happens at the next reclaim run, and the way back while there still is one. */
export function RetireNotice({
  name,
  retiring,
  isAdmin,
  onChanged,
}: {
  name: string;
  retiring: RetireState;
  isAdmin: boolean;
  onChanged: () => void;
}) {
  const { t, i18n } = useTranslation("servers");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const canCancel = isAdmin || !retiring.delete;
  const when = formatRelative(retiring.requested_at, Date.now(), i18n.language);

  async function cancel() {
    setBusy(true);
    setError(null);
    try {
      await api.cancelRetire(name);
      onChanged();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div
      role="status"
      className="flex shrink-0 flex-col gap-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-4 text-sm sm:flex-row sm:items-start"
    >
      <Hourglass className="mt-0.5 h-5 w-5 shrink-0 text-amber-600 dark:text-amber-400" />
      <div className="min-w-0 flex-1 space-y-1">
        <p className="font-medium text-foreground">
          {retiring.delete ? t("retiring_title_delete") : t("retiring_title_release")}
        </p>
        <p className="text-muted-foreground" title={formatAbsolute(retiring.requested_at, i18n.language)}>
          {retiring.delete ? t("retiring_body_delete", { when }) : t("retiring_body_release", { when })}
        </p>
        {!canCancel && <p className="text-xs text-muted-foreground">{t("retiring_cancel_admin_only")}</p>}
        {error && <MessageLine kind="error" message={error} compact />}
      </div>
      {canCancel && (
        <Button size="sm" variant="outline" className="shrink-0 self-start" onClick={() => void cancel()} disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          {t("retiring_cancel")}
        </Button>
      )}
    </div>
  );
}

type Mode = "release" | "delete";

/** RetireCard is the console sidebar's way to give a server up (its owner or an
 *  admin) or delete it (an admin). Either asks for the server's name typed out,
 *  the same confirmation felis-api requires, before anything is sent. */
export function RetireCard({
  name,
  label,
  isAdmin,
  onChanged,
}: {
  name: string;
  /** What the dialog calls the server: its display name, else its name. */
  label: string;
  isAdmin: boolean;
  onChanged: () => void;
}) {
  const { t } = useTranslation("servers");
  const [mode, setMode] = useState<Mode | null>(null);

  return (
    <div className="space-y-3 rounded-lg border border-destructive/30 bg-card p-4">
      <div className="flex items-start gap-3">
        <PackageX className="mt-0.5 h-5 w-5 shrink-0 text-destructive" />
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium">{isAdmin ? t("retire_card_title_admin") : t("retire_card_title")}</p>
          <p className="mt-0.5 text-xs text-muted-foreground">
            {isAdmin ? t("retire_card_desc_admin") : t("retire_card_desc")}
          </p>
        </div>
      </div>
      <div className="flex flex-wrap gap-2">
        <Button
          size="sm"
          variant="outline"
          className="border-destructive/30 text-destructive hover:bg-destructive/10 hover:text-destructive"
          onClick={() => setMode("release")}
        >
          {t("retire_release")}
        </Button>
        {isAdmin && (
          <Button size="sm" variant="destructive" onClick={() => setMode("delete")}>
            {t("retire_delete")}
          </Button>
        )}
      </div>
      {mode && (
        <RetireDialog
          name={name}
          label={label}
          mode={mode}
          isAdmin={isAdmin}
          onClose={() => setMode(null)}
          onDone={onChanged}
        />
      )}
    </div>
  );
}

function RetireDialog({
  name,
  label,
  mode,
  isAdmin,
  onClose,
  onDone,
}: {
  name: string;
  label: string;
  mode: Mode;
  isAdmin: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useTranslation("servers");
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const del = mode === "delete";

  function setOpen(open: boolean) {
    if (!open && !busy) onClose();
  }

  async function confirm() {
    if (typed !== name || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.retireServer(name, { confirm: typed, delete: del });
      setBusy(false);
      onClose();
      onDone();
    } catch (e) {
      setError(humanizeError(e));
      setBusy(false);
    }
  }

  return (
    <Dialog open onOpenChange={setOpen}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{del ? t("retire_delete_title", { name: label }) : t("retire_release_title", { name: label })}</DialogTitle>
          <DialogDescription asChild>
            <ul className="list-disc space-y-1 pl-5 text-left">
              <li>{t("retire_step_stop")}</li>
              <li>{del ? t("retire_step_delete") : t("retire_step_release")}</li>
              <li>{isAdmin ? t("retire_step_cancel_admin") : t("retire_step_cancel_owner")}</li>
            </ul>
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-2">
          <label htmlFor="retire-confirm" className="text-sm">
            {t("retire_confirm_label")} <code className="rounded bg-muted px-1 font-mono text-xs">{name}</code>
          </label>
          <Input
            id="retire-confirm"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void confirm();
            }}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
          />
        </div>
        {error && <MessageLine kind="error" message={error} compact />}
        <ConfirmFooter
          onCancel={() => setOpen(false)}
          onConfirm={() => void confirm()}
          disabled={typed !== name}
          loading={busy}
          cancelLabel={t("access_cancel")}
          confirmLabel={del ? t("retire_confirm_delete") : t("retire_confirm_release")}
        />
      </DialogContent>
    </Dialog>
  );
}
