import { useState, useRef, useEffect, type FormEvent } from "react";
import { ArrowRightLeft, CheckCircle2, Link2, LogOut, ShieldCheck, UserRound, Mail, Fingerprint, Trash2, KeyRound } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Loading, ErrorState } from "@/components/States";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { MessageLine, InlineError } from "@/components/MessageLine";
import { PageHeader } from "@/components/PageHeader";
import { api, clientError, humanizeError } from "@/lib/api";
import { formatAbsolute } from "@/lib/format";
import type { PasskeyCredential } from "@/lib/types";
import { useAsync } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { base64urlToBytes, bytesToBase64url } from "@/lib/utils";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";

// The Account page is the web half of the §10 link flow. A code is born in-game
// (online-mode auth proves the UUID) and consumed here (the session proves the
// user) — so this page only ever reports status and redeems a code; it can never
// originate a binding. The Minecraft-link card is a small state machine: checking
// → linked, or → the two-step "get a code in-game, enter it here" form.

export function Account() {
  const status = useAsync(() => api.linkStatus(), []);
  const { identity, refresh } = useTier();
  const { t, i18n } = useTranslation("account");

  // Email verification state
  const [emailInput, setEmailInput] = useState("");
  const [otpCodeInput, setOtpCodeInput] = useState("");
  const [emailSending, setEmailSending] = useState(false);
  const [emailVerifying, setEmailVerifying] = useState(false);
  const [emailError, setEmailError] = useState<string | null>(null);
  const [emailSent, setEmailSent] = useState(false);
  const [sentEmailAddress, setSentEmailAddress] = useState("");
  const [initializedEmail, setInitializedEmail] = useState(false);

  useEffect(() => {
    if (identity?.email && !initializedEmail) {
      setEmailInput(identity.email);
      setInitializedEmail(true);
    }
  }, [identity, initializedEmail]);

  async function sendEmailOtp(e: FormEvent) {
    e.preventDefault();
    const trimmed = emailInput.trim();
    if (!trimmed || emailSending) return;
    setEmailSending(true);
    setEmailError(null);
    try {
      await api.emailStart(trimmed);
      setEmailSent(true);
      setSentEmailAddress(trimmed);
    } catch (err) {
      setEmailError(humanizeError(err));
    } finally {
      setEmailSending(false);
    }
  }

  async function verifyEmailOtp(e: FormEvent) {
    e.preventDefault();
    const trimmedCode = otpCodeInput.trim();
    if (!trimmedCode || emailVerifying) return;
    setEmailVerifying(true);
    setEmailError(null);
    try {
      await api.emailVerify(trimmedCode);
      await refresh();
      setEmailSent(false);
      setEmailInput("");
      setOtpCodeInput("");
    } catch (err) {
      setEmailError(humanizeError(err));
    } finally {
      setEmailVerifying(false);
    }
  }

  // Passkeys list
  const passkeys = useAsync(() => api.passkeyList(), []);

  // Passkey registration state
  const [passkeyNickname, setPasskeyNickname] = useState("");
  const [registeringPasskey, setRegisteringPasskey] = useState(false);
  const [passkeyError, setPasskeyError] = useState<string | null>(null);
  const [registerDialogOpen, setRegisterDialogOpen] = useState(false);
  // Deleting a passkey goes through a confirm dialog that names it. The API
  // refuses to remove the only passkey of an account whose email is unverified
  // (it would be left with no way back in); the button mirrors that rule so the
  // refusal is explained up front instead of after a round trip.
  const [pendingDelete, setPendingDelete] = useState<PasskeyCredential | null>(null);
  const [deletingPasskey, setDeletingPasskey] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const credentials = passkeys.data?.credentials ?? [];
  const keepLastPasskey = credentials.length === 1 && !identity?.email_verified;

  const abortControllerRef = useRef<AbortController | null>(null);

  function cancelRegistration() {
    if (abortControllerRef.current) {
      abortControllerRef.current.abort();
    }
    setRegisterDialogOpen(false);
    setPasskeyNickname("");
    setPasskeyError(null);
    setRegisteringPasskey(false);
  }

  async function handleRegisterPasskey(e: FormEvent) {
    e.preventDefault();
    const name = passkeyNickname.trim();
    if (!name || registeringPasskey) return;
    setRegisteringPasskey(true);
    setPasskeyError(null);

    const controller = new AbortController();
    abortControllerRef.current = controller;

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
        signal: controller.signal,
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
          transports: typeof response.getTransports === "function" ? response.getTransports() : [],
        },
      };

      await api.passkeyRegisterFinish(name, attestation);
      await passkeys.reload();
      setRegisterDialogOpen(false);
      setPasskeyNickname("");
    } catch (err: any) {
      setPasskeyError(humanizeError(err));
    } finally {
      setRegisteringPasskey(false);
      abortControllerRef.current = null;
    }
  }

  function askDeletePasskey(cred: PasskeyCredential) {
    setDeleteError(null);
    setPendingDelete(cred);
  }

  async function confirmDeletePasskey() {
    if (!pendingDelete || deletingPasskey) return;
    setDeletingPasskey(true);
    setDeleteError(null);
    try {
      await api.passkeyDelete(pendingDelete.id);
      setPendingDelete(null);
      await passkeys.reload();
    } catch (err) {
      // Another device may have changed the list meanwhile: refresh it. A 404
      // means the passkey is already gone, which is what was asked for.
      void passkeys.reload();
      if ((err as { code?: string }).code === "not_found") {
        setPendingDelete(null);
      } else {
        setDeleteError(humanizeError(err));
      }
    } finally {
      setDeletingPasskey(false);
    }
  }

  // Sign-out ends a local session (passkey / email-OTP / bind-code / op-login):
  // clear it server-side, then refresh /me. For a local session that read now 401s
  // → the tier model flips to
  // `unauthenticated` and RequireAuth bounces this page to /login, so no explicit
  // navigation is needed. (On a Zero-Trust proxied session there is no local cookie
  // to drop and /me still succeeds — sign-out is a no-op, which is the honest
  // outcome: you cannot sign out of your org's access proxy from here.)
  const [signingOut, setSigningOut] = useState(false);
  async function signOut() {
    if (signingOut) return;
    setSigningOut(true);
    try {
      await api.logout();
    } finally {
      await refresh();
    }
  }

  // Verify is a mutation, not a read, so it is hand-rolled (the useAsync producer
  // is for the status read). On success we flip linked locally and keep the echoed
  // UUID — the start endpoint never returns it, so this is the only confirmation
  // of *which* account bound, shown without a refetch.
  const [code, setCode] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [verifiedUUID, setVerifiedUUID] = useState<string | null>(null);

  const linked = verifiedUUID !== null || status.data?.linked === true;

  async function submit(e: FormEvent) {
    e.preventDefault();
    const trimmed = code.trim().toUpperCase();
    if (!trimmed || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await api.linkVerify(trimmed);
      setVerifiedUUID(res.mc_uuid);
      setCode("");
    } catch (err) {
      setError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <>
      <PageHeader icon={UserRound} title={t("title")} subtitle={t("subtitle")} />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Link2 className="h-4 w-4 text-primary" /> {t("minecraft_link")}
          </CardTitle>
        </CardHeader>
        <CardContent className="text-sm">
          {linked ? (
            <LinkedState uuid={verifiedUUID} />
          ) : status.loading && !status.data ? (
            <Loading label={t("checking_link")} />
          ) : status.error ? (
            <ErrorState error={status.error} onRetry={status.reload} />
          ) : (
            <LinkForm
              code={code}
              setCode={setCode}
              submitting={submitting}
              error={error}
              onSubmit={submit}
            />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Mail className="h-4 w-4 text-primary" /> {t("email_verification")}
          </CardTitle>
        </CardHeader>
        <CardContent className="text-sm">
          {identity?.email_verified ? (
            <div className="space-y-3">
              <div className="flex items-center gap-2 font-medium text-foreground">
                <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                {t("email_verified")}
              </div>
              <p className="text-muted-foreground">{t("email_desc")}</p>
              <div className="flex items-center gap-2 text-muted-foreground">
                <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
                  {identity.email}
                </code>
              </div>
            </div>
          ) : (
            <ol className="space-y-4">
              <li className="flex gap-3">
                <StepBadge n={1} />
                <div className="w-full space-y-2">
                  <p className="font-medium text-foreground">{t("email_step1")}</p>
                  <p className="text-muted-foreground">{t("email_step1_desc")}</p>
                  {!emailSent ? (
                    <form onSubmit={sendEmailOtp} className="flex gap-2 max-w-md">
                      <Input
                        type="email"
                        placeholder="user@example.com"
                        value={emailInput}
                        onChange={(e) => setEmailInput(e.target.value)}
                        disabled={emailSending}
                        required
                        className="max-w-[18rem]"
                      />
                      <Button type="submit" disabled={emailSending || !emailInput}>
                        {emailSending ? t("sending_code") : t("send_code")}
                      </Button>
                    </form>
                  ) : (
                    <div className="flex items-center gap-2 text-emerald-600 font-medium dark:text-emerald-400">
                      <CheckCircle2 className="h-4 w-4" />
                      <span>{t("email_otp_sent")} ({sentEmailAddress})</span>
                      <Button
                        variant="link"
                        size="sm"
                        onClick={() => setEmailSent(false)}
                        className="h-auto p-0 font-normal"
                      >
                        {t("change_email")}
                      </Button>
                    </div>
                  )}
                </div>
              </li>
              {emailSent && (
                <li className="flex gap-3">
                  <StepBadge n={2} />
                  <div className="w-full space-y-2">
                    <p className="font-medium text-foreground">{t("email_step2")}</p>
                    <p className="text-muted-foreground">{t("email_step2_desc")}</p>
                    <form onSubmit={verifyEmailOtp} className="flex gap-2 max-w-md">
                      <Input
                        type="text"
                        placeholder={t("otp_code_placeholder")}
                        value={otpCodeInput}
                        onChange={(e) => setOtpCodeInput(e.target.value)}
                        disabled={emailVerifying}
                        maxLength={6}
                        required
                        className="max-w-[12rem] font-mono text-center tracking-[0.2em]"
                      />
                      <Button type="submit" disabled={emailVerifying || otpCodeInput.trim().length !== 6}>
                        {emailVerifying ? t("email_verifying") : t("email_verify_btn")}
                      </Button>
                    </form>
                  </div>
                </li>
              )}
              <InlineError message={emailError} className="ml-9" />
            </ol>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0">
          <CardTitle className="flex items-center gap-2 text-base">
            <Fingerprint className="h-4 w-4 text-primary" /> {t("passkeys")}
          </CardTitle>
          <Dialog open={registerDialogOpen} onOpenChange={(open) => {
            if (!open) {
              cancelRegistration();
            } else {
              setRegisterDialogOpen(true);
            }
          }}>
            <DialogTrigger asChild>
              <Button size="sm" variant="outline">
                <Fingerprint className="mr-1 h-3.5 w-3.5" />
                {t("add_passkey")}
              </Button>
            </DialogTrigger>
            <DialogContent hideClose={registeringPasskey}>
              <form onSubmit={handleRegisterPasskey}>
                <DialogHeader>
                  <DialogTitle>{t("add_passkey")}</DialogTitle>
                  <DialogDescription>
                    {t("passkeys_desc")}
                  </DialogDescription>
                </DialogHeader>
                <div className="grid gap-4 py-4">
                  <div className="grid gap-2">
                    <Label htmlFor="pk-name">{t("passkey_name")}</Label>
                    <Input
                      id="pk-name"
                      placeholder={t("passkey_name_placeholder")}
                      value={passkeyNickname}
                      onChange={(e) => setPasskeyNickname(e.target.value)}
                      disabled={registeringPasskey}
                      required
                    />
                  </div>
                  <InlineError message={passkeyError} />
                </div>
                <DialogFooter>
                  <Button
                    type="button"
                    variant="ghost"
                    onClick={cancelRegistration}
                    disabled={registeringPasskey}
                  >
                    {t("common:cancel")}
                  </Button>
                  <Button type="submit" disabled={registeringPasskey || !passkeyNickname.trim()}>
                    {registeringPasskey ? t("registering_passkey") : t("continue_btn")}
                  </Button>
                </DialogFooter>
              </form>
            </DialogContent>
          </Dialog>
        </CardHeader>
        <CardContent className="text-sm space-y-4">
          <p className="text-muted-foreground">{t("passkeys_desc")}</p>
          {passkeys.loading && !passkeys.data ? (
            <Loading label={t("loading_passkeys")} />
          ) : passkeys.error ? (
            <ErrorState error={passkeys.error} onRetry={passkeys.reload} />
          ) : credentials.length === 0 ? (
            <p className="text-xs text-muted-foreground py-2 italic">{t("no_passkeys")}</p>
          ) : (
            <>
              <ul className="border rounded-md divide-y bg-background/50">
                {credentials.map((cred) => (
                  <li key={cred.id} className="flex items-center justify-between gap-3 p-3">
                    <div className="min-w-0 space-y-1">
                      <p className="font-medium text-foreground flex items-center gap-1.5">
                        <KeyRound className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                        <span className="truncate">{cred.name}</span>
                      </p>
                      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                        <span>
                          {t("created_at")}
                          {formatAbsolute(cred.created_at, i18n.language)}
                        </span>
                        <span>
                          {t("last_used")}
                          {cred.last_used_at ? formatAbsolute(cred.last_used_at, i18n.language) : t("never")}
                        </span>
                      </div>
                    </div>
                    <Button
                      size="icon"
                      variant="ghost"
                      onClick={() => askDeletePasskey(cred)}
                      disabled={keepLastPasskey}
                      aria-label={t("passkey_delete_aria", { name: cred.name })}
                      title={keepLastPasskey ? t("passkey_last_hint") : t("passkey_delete_aria", { name: cred.name })}
                      className="shrink-0 text-muted-foreground hover:text-destructive"
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  </li>
                ))}
              </ul>
              {keepLastPasskey && (
                <p className="text-xs text-muted-foreground">{t("passkey_last_hint")}</p>
              )}
            </>
          )}
          <Dialog
            open={pendingDelete !== null}
            onOpenChange={(open) => {
              if (!open && !deletingPasskey) setPendingDelete(null);
            }}
          >
            <DialogContent hideClose={deletingPasskey}>
              <DialogHeader>
                <DialogTitle>{t("passkey_delete_title")}</DialogTitle>
                <DialogDescription>
                  {pendingDelete &&
                    t("passkey_delete_desc", {
                      name: pendingDelete.name,
                      created: formatAbsolute(pendingDelete.created_at, i18n.language),
                    })}
                </DialogDescription>
              </DialogHeader>
              {deleteError && <MessageLine kind="error" message={deleteError} />}
              <ConfirmFooter
                onCancel={() => setPendingDelete(null)}
                onConfirm={() => void confirmDeletePasskey()}
                loading={deletingPasskey}
                disabled={deletingPasskey || keepLastPasskey}
                cancelLabel={t("common:cancel")}
                confirmLabel={t("passkey_delete_confirm")}
              />
            </DialogContent>
          </Dialog>
        </CardContent>
      </Card>

      <MigrationCard
        userId={identity?.user_id}
        hasPasskey={credentials.length > 0}
      />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldCheck className="h-4 w-4 text-primary" /> {t("session")}
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-4 text-sm text-muted-foreground">
          <p>{t("session_desc")}</p>
          <Button
            variant="outline"
            size="sm"
            onClick={signOut}
            disabled={signingOut}
          >
            <LogOut className="mr-2 h-4 w-4" />
            {signingOut ? t("signing_out") : t("sign_out")}
          </Button>
        </CardContent>
      </Card>
    </>
  );
}

/** LinkedState confirms the binding. The UUID is shown only when this session
 *  just verified it (start does not return it), so a pre-existing link renders
 *  the confirmation without a UUID rather than inventing one. */
function LinkedState({ uuid }: { uuid: string | null }) {
  const { t } = useTranslation("account");
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2 font-medium text-foreground">
        <CheckCircle2 className="h-4 w-4 text-emerald-500" />
        {t("linked_title")}
      </div>
      <p className="text-muted-foreground">{t("linked_desc")}</p>
      {uuid && (
        <div className="flex items-center gap-2 text-muted-foreground">
          <span className="text-xs uppercase tracking-wide">{t("uuid_label")}</span>
          <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
            {uuid}
          </code>
        </div>
      )}
    </div>
  );
}

/** LinkForm is the two-step redemption UX: get a code in-game, then enter it. The
 *  input auto-uppercases for instant feedback; the server also trims + uppercases,
 *  so this is cosmetic, not the source of truth. */
function LinkForm({
  code,
  setCode,
  submitting,
  error,
  onSubmit,
}: {
  code: string;
  setCode: (v: string) => void;
  submitting: boolean;
  error: string | null;
  onSubmit: (e: FormEvent) => void;
}) {
  const { t } = useTranslation("account");
  return (
    <ol className="space-y-4">
      <li className="flex gap-3">
        <StepBadge n={1} />
        <div className="space-y-1">
          <p className="font-medium text-foreground">{t("step1_title")}</p>
          <p className="text-muted-foreground">
            {t("step1_desc_prefix")}
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
              /link
            </code>
            {t("step1_desc_suffix")}
          </p>
        </div>
      </li>
      <li className="flex gap-3">
        <StepBadge n={2} />
        <div className="w-full space-y-2">
          <p className="font-medium text-foreground">{t("step2_title")}</p>
          <form onSubmit={onSubmit} className="space-y-2">
            <Label htmlFor="link-code">{t("link_code")}</Label>
            <div className="flex gap-2">
              <Input
                id="link-code"
                value={code}
                onChange={(e) => setCode(e.target.value.toUpperCase())}
                placeholder={t("link_code_placeholder")}
                autoComplete="off"
                autoCapitalize="characters"
                spellCheck={false}
                maxLength={8}
                className="max-w-[14rem] font-mono uppercase tracking-[0.3em]"
                aria-invalid={error ? true : undefined}
              />
              <Button
                type="submit"
                disabled={submitting || code.trim().length === 0}
              >
                {submitting ? t("verifying") : t("verify_btn")}
              </Button>
            </div>
            <InlineError message={error} />
          </form>
        </div>
      </li>
    </ol>
  );
}

/** MigrationCard is the web half of §B3 account migration (scenario A "inherit").
 *  The flow is born in-game (/felis migrate proves the player) and driven here:
 *  status → step-up confirm (passkey when one is enrolled — the server 409s the
 *  OTP door in that case — else email-OTP) → issue-code (the source names the
 *  target account and reads a one-time code) → redeem (the TARGET account spends
 *  the code; the source's servers move over and the source is retired). Both
 *  roles render on every account: the redeem form is always offered, and the
 *  account id is always shown so a target can hand it to the source. */
function MigrationCard({ userId, hasPasskey }: { userId?: string; hasPasskey: boolean }) {
  const { t } = useTranslation("account");
  const mig = useAsync(() => api.migrateStatus(), []);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const [otpSent, setOtpSent] = useState(false);
  const [otpCode, setOtpCode] = useState("");

  const [targetId, setTargetId] = useState("");
  const [issued, setIssued] = useState<{ code: string; expires_at: string } | null>(null);

  const [redeemCode, setRedeemCode] = useState("");
  const [redeemed, setRedeemed] = useState<{ servers_moved: number; servers: string[] } | null>(null);

  async function run(fn: () => Promise<void>) {
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      await fn();
    } catch (e) {
      setErr(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }

  // Step-up passkey confirm. Unlike the register/login begins (which strip the
  // envelope server-side), the migrate begin returns go-webauthn's raw
  // {"publicKey": {...}} document, so we descend into .publicKey here.
  function confirmWithPasskey() {
    void run(async () => {
      const options = await api.migrateConfirmPasskeyBegin();
      const pk = options.publicKey;
      const publicKey: PublicKeyCredentialRequestOptions = {
        ...pk,
        challenge: base64urlToBytes(pk.challenge),
        allowCredentials: pk.allowCredentials?.map((cred: any) => ({
          ...cred,
          id: base64urlToBytes(cred.id),
        })),
      };
      const credential = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential;
      if (!credential) throw clientError("passkey_no_credential");
      const response = credential.response as AuthenticatorAssertionResponse;
      await api.migrateConfirmPasskeyFinish({
        id: credential.id,
        rawId: bytesToBase64url(credential.rawId),
        type: credential.type,
        response: {
          clientDataJSON: bytesToBase64url(response.clientDataJSON),
          authenticatorData: bytesToBase64url(response.authenticatorData),
          signature: bytesToBase64url(response.signature),
          userHandle: response.userHandle ? bytesToBase64url(response.userHandle) : null,
        },
      });
      await mig.reload();
    });
  }

  const state = mig.data?.active ? mig.data.state : undefined;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <ArrowRightLeft className="h-4 w-4 text-primary" /> {t("migration")}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        <p className="text-muted-foreground">{t("migration_desc")}</p>
        {userId && (
          <div className="flex items-center gap-2 text-muted-foreground">
            <span className="text-xs uppercase tracking-wide">{t("account_id")}</span>
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground select-all">
              {userId}
            </code>
          </div>
        )}

        {mig.loading && !mig.data ? (
          <Loading label={t("migration_checking")} />
        ) : mig.error ? (
          <ErrorState error={mig.error} onRetry={mig.reload} />
        ) : (
          <>
            {!mig.data?.active && !redeemed && (
              <p className="text-muted-foreground">
                {t("migration_none_prefix")}
                <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
                  /felis migrate
                </code>
                {t("migration_none_suffix")}
              </p>
            )}

            {state === "initiated" && (
              <div className="space-y-2">
                <p className="font-medium text-foreground">{t("migration_confirm_title")}</p>
                <p className="text-muted-foreground">{t("migration_confirm_desc")}</p>
                {hasPasskey ? (
                  <Button size="sm" onClick={confirmWithPasskey} disabled={busy}>
                    <Fingerprint className="mr-2 h-4 w-4" />
                    {busy ? t("migration_confirming") : t("migration_confirm_passkey_btn")}
                  </Button>
                ) : !otpSent ? (
                  <Button
                    size="sm"
                    disabled={busy}
                    onClick={() =>
                      void run(async () => {
                        await api.migrateConfirmOTPStart();
                        setOtpSent(true);
                      })
                    }
                  >
                    <Mail className="mr-2 h-4 w-4" />
                    {busy ? t("sending_code") : t("migration_confirm_otp_btn")}
                  </Button>
                ) : (
                  <form
                    className="flex gap-2 max-w-md"
                    onSubmit={(e) => {
                      e.preventDefault();
                      void run(async () => {
                        await api.migrateConfirmOTPVerify(otpCode.trim());
                        await mig.reload();
                      });
                    }}
                  >
                    <Input
                      value={otpCode}
                      onChange={(e) => setOtpCode(e.target.value)}
                      placeholder={t("otp_code_placeholder")}
                      maxLength={6}
                      disabled={busy}
                      className="max-w-[12rem] font-mono text-center tracking-[0.2em]"
                    />
                    <Button type="submit" size="sm" disabled={busy || otpCode.trim().length !== 6}>
                      {busy ? t("migration_confirming") : t("email_verify_btn")}
                    </Button>
                  </form>
                )}
              </div>
            )}

            {state === "confirmed" && !issued && (
              <form
                className="space-y-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  void run(async () => {
                    setIssued(await api.migrateIssueCode(targetId.trim()));
                    await mig.reload();
                  });
                }}
              >
                <p className="font-medium text-foreground">{t("migration_issue_title")}</p>
                <p className="text-muted-foreground">{t("migration_issue_desc")}</p>
                <div className="flex gap-2 max-w-md">
                  <Input
                    value={targetId}
                    onChange={(e) => setTargetId(e.target.value)}
                    placeholder={t("migration_target_placeholder")}
                    disabled={busy}
                    className="font-mono"
                  />
                  <Button type="submit" size="sm" disabled={busy || !targetId.trim()}>
                    {busy ? t("migration_issuing") : t("migration_issue_btn")}
                  </Button>
                </div>
              </form>
            )}

            {issued && (
              <div className="space-y-2">
                <p className="font-medium text-foreground">{t("migration_code_title")}</p>
                <code className="block w-fit rounded bg-muted px-3 py-2 font-mono text-base tracking-[0.2em] text-foreground select-all">
                  {issued.code}
                </code>
                <p className="text-xs text-muted-foreground">
                  {t("migration_code_desc")} ({new Date(issued.expires_at).toLocaleString()})
                </p>
              </div>
            )}

            {state === "code_issued" && !issued && (
              <p className="text-muted-foreground">
                {t("migration_code_pending")}{" "}
                {mig.data?.code_expires_at &&
                  `(${new Date(mig.data.code_expires_at).toLocaleString()})`}
              </p>
            )}

            {redeemed ? (
              <div className="flex items-center gap-2 font-medium text-foreground">
                <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                {t("migration_redeemed", { count: redeemed.servers_moved })}
              </div>
            ) : (
              !mig.data?.active && (
                <form
                  className="space-y-2 border-t pt-4"
                  onSubmit={(e) => {
                    e.preventDefault();
                    void run(async () => {
                      setRedeemed(await api.migrateRedeem(redeemCode.trim()));
                    });
                  }}
                >
                  <p className="font-medium text-foreground">{t("migration_redeem_title")}</p>
                  <p className="text-muted-foreground">{t("migration_redeem_desc")}</p>
                  <div className="flex gap-2 max-w-md">
                    <Input
                      value={redeemCode}
                      onChange={(e) => setRedeemCode(e.target.value)}
                      placeholder={t("migration_redeem_placeholder")}
                      disabled={busy}
                      className="font-mono"
                    />
                    <Button type="submit" size="sm" disabled={busy || !redeemCode.trim()}>
                      {busy ? t("migration_redeeming") : t("migration_redeem_btn")}
                    </Button>
                  </div>
                </form>
              )
            )}

            <InlineError message={err} />
          </>
        )}
      </CardContent>
    </Card>
  );
}

function StepBadge({ n }: { n: number }) {
  return (
    <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-xs font-semibold text-primary">
      {n}
    </span>
  );
}
