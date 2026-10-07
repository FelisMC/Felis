import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Fingerprint, Loader2, LogIn, Mail, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { InlineError } from "@/components/MessageLine";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { api, clientError, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { requestAssertion } from "@/lib/passkey";
import { useTier } from "@/lib/tier";

// A change to how the account signs in (a passkey added or removed, the email
// changed) needs this session to have proven a factor in the last few minutes,
// so a browser left signed in cannot quietly swap the account's ways in. The API
// refuses such a change with 403 reauth_required; useReauth catches that, asks
// for the proof in this dialog and runs the change again once it is given.

export function isReauthRequired(e: unknown): boolean {
  return (e as { code?: string } | null)?.code === "reauth_required";
}

/** isReauthCancelled: the confirmation was closed without a proof. Nothing
 *  failed, so callers show no error for it. */
export function isReauthCancelled(e: unknown): boolean {
  return (e as { code?: string } | null)?.code === "reauth_cancelled";
}

export function useReauth() {
  const [open, setOpen] = useState(false);
  const settle = useRef<((ok: boolean) => void) | null>(null);

  const finish = useCallback((ok: boolean) => {
    setOpen(false);
    const resolve = settle.current;
    settle.current = null;
    resolve?.(ok);
  }, []);

  // confirm opens the dialog and resolves true once a factor is proven, false
  // when it is closed without one.
  const confirm = useCallback(() => {
    settle.current?.(false);
    setOpen(true);
    return new Promise<boolean>((resolve) => {
      settle.current = resolve;
    });
  }, []);

  // guard runs a change. Refused for want of a fresh proof, it asks for one and
  // runs the change a second time; closing the dialog rejects with
  // reauth_cancelled. A change that needs a user gesture of its own (a WebAuthn
  // create) uses confirm and lets the user press its button again instead.
  const guard = useCallback(
    async <T,>(change: () => Promise<T>): Promise<T> => {
      try {
        return await change();
      } catch (e) {
        if (!isReauthRequired(e)) throw e;
        if (!(await confirm())) throw clientError("reauth_cancelled");
        return change();
      }
    },
    [confirm],
  );

  const dialog = <ReauthDialog open={open} onDone={() => finish(true)} onCancel={() => finish(false)} />;
  return { guard, confirm, dialog };
}

function ReauthDialog({ open, onDone, onCancel }: { open: boolean; onDone: () => void; onCancel: () => void }) {
  const { t } = useTranslation("account");
  return (
    <Dialog open={open} onOpenChange={(next) => !next && onCancel()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <ShieldCheck className="h-5 w-5 text-primary" />
            {t("reauth_title")}
          </DialogTitle>
          <DialogDescription>{t("reauth_desc")}</DialogDescription>
        </DialogHeader>
        {/* Mounted per opening, so every confirmation starts from a fresh status. */}
        {open && <ReauthBody onDone={onDone} onCancel={onCancel} />}
      </DialogContent>
    </Dialog>
  );
}

type Pending = "passkey" | "send" | "verify" | "sign_in";

function ReauthBody({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const { t } = useTranslation("account");
  const { identity, refresh } = useTier();
  const status = useAsync(() => api.reauthStatus(), []);
  const [pending, setPending] = useState<Pending | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [codeSent, setCodeSent] = useState(false);
  const [code, setCode] = useState("");

  // Proven meanwhile (in another tab, say): there is nothing to ask.
  const proven = status.data?.needed === false;
  useEffect(() => {
    if (proven) onDone();
  }, [proven, onDone]);

  async function act(kind: Pending, fn: () => Promise<void>) {
    if (pending) return;
    setPending(kind);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setPending(null);
    }
  }

  const withPasskey = () =>
    act("passkey", async () => {
      const options = await api.reauthPasskeyBegin();
      await api.reauthPasskeyFinish(await requestAssertion(options.publicKey));
      onDone();
    });

  const sendCode = () =>
    act("send", async () => {
      await api.reauthEmailStart();
      setCodeSent(true);
      setCode("");
    });

  const verifyCode = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = code.trim();
    if (!trimmed) return;
    void act("verify", async () => {
      await api.reauthEmailVerify(trimmed);
      onDone();
    });
  };

  // A fresh sign-in counts as the proof. Signing out flips the session to
  // signed-out and the route guard takes the browser to the sign-in page.
  const signInAgain = () =>
    act("sign_in", async () => {
      await api.logout();
      await refresh();
    });

  if (status.loading && !status.data) {
    return (
      <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        {t("reauth_checking")}
      </div>
    );
  }
  if (status.error || !status.data || proven) {
    return (
      <>
        <InlineError message={status.error ? humanizeError(status.error) : null} />
        <DialogFooter>
          <Button variant="outline" size="sm" onClick={onCancel}>
            {t("common:cancel")}
          </Button>
          {status.error ? (
            <Button size="sm" onClick={status.reload}>
              {t("common:try_again")}
            </Button>
          ) : null}
        </DialogFooter>
      </>
    );
  }

  const factors = status.data.factors;
  const email = identity?.email ?? "";
  const options: React.ReactNode[] = [];

  if (factors.includes("passkey")) {
    options.push(
      <Button key="passkey" className="w-full" onClick={() => void withPasskey()} disabled={pending !== null}>
        {pending === "passkey" ? <Loader2 className="animate-spin" /> : <Fingerprint />}
        {pending === "passkey" ? t("reauth_passkey_waiting") : t("reauth_passkey_btn")}
      </Button>,
    );
  }
  if (factors.includes("email")) {
    options.push(
      codeSent ? (
        <form key="email" onSubmit={verifyCode} className="space-y-2">
          <Label htmlFor="reauth-code">{t("reauth_code_label", { email })}</Label>
          <div className="flex gap-2">
            <Input
              id="reauth-code"
              inputMode="numeric"
              autoComplete="one-time-code"
              placeholder={t("otp_code_placeholder")}
              value={code}
              onChange={(e) => setCode(e.target.value)}
              disabled={pending !== null}
              maxLength={6}
              autoFocus
              className="font-mono text-center tracking-[0.2em]"
            />
            <Button type="submit" disabled={pending !== null || code.trim().length !== 6} className="shrink-0">
              {pending === "verify" && <Loader2 className="animate-spin" />}
              {pending === "verify" ? t("reauth_confirming") : t("reauth_confirm_btn")}
            </Button>
          </div>
          <Button
            type="button"
            variant="link"
            size="sm"
            onClick={() => void sendCode()}
            disabled={pending !== null}
            className="h-auto p-0 font-normal"
          >
            {pending === "send" ? t("sending_code") : t("reauth_resend")}
          </Button>
        </form>
      ) : (
        <Button
          key="email"
          variant="outline"
          className="w-full"
          onClick={() => void sendCode()}
          disabled={pending !== null}
        >
          {pending === "send" ? <Loader2 className="animate-spin" /> : <Mail />}
          <span className="truncate">{pending === "send" ? t("sending_code") : t("reauth_email_btn", { email })}</span>
        </Button>
      ),
    );
  }
  if (factors.includes("sign_in")) {
    options.push(
      <div key="sign_in" className="space-y-2">
        <p className="text-sm text-muted-foreground">{t("reauth_sign_in_desc")}</p>
        <Button variant="outline" className="w-full" onClick={() => void signInAgain()} disabled={pending !== null}>
          {pending === "sign_in" ? <Loader2 className="animate-spin" /> : <LogIn />}
          {t("reauth_sign_in_btn")}
        </Button>
      </div>,
    );
  }

  return (
    <>
      <div className="space-y-3 py-2">
        {options.length === 0 ? (
          <InlineError message={t("errors:no_step_up_factor")} />
        ) : (
          options.flatMap((option, i) =>
            i === 0
              ? [option]
              : [
                  <div key={`or-${i}`} className="flex items-center gap-3 text-xs text-muted-foreground">
                    <span className="h-px flex-1 bg-border" />
                    {t("reauth_or")}
                    <span className="h-px flex-1 bg-border" />
                  </div>,
                  option,
                ],
          )
        )}
        <InlineError message={error} />
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={onCancel}>
          {t("common:cancel")}
        </Button>
      </DialogFooter>
    </>
  );
}
