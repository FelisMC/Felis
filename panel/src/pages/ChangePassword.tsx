import { useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AuthLayout } from "@/components/AuthLayout";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useTier } from "@/lib/tier";
import { api, humanizeError } from "@/lib/api";

// Minimum new-password length. The server is the source of truth (8–72 BYTES, the
// bcrypt limit); this is only a pre-submit courtesy so the obvious case fails
// instantly rather than round-tripping to a `weak_password` error.
const MIN_PASSWORD = 8;

// ChangePassword is the forced first-login change AND the voluntary change surface
// (spec §B1). It lives OUTSIDE RequireAuth on purpose: RequireAuth redirects a
// must-change principal *to* this page, so nesting it under that gate would loop.
// It therefore re-checks auth itself — a 401 principal is sent to /login.
//
// On success the server keeps the caller's own session (revoking only the others),
// so no re-login is needed: we refresh /me — which now reports must_change_password
// false — and continue into the app.
export function ChangePassword() {
  const { loading, unauthenticated, mustChangePassword, refresh } = useTier();
  const navigate = useNavigate();
  const { t } = useTranslation("auth");

  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
  if (unauthenticated) return <Navigate to="/login" replace />;

  const mismatch = confirm.length > 0 && next !== confirm;
  const tooShort = next.length > 0 && next.length < MIN_PASSWORD;
  const canSubmit =
    !submitting &&
    current.length > 0 &&
    next.length >= MIN_PASSWORD &&
    next === confirm;

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!canSubmit) return;
    setSubmitting(true);
    setError(null);
    try {
      await api.changePassword(current, next);
      await refresh();
      navigate("/", { replace: true });
    } catch (err) {
      setError(humanizeError(err));
      setSubmitting(false);
    }
  }

  return (
    <AuthLayout
      title={t("change_password_title")}
      subtitle={
        mustChangePassword
          ? t("change_password_subtitle_forced")
          : t("change_password_subtitle_voluntary")
      }
    >
      <Card>
        <CardContent className="pt-5">
          <form onSubmit={submit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="current">{t("current_password")}</Label>
              <Input
                id="current"
                type="password"
                value={current}
                onChange={(e) => setCurrent(e.target.value)}
                autoComplete="current-password"
                autoFocus
                aria-invalid={error ? true : undefined}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="new-password">{t("new_password")}</Label>
              <Input
                id="new-password"
                type="password"
                value={next}
                onChange={(e) => setNext(e.target.value)}
                autoComplete="new-password"
                aria-invalid={tooShort ? true : undefined}
              />
              {tooShort && (
                <p className="text-xs text-muted-foreground">
                  {t("password_min_length", { min: MIN_PASSWORD })}
                </p>
              )}
            </div>
            <div className="space-y-2">
              <Label htmlFor="confirm-password">{t("confirm_new_password")}</Label>
              <Input
                id="confirm-password"
                type="password"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                autoComplete="new-password"
                aria-invalid={mismatch ? true : undefined}
              />
              {mismatch && (
                <p className="text-xs text-destructive">{t("password_mismatch")}</p>
              )}
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={!canSubmit}>
              {submitting ? t("saving") : t("change_password_btn")}
            </Button>
          </form>
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
