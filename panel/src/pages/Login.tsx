import { useState, useEffect, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { Loader2, KeyRound, Mail, Fingerprint, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AuthLayout } from "@/components/AuthLayout";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useTier } from "@/lib/tier";
import { loginReturnPath } from "@/lib/auth";
import { api, clientError, humanizeError } from "@/lib/api";
import { loadConfig } from "@/lib/config";
import { base64urlToBytes, bytesToBase64url } from "@/lib/utils";
import { InlineError } from "@/components/MessageLine";

// Login is the passwordless sign-in (spec §B). Passkey and email-OTP are the
// primary doors; a first-time player arrives with an in-game Bind Code (/link);
// staff use the vouched op-login door (email code + in-game approval). No password
// exists anywhere in the product. On success the API sets an HttpOnly session
// cookie (invisible here); we then refresh the tier context so the gate
// re-evaluates and land on the dashboard.
//
// Reaching this page already-authenticated (e.g. typing /login while signed in)
// short-circuits to the dashboard rather than showing the form. RequireAuth sends
// a signed-out visitor here with ?next= (the page they were on) and, when their
// session ended under an open page, state.sessionEnded, which shows why; after
// signing in they land back on that page.
export function Login() {
  const { loading, identity, refresh } = useTier();
  const navigate = useNavigate();
  const { t } = useTranslation("auth");
  const [searchParams] = useSearchParams();
  const next = loginReturnPath(searchParams.get("next"));
  const sessionEnded = (useLocation().state as { sessionEnded?: boolean } | null)?.sessionEnded === true;

  const [activeTab, setActiveTab] = useState<"main" | "bind" | "op">("main");
  const [email, setEmail] = useState("");
  const [otpCode, setOtpCode] = useState("");
  const [otpSent, setOtpSent] = useState(false);
  const [countdown, setCountdown] = useState(0);
  const [bindCode, setBindCode] = useState("");
  // Op-login (staff door): start → wait for the in-game vouch → finish with the
  // mailed code. request_id doubles as the handle an online admin approves.
  const [opEmail, setOpEmail] = useState("");
  const [opRequestId, setOpRequestId] = useState<string | null>(null);
  const [opApproved, setOpApproved] = useState(false);
  const [opCode, setOpCode] = useState("");
  const [isOpHost, setIsOpHost] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Countdown timer for OTP resend
  useEffect(() => {
    if (countdown <= 0) return;
    const timer = setTimeout(() => {
      setCountdown(countdown - 1);
    }, 1000);
    return () => clearTimeout(timer);
  }, [countdown]);

  // Tier-aware copy: on the op.console hostname the staff door is the default tab
  // (the player doors refuse staff accounts anyway).
  useEffect(() => {
    void loadConfig().then((cfg) => {
      if (cfg.adminHostname && window.location.hostname === cfg.adminHostname) {
        setIsOpHost(true);
        setActiveTab("op");
      }
    });
  }, []);

  // Poll the op-login request until an in-game approval lands. Errors are
  // swallowed on purpose: a transient failure just means we ask again.
  useEffect(() => {
    if (!opRequestId || opApproved) return;
    const timer = setInterval(async () => {
      try {
        const s = await api.opLoginStatus(opRequestId);
        if (s.approved) setOpApproved(true);
      } catch {
        // keep polling
      }
    }, 3000);
    return () => clearInterval(timer);
  }, [opRequestId, opApproved]);

  // Don't flash the form while the boot /me is still in flight: a signed-in visitor
  // would briefly see a login form before being redirected away.
  if (loading) {
    return (
      <AuthLayout title={t("common:brand_name")}>
        <div className="flex items-center justify-center gap-2 py-8 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          {t("common:loading")}
        </div>
      </AuthLayout>
    );
  }
  if (identity) return <Navigate to={next} replace />;

  async function handleBindSubmit(e: FormEvent) {
    e.preventDefault();
    const code = bindCode.trim();
    if (!code || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await api.bind(code);
      await refresh();
      navigate(next, { replace: true });
    } catch (err) {
      setError(humanizeError(err));
      setSubmitting(false);
    }
  }

  async function handleSendOtp() {
    if (!email.trim() || submitting || countdown > 0) return;
    setSubmitting(true);
    setError(null);
    try {
      await api.authEmailStart(email.trim());
      setOtpSent(true);
      setCountdown(60);
    } catch (err) {
      setError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  async function handleEmailSubmit(e: FormEvent) {
    e.preventDefault();
    if (!email.trim() || !otpCode.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await api.authEmailVerify(email.trim(), otpCode.trim());
      await refresh();
      navigate(next, { replace: true });
    } catch (err) {
      setError(humanizeError(err));
      setSubmitting(false);
    }
  }

  async function handlePasskeyLoginClick() {
    if (submitting) return;
    setSubmitting(true);
    setError(null);

    const identifier = email.trim();
    try {
      let assertion: any;
      if (!identifier) {
        // Discoverable (Usernameless) passkey login
        const options = await api.authPasskeyDiscoverableBegin();
        const publicKey: PublicKeyCredentialRequestOptions = {
          ...options.publicKey,
          challenge: base64urlToBytes(options.publicKey.challenge),
          allowCredentials: options.publicKey.allowCredentials?.map((cred: any) => ({
            ...cred,
            id: base64urlToBytes(cred.id),
          })),
        };

        const credential = (await navigator.credentials.get({
          publicKey,
        })) as PublicKeyCredential;

        if (!credential) {
          throw clientError("passkey_no_credential");
        }

        const response = credential.response as AuthenticatorAssertionResponse;
        assertion = {
          id: credential.id,
          rawId: bytesToBase64url(credential.rawId),
          type: credential.type,
          response: {
            clientDataJSON: bytesToBase64url(response.clientDataJSON),
            authenticatorData: bytesToBase64url(response.authenticatorData),
            signature: bytesToBase64url(response.signature),
            userHandle: response.userHandle ? bytesToBase64url(response.userHandle) : null,
          },
        };

        await api.authPasskeyDiscoverableFinish(options.login_id, assertion);
      } else {
        // Email-first passkey login
        if (!identifier.includes("@")) {
          throw new Error(t("passkey_email_hint"));
        }

        const options = await api.authPasskeyLoginBegin(identifier);
        const publicKey: PublicKeyCredentialRequestOptions = {
          ...options,
          challenge: base64urlToBytes(options.challenge),
          allowCredentials: options.allowCredentials?.map((cred: any) => ({
            ...cred,
            id: base64urlToBytes(cred.id),
          })),
        };

        const credential = (await navigator.credentials.get({
          publicKey,
        })) as PublicKeyCredential;

        if (!credential) {
          throw clientError("passkey_no_credential");
        }

        const response = credential.response as AuthenticatorAssertionResponse;
        assertion = {
          id: credential.id,
          rawId: bytesToBase64url(credential.rawId),
          type: credential.type,
          response: {
            clientDataJSON: bytesToBase64url(response.clientDataJSON),
            authenticatorData: bytesToBase64url(response.authenticatorData),
            signature: bytesToBase64url(response.signature),
            userHandle: response.userHandle ? bytesToBase64url(response.userHandle) : null,
          },
        };

        await api.authPasskeyLoginFinish(identifier, assertion);
      }

      await refresh();
      navigate(next, { replace: true });
    } catch (err: any) {
      setError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  async function handleOpStart(e: FormEvent) {
    e.preventDefault();
    if (!opEmail.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await api.opLoginStart(opEmail.trim());
      setOpRequestId(res.request_id);
    } catch (err) {
      setError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  async function handleOpFinish(e: FormEvent) {
    e.preventDefault();
    if (!opRequestId || !opCode.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await api.opLoginFinish(opRequestId, opCode.trim());
      await refresh();
      navigate(next, { replace: true });
    } catch (err) {
      setError(humanizeError(err));
      setSubmitting(false);
    }
  }

  function switchTab(tab: "main" | "bind" | "op") {
    setError(null);
    setActiveTab(tab);
  }

  return (
    <AuthLayout
      title={t("login_title")}
      subtitle={t(isOpHost ? "login_subtitle_op" : "login_subtitle")}
    >
      {sessionEnded && (
        <p
          role="status"
          className="rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-800 dark:text-amber-200"
        >
          {t("session_ended_notice")}
        </p>
      )}
      <Card>
        <CardContent className="pt-6">
          {activeTab === "main" && (
            <div className="space-y-4">
              <form onSubmit={handleEmailSubmit} className="space-y-4">
                <div className="space-y-2">
                  <Label htmlFor="email">{t("email_address")}</Label>
                  <div className="flex gap-2">
                    <Input
                      id="email"
                      type="email"
                      placeholder={t("email_placeholder")}
                      value={email}
                      onChange={(e) => setEmail(e.target.value)}
                      autoComplete="email webauthn"
                      autoCapitalize="none"
                      autoCorrect="off"
                      spellCheck={false}
                      autoFocus
                      disabled={submitting || otpSent}
                      aria-invalid={error ? true : undefined}
                      className="flex-1"
                    />
                    <Button
                      type="button"
                      variant="outline"
                      onClick={handleSendOtp}
                      disabled={submitting || !email.trim() || countdown > 0}
                      className="shrink-0 font-normal"
                    >
                      {submitting && !otpSent ? (
                        <>
                          <Loader2 className="mr-1 h-3 w-3 animate-spin" />
                          {t("sending_otp")}
                        </>
                      ) : countdown > 0 ? (
                        `${countdown}${t("resend_in")}`
                      ) : (
                        t("send_otp")
                      )}
                    </Button>
                  </div>
                  {otpSent && (
                    <p className="text-[11px] text-emerald-600 dark:text-emerald-400 mt-1 leading-normal">
                      {t("otp_sent")}
                    </p>
                  )}
                </div>

                {otpSent && (
                  <div className="space-y-2">
                    <Label htmlFor="otpCode">{t("otp_code")}</Label>
                    <Input
                      id="otpCode"
                      placeholder={t("otp_placeholder")}
                      value={otpCode}
                      onChange={(e) => setOtpCode(e.target.value)}
                      autoComplete="one-time-code"
                      autoCapitalize="none"
                      autoCorrect="off"
                      spellCheck={false}
                      autoFocus
                      disabled={submitting}
                      aria-invalid={error ? true : undefined}
                    />
                  </div>
                )}

                <InlineError message={error} />

                {otpSent && (
                  <Button
                    type="submit"
                    className="w-full"
                    disabled={submitting || !otpCode.trim()}
                  >
                    {submitting ? (
                      <>
                        <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                        {t("signing_in")}
                      </>
                    ) : (
                      <>
                        <Mail className="mr-2 h-4 w-4" />
                        {t("otp_btn")}
                      </>
                    )}
                  </Button>
                )}
              </form>

              <div className="relative my-2">
                <div className="absolute inset-0 flex items-center">
                  <div className="w-full border-t border-muted" />
                </div>
                <div className="relative flex justify-center text-[10px] uppercase">
                  <span className="bg-card px-2 text-muted-foreground font-semibold tracking-wider">
                    {t("or_divider")}
                  </span>
                </div>
              </div>

              <div className="space-y-2">
                <Button
                  type="button"
                  variant="outline"
                  className="w-full justify-center gap-2 font-medium"
                  onClick={handlePasskeyLoginClick}
                  disabled={submitting}
                >
                  <Fingerprint className="h-4 w-4 text-primary" />
                  {t("passkey_btn")}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  className="w-full justify-center gap-2 font-medium"
                  onClick={() => switchTab("bind")}
                  disabled={submitting}
                >
                  <KeyRound className="h-4 w-4 text-muted-foreground" />
                  {t("tab_bind_btn")}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  className="w-full justify-center gap-2 font-medium"
                  onClick={() => switchTab("op")}
                  disabled={submitting}
                >
                  <ShieldCheck className="h-4 w-4 text-muted-foreground" />
                  {t("tab_op_btn")}
                </Button>
              </div>
            </div>
          )}

          {activeTab === "bind" && (
            <form onSubmit={handleBindSubmit} className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor="bindCode">{t("bind_code")}</Label>
                <Input
                  id="bindCode"
                  placeholder={t("bind_code_placeholder")}
                  value={bindCode}
                  onChange={(e) => setBindCode(e.target.value)}
                  autoCapitalize="characters"
                  autoCorrect="off"
                  spellCheck={false}
                  autoFocus
                  disabled={submitting}
                  aria-invalid={error ? true : undefined}
                />
                <p className="text-[11px] text-muted-foreground/80 mt-1 leading-normal">
                  {t("bind_hint")}
                </p>
              </div>
              <InlineError message={error} />
              <Button
                type="submit"
                className="w-full"
                disabled={submitting || !bindCode.trim()}
              >
                {submitting ? (
                  <>
                    <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                    {t("binding")}
                  </>
                ) : (
                  <>
                    <KeyRound className="mr-2 h-4 w-4" />
                    {t("bind_btn")}
                  </>
                )}
              </Button>

              <div className="mt-4 text-center">
                <button
                  type="button"
                  onClick={() => switchTab("main")}
                  className="text-xs text-muted-foreground hover:text-primary transition-colors inline-flex items-center gap-1 font-medium"
                >
                  <span>←</span>
                  <span>{t("back_to_login")}</span>
                </button>
              </div>
            </form>
          )}

          {activeTab === "op" && !opRequestId && (
            <form onSubmit={handleOpStart} className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor="opEmail">{t("email_address")}</Label>
                <Input
                  id="opEmail"
                  type="email"
                  placeholder={t("email_placeholder")}
                  value={opEmail}
                  onChange={(e) => setOpEmail(e.target.value)}
                  autoComplete="email"
                  autoCapitalize="none"
                  autoCorrect="off"
                  spellCheck={false}
                  autoFocus
                  disabled={submitting}
                  aria-invalid={error ? true : undefined}
                />
                <p className="text-[11px] text-muted-foreground/80 mt-1 leading-normal">
                  {t("op_hint")}
                </p>
              </div>
              <InlineError message={error} />
              <Button
                type="submit"
                className="w-full"
                disabled={submitting || !opEmail.trim()}
              >
                {submitting ? (
                  <>
                    <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                    {t("sending_otp")}
                  </>
                ) : (
                  <>
                    <ShieldCheck className="mr-2 h-4 w-4" />
                    {t("op_start_btn")}
                  </>
                )}
              </Button>

              <div className="mt-4 text-center">
                <button
                  type="button"
                  onClick={() => switchTab("main")}
                  className="text-xs text-muted-foreground hover:text-primary transition-colors inline-flex items-center gap-1 font-medium"
                >
                  <span>←</span>
                  <span>{t("back_to_login")}</span>
                </button>
              </div>
            </form>
          )}

          {activeTab === "op" && opRequestId && (
            <form onSubmit={handleOpFinish} className="space-y-4">
              <div className="rounded-md border bg-muted/40 p-3 space-y-2">
                <p className="text-[11px] text-muted-foreground leading-normal">
                  {t("op_approve_hint")}
                </p>
                <p className="font-mono text-xs break-all select-all">
                  /felis web op approve {opRequestId}
                </p>
              </div>

              {opApproved ? (
                <p className="text-[11px] text-emerald-600 dark:text-emerald-400 leading-normal">
                  {t("op_approved")}
                </p>
              ) : (
                <p className="inline-flex items-center gap-2 text-[11px] text-muted-foreground leading-normal">
                  <Loader2 className="h-3 w-3 animate-spin" />
                  {t("op_waiting")}
                </p>
              )}

              <div className="space-y-2">
                <Label htmlFor="opCode">{t("otp_code")}</Label>
                <Input
                  id="opCode"
                  placeholder={t("otp_placeholder")}
                  value={opCode}
                  onChange={(e) => setOpCode(e.target.value)}
                  autoComplete="one-time-code"
                  autoCapitalize="none"
                  autoCorrect="off"
                  spellCheck={false}
                  disabled={submitting}
                  aria-invalid={error ? true : undefined}
                />
              </div>

              <InlineError message={error} />

              <Button
                type="submit"
                className="w-full"
                disabled={submitting || !opCode.trim() || !opApproved}
              >
                {submitting ? (
                  <>
                    <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                    {t("signing_in")}
                  </>
                ) : (
                  <>
                    <ShieldCheck className="mr-2 h-4 w-4" />
                    {t("otp_btn")}
                  </>
                )}
              </Button>

              <div className="mt-4 text-center">
                <button
                  type="button"
                  onClick={() => {
                    setOpRequestId(null);
                    setOpApproved(false);
                    setOpCode("");
                    switchTab("op");
                  }}
                  className="text-xs text-muted-foreground hover:text-primary transition-colors inline-flex items-center gap-1 font-medium"
                >
                  <span>←</span>
                  <span>{t("op_restart")}</span>
                </button>
              </div>
            </form>
          )}
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
