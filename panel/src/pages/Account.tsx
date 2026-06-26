import { useState, type FormEvent } from "react";
import { CheckCircle2, Link2, ShieldCheck } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Loading, ErrorState } from "@/components/States";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";

// The Account page is the web half of the §10 link flow. A code is born in-game
// (online-mode auth proves the UUID) and consumed here (the session proves the
// user) — so this page only ever reports status and redeems a code; it can never
// originate a binding. The Minecraft-link card is a small state machine: checking
// → linked, or → the two-step "get a code in-game, enter it here" form.

export function Account() {
  const status = useAsync(() => api.linkStatus(), []);

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
    <div className="mx-auto max-w-4xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Account</h1>
        <p className="text-sm text-muted-foreground">
          Identity and Minecraft linking.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldCheck className="h-4 w-4 text-primary" /> Session
          </CardTitle>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          You reached this panel through the platform's identity proxy (Zero-Trust /
          Access). The panel itself holds no credentials — every request rides your
          existing session cookie (spec §7/§8).
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Link2 className="h-4 w-4 text-primary" /> Minecraft link
          </CardTitle>
        </CardHeader>
        <CardContent className="text-sm">
          {linked ? (
            <LinkedState uuid={verifiedUUID} />
          ) : status.loading && !status.data ? (
            <Loading label="Checking link status…" />
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
    </div>
  );
}

/** LinkedState confirms the binding. The UUID is shown only when this session
 *  just verified it (start does not return it), so a pre-existing link renders
 *  the confirmation without a UUID rather than inventing one. */
function LinkedState({ uuid }: { uuid: string | null }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2 font-medium text-foreground">
        <CheckCircle2 className="h-4 w-4 text-emerald-500" />
        Your Minecraft account is linked.
      </div>
      <p className="text-muted-foreground">
        Linked sessions can claim and own servers — ownership operations (claim,
        wake, stop) are unblocked.
      </p>
      {uuid && (
        <div className="flex items-center gap-2 text-muted-foreground">
          <span className="text-xs uppercase tracking-wide">UUID</span>
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
  return (
    <ol className="space-y-4">
      <li className="flex gap-3">
        <StepBadge n={1} />
        <div className="space-y-1">
          <p className="font-medium text-foreground">Get a code in-game</p>
          <p className="text-muted-foreground">
            Join any server and run{" "}
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
              /link
            </code>{" "}
            in chat. The server already knows your verified identity, so it hands
            you a one-time code (valid about 10 minutes).
          </p>
        </div>
      </li>
      <li className="flex gap-3">
        <StepBadge n={2} />
        <div className="w-full space-y-2">
          <p className="font-medium text-foreground">Enter it here</p>
          <form onSubmit={onSubmit} className="space-y-2">
            <Label htmlFor="link-code">Link code</Label>
            <div className="flex gap-2">
              <Input
                id="link-code"
                value={code}
                onChange={(e) => setCode(e.target.value.toUpperCase())}
                placeholder="ABCD2345"
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
                {submitting ? "Verifying…" : "Link"}
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
