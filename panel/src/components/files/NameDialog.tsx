import { useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { MessageLine } from "@/components/MessageLine";
import { humanizeError } from "@/lib/api";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  label: string;
  confirmLabel: string;
  /** What the field starts with. A rename starts from the old name, selected up
   *  to its extension so typing replaces just the name. */
  initial?: string;
  /** Why a name cannot be used, as a sentence, or null when it can. */
  problem: (name: string) => string | null;
  /** Runs the action with the name less surrounding spaces. The dialog closes
   *  when it resolves; a rejection is shown in the dialog, which stays open. */
  onSubmit: (name: string) => Promise<void>;
}

// NameDialog asks for one file or folder name. The problem with a name shows as
// soon as something has been typed, so the button is never disabled without a
// reason on screen; the server's own refusal lands in the same place. Mount it
// afresh for each question (a key per opening): the field starts from `initial`.
export function NameDialog({
  open,
  onOpenChange,
  title,
  description,
  label,
  confirmLabel,
  initial = "",
  problem,
  onSubmit,
}: Props) {
  const { t } = useTranslation("common");
  const [value, setValue] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const issue = problem(value);
  const unchanged = initial !== "" && value.trim() === initial;
  // An untouched empty field has nothing wrong with it yet; saying so before the
  // first keystroke would greet the dialog with an error.
  const shownIssue = value === "" && initial === "" ? null : issue;

  function setOpen(next: boolean) {
    if (busy) return;
    onOpenChange(next);
  }

  // The submit button is disabled whenever the name cannot be sent, and a form
  // whose default button is disabled does not submit on Enter either.
  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onSubmit(value.trim());
      setBusy(false);
      onOpenChange(false);
    } catch (err) {
      setError(humanizeError(err));
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          const input = inputRef.current;
          if (!input) return;
          input.focus();
          const dot = initial.lastIndexOf(".");
          input.setSelectionRange(0, dot > 0 ? dot : initial.length);
        }}
      >
        <form onSubmit={(e) => void submit(e)} className="grid gap-4">
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            {description && <DialogDescription>{description}</DialogDescription>}
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="file-name">{label}</Label>
            <Input
              id="file-name"
              ref={inputRef}
              value={value}
              onChange={(e) => {
                setValue(e.target.value);
                setError(null);
              }}
              disabled={busy}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={shownIssue !== null}
              aria-describedby={shownIssue ? "file-name-problem" : undefined}
              className="font-mono"
            />
            {shownIssue && (
              <p id="file-name-problem" className="text-xs text-destructive">
                {shownIssue}
              </p>
            )}
          </div>
          {error && <MessageLine kind="error" message={error} compact />}
          <DialogFooter>
            <Button type="button" variant="outline" size="sm" onClick={() => setOpen(false)} disabled={busy}>
              {t("cancel")}
            </Button>
            <Button type="submit" size="sm" disabled={busy || issue !== null || unchanged}>
              {busy && <Loader2 className="h-4 w-4 animate-spin" />}
              {confirmLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
