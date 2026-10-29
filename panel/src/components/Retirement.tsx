import { useState } from "react";
import { Hourglass, Loader2 } from "lucide-react";
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
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { cn } from "@/lib/utils";

// Retirement holds the server stopped; only administrators can cancel deletion.

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

type Mode = "release" | "delete" | "stop" | "kill";

// Require the typed server name before submitting retirement.
/** DangerZone lists the actions that stop or remove a server, each with what it costs and its own confirmation. */
export function DangerZone({
  name,
  label,
  isAdmin,
  isOwner = false,
  canRetire = true,
  canKill = false,
  onChanged,
}: {
  name: string;
  /** What the dialog calls the server: its display name, else its name. */
  label: string;
  isAdmin: boolean;
  isOwner?: boolean;
  canRetire?: boolean;
  canKill?: boolean;
  onChanged: () => void;
}) {
  const { t } = useTranslation("servers");
  const [mode, setMode] = useState<Mode | null>(null);
  const rows: { mode: Mode; title: string; body: string; destructive?: boolean }[] = [
    ...(canKill ? [{ mode: "kill" as const, title: t("kill"), body: t("kill_desc"), destructive: true }] : []),
    ...(isOwner ? [{ mode: "stop" as const, title: t("emergency_stop"), body: t("emergency_stop_desc") }] : []),
    ...(canRetire ? [{ mode: "release" as const, title: t("retire_release"), body: t("retire_card_desc") }] : []),
    ...(canRetire && isAdmin ? [{ mode: "delete" as const, title: t("retire_delete"), body: t("retire_delete_desc"), destructive: true }] : []),
  ];
  if (rows.length === 0) return null;

  return (
    <section aria-labelledby="server-danger-zone" className="overflow-hidden rounded-xl border border-destructive/40 bg-card">
      <h2 id="server-danger-zone" className="border-b border-destructive/20 px-5 py-3 text-sm font-semibold text-destructive">{t("settings_danger_zone")}</h2>
      <div className="divide-y divide-border">
        {rows.map((row) => (
          <div key={row.mode} className="flex flex-wrap items-center justify-between gap-x-6 gap-y-3 px-5 py-4">
            <div className="min-w-0 max-w-2xl space-y-1">
              <p className="text-sm font-medium">{row.title}</p>
              <p className="text-xs leading-relaxed text-muted-foreground">{row.body}</p>
            </div>
            <Button size="sm" variant={row.destructive ? "destructive" : "outline"} className={cn(!row.destructive && "border-destructive/40 text-destructive hover:bg-destructive/10 hover:text-destructive")} onClick={() => setMode(row.mode)}>
              {row.title}
            </Button>
          </div>
        ))}
      </div>
      {mode === "kill" ? (
        <KillDialog name={name} label={label} onClose={() => setMode(null)} onDone={() => { setMode(null); onChanged(); }} />
      ) : mode && (
        <RetireDialog name={name} label={label} mode={mode} isAdmin={isAdmin} onClose={() => setMode(null)} onDone={onChanged} />
      )}
    </section>
  );
}

function KillDialog({ name, label, onClose, onDone }: { name: string; label: string; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation("servers");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function confirm() {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.kill(name);
      onDone();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("kill_title", { name: label })}</DialogTitle>
          <DialogDescription>{t("kill_warning")}</DialogDescription>
        </DialogHeader>
        {error && <MessageLine kind="error" message={error} compact />}
        <ConfirmFooter onCancel={onClose} onConfirm={() => void confirm()} loading={busy} cancelLabel={t("common:cancel")} confirmLabel={t("kill")} />
      </DialogContent>
    </Dialog>
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
  mode: Exclude<Mode, "kill">;
  isAdmin: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useTranslation("servers");
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const del = mode === "delete";
  const stopping = mode === "stop";
  const reauth = useReauth();
  const [accepted, setAccepted] = useState(false);

  function setOpen(open: boolean) {
    if (!open && !busy) onClose();
  }

  async function confirm() {
    if (typed !== name || busy || accepted) return;
    setBusy(true);
    setError(null);
    try {
      if (stopping) {
        await reauth.guard(() => api.emergencyStop(name, typed));
        setAccepted(true);
        setBusy(false);
        onDone();
        return;
      }
      await api.retireServer(name, { confirm: typed, delete: del });
      setBusy(false);
      onClose();
      onDone();
    } catch (e) {
      if (!isReauthCancelled(e)) setError(humanizeError(e));
      setBusy(false);
    }
  }

  return (
    <>
    <Dialog open onOpenChange={setOpen}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{stopping ? t("emergency_stop") : del ? t("retire_delete_title", { name: label }) : t("retire_release_title", { name: label })}</DialogTitle>
          {stopping ? <DialogDescription>{t("emergency_stop_warning")}</DialogDescription> : <DialogDescription asChild>
            <ul className="list-disc space-y-1 pl-5 text-left">
              <li>{t("retire_step_stop")}</li>
              <li>{del ? t("retire_step_delete") : t("retire_step_release")}</li>
              <li>{isAdmin ? t("retire_step_cancel_admin") : t("retire_step_cancel_owner")}</li>
            </ul>
          </DialogDescription>}
        </DialogHeader>
        {accepted ? <MessageLine kind="success" message={t("emergency_stop_accepted")} /> : <div className="grid gap-2">
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
        </div>}
        {error && <MessageLine kind="error" message={error} compact />}
        {stopping && error && <p className="text-sm text-muted-foreground">{t("emergency_stop_unknown")}</p>}
        <ConfirmFooter
          onCancel={() => setOpen(false)}
          onConfirm={() => void confirm()}
          disabled={typed !== name || accepted}
          loading={busy}
          cancelLabel={accepted ? t("common:close_sr") : t("access_cancel")}
          confirmLabel={stopping ? t("emergency_stop") : del ? t("retire_confirm_delete") : t("retire_confirm_release")}
        />
      </DialogContent>
    </Dialog>
    {reauth.dialog}
    </>
  );
}
