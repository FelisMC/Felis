import { useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { AuthLayout } from "@/components/AuthLayout";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useTier } from "@/lib/tier";
import { api, humanizeError } from "@/lib/api";

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

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Don't flash the form while the boot /me is still in flight: a signed-in visitor
  // would briefly see a login form before being redirected away.
  if (loading) {
    return (
      <AuthLayout title="Felis">
        <div className="flex items-center justify-center gap-2 py-8 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading…
        </div>
      </AuthLayout>
    );
  }
  if (identity && mustChangePassword) return <Navigate to="/change-password" replace />;
  if (identity) return <Navigate to="/" replace />;

  async function submit(e: FormEvent) {
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

  return (
    <AuthLayout title="Sign in to Felis" subtitle="Operator console">
      <Card>
        <CardContent className="pt-5">
          <form onSubmit={submit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="username">Username</Label>
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
              <Label htmlFor="password">Password</Label>
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
              {submitting ? "Signing in…" : "Sign in"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
