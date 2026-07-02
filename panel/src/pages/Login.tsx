import { useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { Loader2, KeyRound } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AuthLayout } from "@/components/AuthLayout";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useTier } from "@/lib/tier";
import { api, humanizeError } from "@/lib/api";
import { cn } from "@/lib/utils";

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

  const [activeTab, setActiveTab] = useState<"password" | "bind">("password");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [bindCode, setBindCode] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

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

  return (
    <AuthLayout title={t("login_title")} subtitle={t("login_subtitle")}>
      <Card>
        <CardContent className="pt-5">
          {/* Tab Selector */}
          <div className="grid grid-cols-2 gap-1 rounded-lg bg-muted p-1 text-muted-foreground select-none mb-4">
            <button
              type="button"
              onClick={() => {
                setError(null);
                setActiveTab("password");
              }}
              className={cn(
                "inline-flex items-center justify-center whitespace-nowrap rounded-md py-1.5 text-xs font-semibold transition-all focus-visible:outline-none",
                activeTab === "password"
                  ? "bg-background text-foreground shadow-sm"
                  : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
              )}
            >
              {t("tab_password")}
            </button>
            <button
              type="button"
              onClick={() => {
                setError(null);
                setActiveTab("bind");
              }}
              className={cn(
                "inline-flex items-center justify-center whitespace-nowrap rounded-md py-1.5 text-xs font-semibold transition-all focus-visible:outline-none",
                activeTab === "bind"
                  ? "bg-background text-foreground shadow-sm"
                  : "text-muted-foreground hover:bg-background/30 hover:text-foreground",
              )}
            >
              {t("tab_bind")}
            </button>
          </div>

          {activeTab === "password" ? (
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
          ) : (
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
            </form>
          )}
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
