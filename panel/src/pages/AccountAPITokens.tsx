import { useEffect, useRef, useState, type FormEvent } from "react";
import { AlertTriangle, Check, Copy, KeyRound, Loader2, Plus, Trash2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Loading, ErrorState } from "@/components/States";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { InlineError, MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { SegmentedControl } from "@/components/SegmentedControl";
import { api, humanizeError } from "@/lib/api";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import type { APIToken, CreatedAPIToken } from "@/lib/types";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

type Expiry = "7" | "30" | "90" | "365" | "never";

const NORMAL_EXPIRIES: readonly Expiry[] = ["30", "90", "365", "never"];
const ADMIN_EXPIRIES: readonly Expiry[] = ["7", "30", "90"];
const NAME_MAX = 64;
const DAY_MS = 86_400_000;
// A minute short of the day count keeps the longest choices inside the server's limit when this clock runs ahead.
const EXPIRY_MARGIN_MS = 60_000;
const COPIED_MS = 2000;

function expiresAt(expiry: Expiry): string | undefined {
  if (expiry === "never") return undefined;
  return new Date(Date.now() + Number(expiry) * DAY_MS - EXPIRY_MARGIN_MS).toISOString();
}

export function AccountAPITokensCard() {
  const { t, i18n } = useTranslation("account");
  const tokens = useAsync(() => api.listAPITokens(), []);
  const [creating, setCreating] = useState(false);
  const [revokeTarget, setRevokeTarget] = useState<APIToken | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const data = tokens.data;
  const list = data?.tokens ?? [];
  const limit = data?.limit;
  const full = limit !== undefined && list.length >= limit;
  const now = Date.now();
  const locale = i18n.language;

  async function revoke(token: APIToken) {
    try {
      await api.revokeAPIToken(token.id);
      setMessage(t("api_token_revoked", { name: token.name }));
    } finally {
      tokens.reload();
    }
  }

  return (
    <Card>
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="h-4 w-4 text-primary" /> {t("api_tokens_title")}
        </CardTitle>
        <Button
          size="sm"
          variant="outline"
          disabled={!data?.enabled || full}
          onClick={() => {
            setMessage(null);
            setCreating(true);
          }}
        >
          <Plus />
          {t("api_token_create")}
        </Button>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        <p className="text-muted-foreground">{t("api_tokens_desc")}</p>
        {data && !data.enabled && (
          <p className="rounded-md border bg-muted/40 p-3 text-muted-foreground">{t("api_tokens_off")}</p>
        )}
        {message && <MessageLine kind="success" message={message} />}
        {tokens.loading && !data ? (
          <Loading label={t("api_tokens_loading")} />
        ) : tokens.error && !data ? (
          <ErrorState error={tokens.error} onRetry={tokens.reload} />
        ) : list.length === 0 ? (
          <p className="py-2 text-xs italic text-muted-foreground">{t("api_tokens_empty")}</p>
        ) : (
          <ul className="divide-y rounded-md border bg-background/50">
            {list.map((token) => (
              <li key={token.id} className="flex items-center justify-between gap-3 p-3">
                <div className="min-w-0 space-y-1">
                  <p className="flex flex-wrap items-center gap-2 font-medium text-foreground">
                    <span className="truncate" title={token.name}>
                      {token.name}
                    </span>
                    {token.admin && <Badge variant="warning">{t("api_token_admin_badge")}</Badge>}
                  </p>
                  <p className="break-all font-mono text-xs text-muted-foreground">{`${token.prefix}…`}</p>
                  <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                    <span title={formatAbsolute(token.created_at, locale)}>
                      {t("api_token_created", { when: formatRelative(token.created_at, now, locale) })}
                    </span>
                    {token.expires_at ? (
                      <span title={formatAbsolute(token.expires_at, locale)}>
                        {t("api_token_expires", { when: formatRelative(token.expires_at, now, locale) })}
                      </span>
                    ) : (
                      <span>{t("api_token_no_expiry")}</span>
                    )}
                    {token.last_used_at ? (
                      <span title={formatAbsolute(token.last_used_at, locale)}>
                        {t("api_token_last_used", { when: formatRelative(token.last_used_at, now, locale) })}
                      </span>
                    ) : (
                      <span>{t("api_token_never_used")}</span>
                    )}
                  </div>
                </div>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    setMessage(null);
                    setRevokeTarget(token);
                  }}
                  aria-label={t("api_token_revoke_aria", { name: token.name })}
                  className="shrink-0 text-muted-foreground hover:text-destructive"
                >
                  <Trash2 className="h-4 w-4" />
                  <span className="hidden sm:inline">{t("api_token_revoke")}</span>
                </Button>
              </li>
            ))}
          </ul>
        )}
        {full && <p className="text-xs text-muted-foreground">{t("api_tokens_limit_reached", { limit })}</p>}
      </CardContent>
      {revokeTarget && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open) setRevokeTarget(null);
          }}
          title={t("api_token_revoke_title", { name: revokeTarget.name })}
          description={t("api_token_revoke_desc")}
          confirmLabel={t("api_token_revoke")}
          onConfirm={() => revoke(revokeTarget)}
        />
      )}
      {creating && (
        <CreateTokenDialog
          canCreateAdmin={data?.can_create_admin === true}
          onClose={(created) => {
            setCreating(false);
            if (created) tokens.reload();
          }}
        />
      )}
    </Card>
  );
}

function CreateTokenDialog({ canCreateAdmin, onClose }: { canCreateAdmin: boolean; onClose: (created: boolean) => void }) {
  const { t } = useTranslation("account");
  const reauth = useReauth();
  const [name, setName] = useState("");
  const [admin, setAdmin] = useState(false);
  const [expiry, setExpiry] = useState<Expiry>("90");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<CreatedAPIToken | null>(null);

  const length = [...name.trim()].length;
  const tooLong = length > NAME_MAX;
  const options = admin ? ADMIN_EXPIRIES : NORMAL_EXPIRIES;

  function chooseAdmin(next: boolean) {
    setAdmin(next);
    setExpiry(next ? "30" : "90");
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (length === 0 || tooLong || submitting) return;
    setSubmitting(true);
    setError(null);
    const body = { name: name.trim(), expires_at: expiresAt(expiry), admin: admin || undefined };
    try {
      setCreated(await reauth.guard(() => api.createAPIToken(body)));
    } catch (err) {
      if (!isReauthCancelled(err)) setError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open && !created && !submitting) onClose(false);
        }}
      >
        <DialogContent hideClose={created !== null || submitting}>
          {created ? (
            <CreatedKey created={created} onDone={() => onClose(true)} />
          ) : (
            <form onSubmit={(e) => void submit(e)} className="grid gap-4">
              <DialogHeader>
                <DialogTitle>{t("api_token_create_title")}</DialogTitle>
                <DialogDescription>{t("api_token_create_desc")}</DialogDescription>
              </DialogHeader>
              <div className="grid gap-2">
                <Label htmlFor="api-token-name">{t("api_token_name")}</Label>
                <Input
                  id="api-token-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder={t("api_token_name_placeholder")}
                  autoComplete="off"
                  spellCheck={false}
                  disabled={submitting}
                  aria-invalid={tooLong}
                />
                <p className={tooLong ? "text-xs text-destructive" : "text-xs text-muted-foreground"}>
                  {t("api_token_name_hint")}
                </p>
              </div>
              <div className="grid gap-2">
                <span className="text-sm font-medium leading-none">{t("api_token_expiry")}</span>
                <SegmentedControl
                  label={t("api_token_expiry")}
                  value={expiry}
                  onChange={setExpiry}
                  segments={options.map((key) => ({ key, label: t(`api_token_expiry_${key}`) }))}
                />
              </div>
              {canCreateAdmin && (
                <div className="grid gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3">
                  <label htmlFor="api-token-admin" className="flex cursor-pointer items-center gap-2 text-sm font-medium">
                    <Checkbox
                      id="api-token-admin"
                      checked={admin}
                      disabled={submitting}
                      onChange={(e) => chooseAdmin(e.target.checked)}
                    />
                    {t("api_token_admin")}
                  </label>
                  <p className="text-xs leading-relaxed text-muted-foreground">{t("api_token_admin_risk")}</p>
                </div>
              )}
              <InlineError message={error} />
              <DialogFooter>
                <Button type="button" variant="outline" onClick={() => onClose(false)} disabled={submitting}>
                  {t("common:cancel")}
                </Button>
                <Button type="submit" disabled={length === 0 || tooLong || submitting}>
                  {submitting && <Loader2 className="animate-spin" />}
                  {t("api_token_create_submit")}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
      {reauth.dialog}
    </>
  );
}

function CreatedKey({ created, onDone }: { created: CreatedAPIToken; onDone: () => void }) {
  const { t } = useTranslation("account");
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy() {
    window.clearTimeout(timer.current);
    try {
      await navigator.clipboard.writeText(created.token);
      setCopyFailed(false);
      setCopied(true);
      timer.current = window.setTimeout(() => setCopied(false), COPIED_MS);
    } catch {
      setCopied(false);
      setCopyFailed(true);
    }
  }

  return (
    <div className="grid gap-4">
      <DialogHeader>
        <DialogTitle>{t("api_token_created_title")}</DialogTitle>
        <DialogDescription>{t("api_token_created_desc", { name: created.name })}</DialogDescription>
      </DialogHeader>
      <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-sm">
        <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-600 dark:text-amber-300" />
        <p>{t("api_token_created_warning")}</p>
      </div>
      <code className="block select-all break-all rounded-md bg-muted px-3 py-2 font-mono text-xs text-foreground">
        {created.token}
      </code>
      <InlineError message={copyFailed ? t("api_token_copy_failed") : null} />
      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => void copy()} aria-live="polite">
          {copied ? <Check className="text-emerald-500" /> : <Copy />}
          {t(copied ? "api_token_copied" : "api_token_copy")}
        </Button>
        <Button type="button" onClick={onDone}>
          {t("api_token_saved")}
        </Button>
      </DialogFooter>
    </div>
  );
}
