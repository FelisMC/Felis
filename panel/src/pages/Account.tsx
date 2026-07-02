import { useState, useRef, useEffect, type FormEvent } from "react";
import { CheckCircle2, Link2, LogOut, ShieldCheck, UserRound, Mail, Fingerprint, Trash2, KeyRound } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Loading, ErrorState } from "@/components/States";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
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
  const { t } = useTranslation("account");

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
  const [deletingMap, setDeletingMap] = useState<Record<string, boolean>>({});

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
          transports: typeof response.getTransports === "function" ? response.getTransports() : [],
        },
      };

      await api.passkeyRegisterFinish(name, attestation);
      await passkeys.reload();
      setRegisterDialogOpen(false);
      setPasskeyNickname("");
    } catch (err: any) {
      if (err.name === "NotAllowedError") {
        setPasskeyError("操作已被用户或浏览器取消。");
      } else if (err.name === "AbortError") {
        setPasskeyError("注册已被取消。");
      } else {
        setPasskeyError(humanizeError(err));
      }
    } finally {
      setRegisteringPasskey(false);
      abortControllerRef.current = null;
    }
  }

  async function handleDeletePasskey(id: string) {
    if (deletingMap[id]) return;
    setDeletingMap((prev) => ({ ...prev, [id]: true }));
    try {
      await api.passkeyDelete(id);
      await passkeys.reload();
    } catch (err) {
      alert(humanizeError(err));
    } finally {
      setDeletingMap((prev) => ({ ...prev, [id]: false }));
    }
  }

  // Sign-out ends a local-password session: clear it server-side, then refresh /me.
  // For a local session that read now 401s → the tier model flips to
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
      <div className="flex items-center gap-3">
        <UserRound className="h-6 w-6 text-primary" />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t("title")}</h1>
          <p className="text-sm text-muted-foreground">
            {t("subtitle")}
          </p>
        </div>
      </div>

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
                        修改邮箱
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
              {emailError && <p className="text-sm text-destructive ml-9">{emailError}</p>}
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
                  {passkeyError && <p className="text-sm text-destructive">{passkeyError}</p>}
                </div>
                <DialogFooter>
                  <Button
                    type="button"
                    variant="ghost"
                    onClick={cancelRegistration}
                    disabled={registeringPasskey}
                  >
                    取消
                  </Button>
                  <Button type="submit" disabled={registeringPasskey || !passkeyNickname.trim()}>
                    {registeringPasskey ? t("registering_passkey") : "继续"}
                  </Button>
                </DialogFooter>
              </form>
            </DialogContent>
          </Dialog>
        </CardHeader>
        <CardContent className="text-sm space-y-4">
          <p className="text-muted-foreground">{t("passkeys_desc")}</p>
          {passkeys.loading && !passkeys.data ? (
            <Loading label="加载 Passkey 列表中..." />
          ) : passkeys.error ? (
            <ErrorState error={passkeys.error} onRetry={passkeys.reload} />
          ) : !passkeys.data?.credentials || passkeys.data.credentials.length === 0 ? (
            <p className="text-xs text-muted-foreground py-2 italic">{t("no_passkeys")}</p>
          ) : (
            <div className="border rounded-md divide-y bg-background/50">
              {passkeys.data.credentials.map((cred: any) => (
                <div key={cred.id} className="flex items-center justify-between p-3">
                  <div className="space-y-1">
                    <p className="font-medium text-foreground flex items-center gap-1.5">
                      <KeyRound className="h-3.5 w-3.5 text-muted-foreground" />
                      {cred.name}
                    </p>
                    <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                      <span>
                        {t("created_at")}
                        {new Date(cred.created_at).toLocaleString()}
                      </span>
                      <span>
                        {t("last_used")}
                        {cred.last_used_at ? new Date(cred.last_used_at).toLocaleString() : t("never")}
                      </span>
                    </div>
                  </div>
                  <Button
                    size="icon"
                    variant="ghost"
                    onClick={() => handleDeletePasskey(cred.id)}
                    disabled={deletingMap[cred.id]}
                    className="text-muted-foreground hover:text-destructive"
                  >
                    <Trash2 className="h-4 w-4" />
                  </Button>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

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
            {error && <p className="text-sm text-destructive">{error}</p>}
          </form>
        </div>
      </li>
    </ol>
  );
}

function StepBadge({ n }: { n: number }) {
  return (
    <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-xs font-semibold text-primary">
      {n}
    </span>
  );
}

function base64urlToBytes(str: string): ArrayBuffer {
  let base64 = str.replace(/-/g, "+").replace(/_/g, "/");
  const pad = base64.length % 4;
  if (pad) {
    base64 += "=".repeat(4 - pad);
  }
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes.buffer;
}

function bytesToBase64url(bytes: ArrayBuffer): string {
  let binary = "";
  const uint8 = new Uint8Array(bytes);
  const len = uint8.byteLength;
  for (let i = 0; i < len; i++) {
    binary += String.fromCharCode(uint8[i]);
  }
  const base64 = btoa(binary);
  return base64
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}
