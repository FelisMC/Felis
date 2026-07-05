import { useState, useEffect, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { Loader2, KeyRound, Mail, Fingerprint } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AuthLayout } from "@/components/AuthLayout";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useTier } from "@/lib/tier";
import { api, humanizeError } from "@/lib/api";
import { base64urlToBytes, bytesToBase64url } from "@/lib/utils";

// Login is the local-password sign-in (spec §B1). It is the ONLY local credential
// surface — username + password; Passkey/PWA onboarding is Phase B2/C. On success
// the API sets an HttpOnly session cookie (invisible here); we then refresh the tier
// context so the gate re-evaluates, and route to the forced change-password card
// when the account still owes its first-login change, else to the dashboard.
//
// Reaching this page already-authenticated (e.g. typing /login while signed in)
// short-circuits to the right destination rather than showing the form.
export function Login() {
  const { loading, identity, mustChangePassword, refresh } = useTier();
  const navigate = useNavigate();
  const { t } = useTranslation("auth");

  const [activeTab, setActiveTab] = useState<"password" | "bind" | "email">("password");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [bindCode, setBindCode] = useState("");
  const [email, setEmail] = useState("");
  const [otpCode, setOtpCode] = useState("");
  const [otpSent, setOtpSent] = useState(false);
  const [countdown, setCountdown] = useState(0);
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
  if (identity && mustChangePassword) return <Navigate to="/change-password" replace />;
  if (identity) return <Navigate to="/" replace />;

  async function handlePasswordSubmit(e: FormEvent) {
    e.preventDefault();
    if (!username.trim() || !password || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await api.login(username.trim(), password);
      // Re-read /me so the context reflects the new session before we leave this
      // page; the route we land on is gated on that fresh state.
      await refresh();
      navigate(res.must_change_password ? "/change-password" : "/", { replace: true });
    } catch (err) {
      setError(humanizeError(err));
      setSubmitting(false);
    }
  }

  async function handleBindSubmit(e: FormEvent) {
    e.preventDefault();
    const code = bindCode.trim();
    if (!code || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await api.bind(code);
      await refresh();
      navigate("/", { replace: true });
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
      navigate("/", { replace: true });
    } catch (err) {
      setError(humanizeError(err));
      setSubmitting(false);
    }
  }

  async function handlePasskeyLoginClick() {
    if (submitting) return;
    setSubmitting(true);
    setError(null);

    const identifier = username.trim();
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
          throw new Error("Failed to get credential");
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
        // Username-first (Email-first) passkey login
        if (!identifier.includes("@")) {
          throw new Error("使用 Passkey 登录请在上方输入框中输入您绑定的邮箱，或留空直接进行免密登录。");
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
          throw new Error("Failed to get credential");
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
      navigate("/", { replace: true });
    } catch (err: any) {
      setError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <AuthLayout title={t("login_title")} subtitle={t("login_subtitle")}>
      <Card>
        <CardContent className="pt-6">
          {activeTab === "password" && (
            <div className="space-y-4">
              <form onSubmit={handlePasswordSubmit} className="space-y-4">
                <div className="space-y-2">
                  <Label htmlFor="username">{t("username")}</Label>
                  <Input
                    id="username"
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                    autoComplete="username"
                    autoCapitalize="none"
                    autoCorrect="off"
                    spellCheck={false}
                    autoFocus
                    aria-invalid={error ? true : undefined}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="password">{t("password")}</Label>
                  <Input
                    id="password"
                    type="password"
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    autoComplete="current-password"
                    aria-invalid={error ? true : undefined}
                  />
                </div>
                {error && <p className="text-sm text-destructive">{error}</p>}
                <Button
                  type="submit"
                  className="w-full"
                  disabled={submitting || !username.trim() || !password}
                >
                  {submitting ? t("signing_in") : t("sign_in")}
                </Button>
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
                  onClick={() => {
                    setError(null);
                    setActiveTab("email");
                  }}
                  disabled={submitting}
                >
                  <Mail className="h-4 w-4 text-muted-foreground" />
                  {t("tab_email_btn")}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  className="w-full justify-center gap-2 font-medium"
                  onClick={() => {
                    setError(null);
                    setActiveTab("bind");
                  }}
                  disabled={submitting}
                >
                  <KeyRound className="h-4 w-4 text-muted-foreground" />
                  {t("tab_bind_btn")}
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
              {error && <p className="text-sm text-destructive">{error}</p>}
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
                  onClick={() => {
                    setError(null);
                    setActiveTab("password");
                  }}
                  className="text-xs text-muted-foreground hover:text-primary transition-colors inline-flex items-center gap-1 font-medium"
                >
                  <span>←</span>
                  <span>{t("back_to_password")}</span>
                </button>
              </div>
            </form>
          )}

          {activeTab === "email" && (
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
                    autoComplete="email"
                    autoCapitalize="none"
                    autoCorrect="off"
                    spellCheck={false}
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

              {error && <p className="text-sm text-destructive">{error}</p>}

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

              <div className="mt-4 text-center">
                <button
                  type="button"
                  onClick={() => {
                    setError(null);
                    setActiveTab("password");
                  }}
                  className="text-xs text-muted-foreground hover:text-primary transition-colors inline-flex items-center gap-1 font-medium"
                >
                  <span>←</span>
                  <span>{t("back_to_password")}</span>
                </button>
              </div>
            </form>
          )}
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
