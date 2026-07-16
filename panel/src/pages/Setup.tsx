import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Loader2, Mail, Fingerprint } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AuthLayout } from "@/components/AuthLayout";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError, type SetupState } from "@/lib/api";
import { base64urlToBytes, bytesToBase64url } from "@/lib/utils";
import { useTier } from "@/lib/tier";

// Setup is the Owner's first-run onboarding wizard (spec §B setup bootstrap). The
// `felis setup` MC-bind flow prints https://op.console.<root>/setup?token=<raw> —
// the Owner is staff, so onboarding lands on the operator console, not the player
// panel; this page redeems that one-time token (minting a lockdown session), then drives the
// two remaining steps — verify email, enroll a passkey — before handing off to the
// dashboard. It sits OUTSIDE RequireAuth (like /login): the visitor arrives without
// a session, and the redeem is what creates one.
//
// Reload-safe: the token is single-use, so a refresh mid-wizard re-reads progress
// from /auth/setup/status (the surviving session) rather than dead-ending on a
// spent token. The two step endpoints and /me are all SetupAllowed, so the lockdown
// session can complete the wizard; the backend lifts the lockdown once email is
// verified, and we hand off to / once nothing remains.
export function Setup() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const { refresh } = useTier();
  const { t } = useTranslation("auth");

  const [state, setState] = useState<SetupState | null>(null);
  const [booting, setBooting] = useState(true);
  const [fatal, setFatal] = useState<string | null>(null);
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
        if (alive) setFatal(humanizeError(e));
      } finally {
        if (alive) setBooting(false);
      }
    })();
    return () => {
      alive = false;
    };
  }, [params]);

  // reload re-reads progress after a wizard step so the view advances to the next.
  const reload = useCallback(async () => {
    setState(await api.setupStatus());
  }, []);

  // finish re-reads /me (so RequireAuth sees the authenticated session) and hands
  // off to the dashboard. Idempotent — a completion effect and the skip button can
  // both reach here.
  const finish = useCallback(async () => {
    if (finishing.current) return;
    finishing.current = true;
    await refresh();
    navigate("/", { replace: true });
  }, [refresh, navigate]);

  // Once nothing remains (email verified AND a passkey exists, or the owner skipped
  // to a backend-valid state), hand off.
  useEffect(() => {
    if (state && !state.setup_required) void finish();
  }, [state, finish]);

  if (booting) {
    return (
      <AuthLayout title="Felis">
        <div className="flex items-center justify-center gap-2 py-8 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          {t("setup_preparing")}
        </div>
      </AuthLayout>
    );
  }

  if (fatal) {
    return (
      <AuthLayout title={t("setup_invalid_title")} subtitle={t("setup_invalid_subtitle")}>
        <Card>
          <CardContent className="space-y-4 pt-6 text-sm">
            <p className="text-muted-foreground">{fatal}</p>
            <p className="text-muted-foreground">
              {t("setup_invalid_hint_prefix")}
              <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
                felis setup
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
        <CardContent className="pt-6">
          {!state.email_verified ? (
            <EmailStep initialEmail={state.email} onVerified={reload} />
          ) : !state.has_passkey ? (
            <PasskeyStep onEnrolled={reload} onSkip={() => void finish()} />
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

/** EmailStep is the §B email-OTP step: send a code, then verify it. On success it
 *  calls onVerified (a status re-read) so the wizard advances to the passkey step.
 *  Mirrors the Account page's email card against the same SetupAllowed endpoints. */
function EmailStep({
  initialEmail,
  onVerified,
}: {
  initialEmail: string | null;
  onVerified: () => Promise<void>;
}) {
  const { t } = useTranslation("auth");
  const [email, setEmail] = useState(initialEmail ?? "");
  const [otp, setOtp] = useState("");
  const [sent, setSent] = useState(false);
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
      setSent(true);
    } catch (err) {
      setError(humanizeError(err));
    } finally {
      setBusy(false);
    }
  }

  async function verify(e: FormEvent) {
    e.preventDefault();
    const code = otp.trim();
    if (!code || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.emailVerify(code);
      await onVerified(); // advances (unmounts this step) — no need to clear busy
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
      {!sent ? (
        <form onSubmit={send} className="space-y-3">
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
          {error && <p className="text-sm text-destructive">{error}</p>}
          <Button type="submit" className="w-full" disabled={busy || !email.trim()}>
            {busy ? (
              <>
                <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                {t("sending_otp")}
              </>
            ) : (
              t("send_otp")
            )}
          </Button>
        </form>
      ) : (
        <form onSubmit={verify} className="space-y-3">
          <p className="text-xs text-emerald-600 dark:text-emerald-400">
            {t("setup_otp_sent", { email: email.trim() })}
          </p>
          <div className="space-y-2">
            <Label htmlFor="setup-otp">{t("otp_code")}</Label>
            <Input
              id="setup-otp"
              value={otp}
              onChange={(e) => setOtp(e.target.value)}
              inputMode="numeric"
              autoComplete="one-time-code"
              maxLength={6}
              className="font-mono text-center tracking-[0.3em]"
              disabled={busy}
              autoFocus
              aria-invalid={error ? true : undefined}
            />
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <Button type="submit" className="w-full" disabled={busy || otp.trim().length !== 6}>
            {busy ? (
              <>
                <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                {t("binding")}
              </>
            ) : (
              t("setup_verify_continue")
            )}
          </Button>
          <button
            type="button"
            onClick={() => {
              setSent(false);
              setOtp("");
              setError(null);
            }}
            disabled={busy}
            className="w-full text-center text-xs text-muted-foreground hover:text-primary disabled:opacity-50"
          >
            {t("setup_change_email")}
          </button>
        </form>
      )}
    </div>
  );
}

/** PasskeyStep enrolls the Owner's first passkey against the SetupAllowed register
 *  endpoints — the same ceremony as the Account page. Email is already verified at
 *  this point (backend lockdown lifted), so "skip" is a safe escape if the
 *  authenticator misbehaves: the owner lands in the console and can enroll later. */
function PasskeyStep({
  onEnrolled,
  onSkip,
}: {
  onEnrolled: () => Promise<void>;
  onSkip: () => void;
}) {
  const { t } = useTranslation("auth");
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
        throw new Error("Failed to create credential");
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
      setError(humanizeError(err));
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-sm font-medium text-foreground">
        <Fingerprint className="h-4 w-4 text-primary" /> {t("setup_passkey_step")}
      </div>
      <p className="text-sm text-muted-foreground">{t("setup_passkey_desc")}</p>
      {error && <p className="text-sm text-destructive">{error}</p>}
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
      <button
        type="button"
        onClick={onSkip}
        disabled={busy}
        className="w-full text-center text-xs text-muted-foreground hover:text-primary disabled:opacity-50"
      >
        {t("setup_skip")}
      </button>
    </div>
  );
}
