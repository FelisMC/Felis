import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Loader2, Mail, Fingerprint } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AuthLayout } from "@/components/AuthLayout";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, clientError, humanizeError, type SetupState } from "@/lib/api";
import type { ApiError } from "@/lib/types";
import { base64urlToBytes, bytesToBase64url } from "@/lib/utils";
import { useTier } from "@/lib/tier";
import { useBranding } from "@/lib/branding";
import { useConfig } from "@/lib/hooks";
import { InlineError } from "@/components/MessageLine";

// Setup creates its own session, requires a passkey before SMTP exists, and retries only transient token failures.
function retryable(e: unknown): boolean {
  const status = (e as Partial<ApiError> | undefined)?.status;
  return status === 0 || status === 429 || (typeof status === "number" && status >= 500);
}

export function Setup() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const { refresh } = useTier();
  const { t } = useTranslation("auth");
  const { name } = useBranding();
  const setupCommand = useConfig()?.nano ? "felis nano setup" : "felis setup";

  const [state, setState] = useState<SetupState | null>(null);
  const [preferPasskey, setPreferPasskey] = useState(false);
  const [booting, setBooting] = useState(true);
  const [fatal, setFatal] = useState<{ message: string; retryable: boolean } | null>(null);
  const [attempt, setAttempt] = useState(0);
  const finishing = useRef(false);

  // Boot: redeem the URL token, or resume from the session if the token is already
  // spent (a reload). No token + no session → a dead link.
  useEffect(() => {
    let alive = true;
    (async () => {
      const token = params.get("token");
      try {
        let st: SetupState;
        if (token) {
          try {
            st = await api.setupRedeem(token);
          } catch (redeemErr) {
            // The token may already be consumed (a reload). If a session survived,
            // resume from status; otherwise surface the original redeem error.
            try {
              st = await api.setupStatus();
            } catch {
              throw redeemErr;
            }
          }
        } else {
          st = await api.setupStatus();
        }
        if (alive) setState(st);
      } catch (e) {
        if (alive) setFatal({ message: humanizeError(e), retryable: retryable(e) });
      } finally {
        if (alive) setBooting(false);
      }
    })();
    return () => {
      alive = false;
    };
  }, [params, attempt]);

  const retry = () => {
    setFatal(null);
    setBooting(true);
    setAttempt((n) => n + 1);
  };

  // reload re-reads progress after a wizard step so the view advances to the next.
  const reload = useCallback(async () => {
    setState(await api.setupStatus());
  }, []);

  // finish re-reads /me (so RequireAuth sees the authenticated session) and hands
  // off to the dashboard. Idempotent — guarded so the completion effect fires once.
  const finish = useCallback(async () => {
    if (finishing.current) return;
    finishing.current = true;
    await refresh();
    navigate("/", { replace: true });
  }, [refresh, navigate]);

  // Once nothing remains (email recorded AND a passkey enrolled), hand off.
  useEffect(() => {
    if (state && !state.setup_required) void finish();
  }, [state, finish]);

  if (booting) {
    return (
      <AuthLayout title={name}>
        <div className="flex items-center justify-center gap-2 py-8 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          {t("setup_preparing")}
        </div>
      </AuthLayout>
    );
  }

  if (fatal?.retryable) {
    return (
      <AuthLayout title={t("setup_failed_title")} subtitle={t("setup_failed_subtitle")}>
        <Card>
          <CardContent className="space-y-4 pt-6 text-sm">
            <p role="alert" className="text-muted-foreground">{fatal.message}</p>
            <Button className="w-full" onClick={retry}>
              {t("setup_retry")}
            </Button>
          </CardContent>
        </Card>
      </AuthLayout>
    );
  }

  if (fatal) {
    return (
      <AuthLayout title={t("setup_invalid_title")} subtitle={t("setup_invalid_subtitle")}>
        <Card>
          <CardContent className="space-y-4 pt-6 text-sm">
            <p role="alert" className="text-muted-foreground">{fatal.message}</p>
            <p className="text-muted-foreground">
              {t("setup_invalid_hint_prefix")}
              <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
                {setupCommand}
              </code>
              {t("setup_invalid_hint_suffix")}
            </p>
            <Button
              variant="outline"
              className="w-full"
              onClick={() => navigate("/login", { replace: true })}
            >
              {t("setup_goto_login")}
            </Button>
          </CardContent>
        </Card>
      </AuthLayout>
    );
  }

  // booting/fatal cover every other branch; state is set here.
  if (!state) return null;

  return (
    <AuthLayout title={t("setup_title")} subtitle={t("setup_welcome", { name: state.username })}>
      <Card>
        <CardContent className="space-y-4 pt-6">
          <p className="text-sm text-muted-foreground">{t("setup_game_optional")}</p>
          {state.mail_available && !state.email_verified && !preferPasskey ? (
            <EmailVerifyStep initialEmail={state.email} onVerified={reload} onUsePasskey={() => setPreferPasskey(true)} />
          ) : !state.email ? (
            <EmailStep initialEmail={state.email} onRecorded={reload} />
          ) : !state.has_passkey ? (
            <PasskeyStep onEnrolled={reload} onUseEmail={state.mail_available ? () => setPreferPasskey(false) : undefined} />
          ) : (
            <div className="flex items-center justify-center gap-2 py-6 text-sm text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" />
              {t("setup_entering")}
            </div>
          )}
        </CardContent>
      </Card>
    </AuthLayout>
  );
}

function EmailStep({
  initialEmail,
  onRecorded,
}: {
  initialEmail: string | null;
  onRecorded: () => Promise<void>;
}) {
  const { t } = useTranslation("auth");
  const [email, setEmail] = useState(initialEmail ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function save(e: FormEvent) {
    e.preventDefault();
    const addr = email.trim();
    if (!addr || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.setEmail(addr);
      await onRecorded(); // advances (unmounts this step) — no need to clear busy
    } catch (err) {
      setError(humanizeError(err));
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-sm font-medium text-foreground">
        <Mail className="h-4 w-4 text-primary" /> {t("setup_email_step")}
      </div>
      <p className="text-sm text-muted-foreground">{t("setup_email_desc")}</p>
      <form onSubmit={save} className="space-y-3">
        <div className="space-y-2">
          <Label htmlFor="setup-email">{t("email_address")}</Label>
          <Input
            id="setup-email"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            placeholder="you@example.com"
            disabled={busy}
            autoFocus
            aria-invalid={error ? true : undefined}
          />
        </div>
        <InlineError message={error} />
        <Button type="submit" className="w-full" disabled={busy || !email.trim()}>
          {busy ? (
            <>
              <Loader2 className="mr-2 h-4 w-4 animate-spin" />
              {t("saving")}
            </>
          ) : (
            t("setup_email_save")
          )}
        </Button>
      </form>
    </div>
  );
}

// With a mail relay, a verified email completes setup on its own, so a self-signed certificate cannot block it.
function EmailVerifyStep({
  initialEmail,
  onVerified,
  onUsePasskey,
}: {
  initialEmail: string | null;
  onVerified: () => Promise<void>;
  onUsePasskey: () => void;
}) {
  const { t } = useTranslation("auth");
  const [email, setEmail] = useState(initialEmail ?? "");
  const [sentTo, setSentTo] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function send(e: FormEvent) {
    e.preventDefault();
    const addr = email.trim();
    if (!addr || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.emailStart(addr);
      setSentTo(addr);
      setCode("");
    } catch (err) {
      setError(humanizeError(err));
    } finally {
      setBusy(false);
    }
  }

  async function verify(e: FormEvent) {
    e.preventDefault();
    const value = code.trim();
    if (!value || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.emailVerify(value);
      await onVerified(); // setup completes and the page moves on
    } catch (err) {
      setError(humanizeError(err));
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-sm font-medium text-foreground">
        <Mail className="h-4 w-4 text-primary" /> {t("setup_verify_step")}
      </div>
      <p className="text-sm text-muted-foreground">{t("setup_verify_desc")}</p>
      {sentTo === null ? (
        <form onSubmit={send} className="space-y-3">
          <div className="space-y-2">
            <Label htmlFor="setup-verify-email">{t("email_address")}</Label>
            <Input
              id="setup-verify-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="email"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="you@example.com"
              disabled={busy}
              autoFocus
              aria-invalid={error ? true : undefined}
            />
          </div>
          <InlineError message={error} />
          <Button type="submit" className="w-full" disabled={busy || !email.trim()}>
            {busy ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}
            {t("setup_send_code")}
          </Button>
        </form>
      ) : (
        <form onSubmit={verify} className="space-y-3">
          <p className="text-sm text-muted-foreground">{t("setup_code_sent", { email: sentTo })}</p>
          <div className="space-y-2">
            <Label htmlFor="setup-verify-code">{t("setup_code_label")}</Label>
            <Input
              id="setup-verify-code"
              inputMode="numeric"
              autoComplete="one-time-code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              disabled={busy}
              autoFocus
              aria-invalid={error ? true : undefined}
            />
          </div>
          <InlineError message={error} />
          <Button type="submit" className="w-full" disabled={busy || !code.trim()}>
            {busy ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}
            {t("setup_verify_submit")}
          </Button>
          <Button type="button" variant="ghost" className="w-full" disabled={busy} onClick={() => setSentTo(null)}>
            {t("setup_change_email")}
          </Button>
        </form>
      )}
      <Button type="button" variant="link" className="w-full" disabled={busy} onClick={onUsePasskey}>
        {t("setup_use_passkey")}
      </Button>
    </div>
  );
}

// The setup lock lifts after passkey enrollment; onUseEmail offers the email code instead when mail works.
function PasskeyStep({
  onEnrolled,
  onUseEmail,
}: {
  onEnrolled: () => Promise<void>;
  onUseEmail?: () => void;
}) {
  const { t } = useTranslation("auth");
  const nano = useConfig()?.nano;
  const refusal = onUseEmail ? "setup_passkey_refused_mail" : nano ? "setup_passkey_refused_nano" : "setup_passkey_refused";
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function enroll() {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const options = await api.passkeyRegisterBegin();
      const publicKey: PublicKeyCredentialCreationOptions = {
        ...options,
        challenge: base64urlToBytes(options.challenge),
        user: {
          ...options.user,
          id: base64urlToBytes(options.user.id),
        },
        excludeCredentials: options.excludeCredentials?.map((cred: any) => ({
          ...cred,
          id: base64urlToBytes(cred.id),
        })),
      };

      const credential = (await navigator.credentials.create({
        publicKey,
      })) as PublicKeyCredential;

      if (!credential) {
        throw clientError("passkey_no_credential");
      }

      const response = credential.response as AuthenticatorAttestationResponse;
      const attestation = {
        id: credential.id,
        rawId: bytesToBase64url(credential.rawId),
        type: credential.type,
        response: {
          clientDataJSON: bytesToBase64url(response.clientDataJSON),
          attestationObject: bytesToBase64url(response.attestationObject),
          transports:
            typeof response.getTransports === "function" ? response.getTransports() : [],
        },
      };

      await api.passkeyRegisterFinish(t("setup_default_passkey_name"), attestation);
      await onEnrolled();
    } catch (err: any) {
      // Chromium refuses passkeys on pages whose certificate it does not trust and reports it like a cancel.
      const refused = err?.name === "NotAllowedError" || err?.name === "SecurityError";
      setError(refused ? t(refusal) : humanizeError(err));
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-sm font-medium text-foreground">
        <Fingerprint className="h-4 w-4 text-primary" /> {t("setup_passkey_step")}
      </div>
      <p className="text-sm text-muted-foreground">{t(onUseEmail ? "setup_passkey_desc_optional" : "setup_passkey_desc")}</p>
      <InlineError message={error} />
      <Button className="w-full gap-2" onClick={enroll} disabled={busy}>
        {busy ? (
          <>
            <Loader2 className="h-4 w-4 animate-spin" />
            {t("setup_registering")}
          </>
        ) : (
          <>
            <Fingerprint className="h-4 w-4" />
            {t("setup_create_passkey")}
          </>
        )}
      </Button>
      {onUseEmail ? (
        <Button type="button" variant="link" className="w-full" disabled={busy} onClick={onUseEmail}>
          {t("setup_use_email")}
        </Button>
      ) : null}
    </div>
  );
}
