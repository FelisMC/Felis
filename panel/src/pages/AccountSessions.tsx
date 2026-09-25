import { useState } from "react";
import { Loader2, LogOut, Monitor, MonitorSmartphone, Smartphone } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Loading, ErrorState } from "@/components/States";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { deviceLabel, guessDevice } from "@/lib/device";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import type { SessionView } from "@/lib/types";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

// The server records activity at most once a minute, so a device seen within
// two minutes is as live as the one reading this page.
const ACTIVE_NOW_MS = 2 * 60_000;

interface Props {
  /** Bumped by the page after a change that signs other devices out on the
   *  server (removing a passkey), so the list reloads. */
  version: number;
  /** Operator accounts: their sessions idle out, which the card says. */
  staff: boolean;
  onSignOut: () => void;
  signingOut: boolean;
}

/** AccountSessionsCard lists every browser signed in to the caller's account
 *  and signs any of them out. This device can only leave through the ordinary
 *  sign-out, which also drops the cookie and returns to the login page. */
export function AccountSessionsCard({ version, staff, onSignOut, signingOut }: Props) {
  const { t, i18n } = useTranslation("account");
  const sessions = useAsync(() => api.listMySessions(), [version]);
  const [revoking, setRevoking] = useState<string | null>(null);
  const [confirmOthers, setConfirmOthers] = useState(false);
  const [revokingOthers, setRevokingOthers] = useState(false);
  const [message, setMessage] = useState<{ kind: "error" | "success"; text: string } | null>(null);

  const list = sessions.data ?? [];
  const others = list.filter((s) => !s.current);
  const now = Date.now();
  const locale = i18n.language;

  async function revoke(s: SessionView) {
    if (revoking) return;
    const device = deviceLabel(s.user_agent, t);
    setRevoking(s.token_hash);
    setMessage(null);
    try {
      await api.revokeMySession(s.token_hash);
      setMessage({ kind: "success", text: t("session_signed_out_device", { device }) });
    } catch (err) {
      // Already over (it expired, or another tab got there first) is what was asked.
      if ((err as { code?: string }).code === "session_not_found") {
        setMessage({ kind: "success", text: t("session_signed_out_device", { device }) });
      } else {
        setMessage({ kind: "error", text: humanizeError(err) });
      }
    } finally {
      setRevoking(null);
      sessions.reload();
    }
  }

  async function revokeOthers() {
    if (revokingOthers) return;
    setRevokingOthers(true);
    setMessage(null);
    try {
      const { revoked } = await api.revokeMyOtherSessions();
      setConfirmOthers(false);
      setMessage({ kind: "success", text: t("sessions_signed_out_others", { count: revoked }) });
    } catch (err) {
      setConfirmOthers(false);
      setMessage({ kind: "error", text: humanizeError(err) });
    } finally {
      setRevokingOthers(false);
      sessions.reload();
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <MonitorSmartphone className="h-4 w-4 text-primary" /> {t("sessions_title")}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        <div className="space-y-1 text-muted-foreground">
          <p>{t("sessions_desc")}</p>
          {staff && <p className="text-xs">{t("sessions_staff_idle")}</p>}
        </div>
        {message && <MessageLine kind={message.kind} message={message.text} />}
        {sessions.loading && !sessions.data ? (
          <Loading label={t("loading_sessions")} />
        ) : sessions.error && !sessions.data ? (
          <ErrorState error={sessions.error} onRetry={sessions.reload} />
        ) : list.length === 0 ? (
          <p className="py-2 text-xs italic text-muted-foreground">{t("sessions_empty")}</p>
        ) : (
          <ul className="divide-y rounded-md border bg-background/50">
            {list.map((s) => {
              const device = deviceLabel(s.user_agent, t);
              const Icon = guessDevice(s.user_agent).mobile ? Smartphone : Monitor;
              const seen = new Date(s.last_seen_at).getTime();
              const activeNow = s.current || now - seen < ACTIVE_NOW_MS;
              return (
                <li key={s.token_hash} className="flex items-center justify-between gap-3 p-3">
                  <div className="flex min-w-0 items-start gap-3">
                    <Icon className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
                    <div className="min-w-0 space-y-1">
                      <p className="flex flex-wrap items-center gap-2 font-medium text-foreground">
                        <span className="truncate" title={s.user_agent || undefined}>
                          {device}
                        </span>
                        {s.current && <Badge>{t("session_this_device")}</Badge>}
                      </p>
                      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                        <span
                          className={activeNow ? "text-emerald-500" : undefined}
                          title={formatAbsolute(s.last_seen_at, locale)}
                        >
                          {activeNow
                            ? t("session_active_now")
                            : t("session_active", { when: formatRelative(s.last_seen_at, now, locale) })}
                        </span>
                        {s.client_ip && <span className="font-mono">{s.client_ip}</span>}
                        <span title={formatAbsolute(s.created_at, locale)}>
                          {t("session_signed_in", { when: formatRelative(s.created_at, now, locale) })}
                        </span>
                      </div>
                    </div>
                  </div>
                  {!s.current && (
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => void revoke(s)}
                      disabled={revoking !== null || revokingOthers}
                      aria-label={t("session_sign_out_aria", { device })}
                      className="shrink-0 text-muted-foreground hover:text-destructive"
                    >
                      {revoking === s.token_hash ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                      ) : (
                        <LogOut className="h-4 w-4" />
                      )}
                      <span className="hidden sm:inline">{t("sign_out")}</span>
                    </Button>
                  )}
                </li>
              );
            })}
          </ul>
        )}
        <div className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              setMessage(null);
              setConfirmOthers(true);
            }}
            disabled={others.length === 0 || revoking !== null}
          >
            <MonitorSmartphone className="mr-2 h-4 w-4" />
            {t("sessions_sign_out_others")}
          </Button>
          <Button variant="outline" size="sm" onClick={onSignOut} disabled={signingOut}>
            <LogOut className="mr-2 h-4 w-4" />
            {signingOut ? t("signing_out") : t("sign_out")}
          </Button>
        </div>
        <Dialog
          open={confirmOthers}
          onOpenChange={(open) => {
            if (!open && !revokingOthers) setConfirmOthers(false);
          }}
        >
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t("sessions_sign_out_others_title")}</DialogTitle>
              <DialogDescription>{t("sessions_sign_out_others_desc")}</DialogDescription>
            </DialogHeader>
            <ConfirmFooter
              onCancel={() => setConfirmOthers(false)}
              onConfirm={() => void revokeOthers()}
              loading={revokingOthers}
              cancelLabel={t("common:cancel")}
              confirmLabel={t("sessions_sign_out_others")}
            />
          </DialogContent>
        </Dialog>
      </CardContent>
    </Card>
  );
}
