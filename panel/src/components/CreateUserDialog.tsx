import { useState } from "react";
import { Plus, Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";

interface Props {
  onCreated: (id: string) => void;
}

// Passwordless create (spec §B): the account is minted with no credential at all.
// The new user signs in with an in-game /link bind code (or email-OTP / passkey
// once their address is verified), so there is nothing to hand over here — on
// success we just jump to the new user's detail page.
export function CreateUserDialog({ onCreated }: Props) {
  const { t } = useTranslation("admin");
  const [open, setOpen] = useState(false);
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<"user" | "admin">("user");
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  function reset() {
    setUsername("");
    setEmail("");
    setRole("user");
    setErr(null);
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitting) return;
    setErr(null);

    if (!username.trim()) {
      setErr(t("users_create_validation_username"));
      return;
    }

    setSubmitting(true);
    try {
      const u = await api.createUser({
        username: username.trim(),
        email: email.trim() || undefined,
        role,
      });
      setOpen(false);
      reset();
      onCreated(u.id);
    } catch (e: any) {
      if (e && e.code === "already_exists") {
        setErr(t("users_create_validation_username_taken"));
      } else {
        setErr(humanizeError(e));
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(v) => { setOpen(v); if (!v) reset(); }}>
      <DialogTrigger asChild>
        <Button size="sm" className="gap-1.5">
          <Plus className="h-4 w-4" />
          {t("users_create_btn")}
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-md" hideClose={submitting}>
        <DialogHeader>
          <DialogTitle>{t("users_create_title")}</DialogTitle>
          <DialogDescription>{t("users_create_desc")}</DialogDescription>
        </DialogHeader>

        <form onSubmit={handleSubmit} className="space-y-4">
          {/* Username */}
          <div className="space-y-1.5">
            <Label htmlFor="create-user-username" className="text-xs font-semibold text-muted-foreground">
              {t("users_field_username")} *
            </Label>
            <Input
              id="create-user-username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder={t("users_create_username_placeholder")}
              className="h-9 text-sm"
              autoFocus
            />
          </div>

          {/* Email */}
          <div className="space-y-1.5">
            <Label htmlFor="create-user-email" className="text-xs font-semibold text-muted-foreground">
              {t("users_field_email")}
            </Label>
            <Input
              id="create-user-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="user@example.com"
              className="h-9 text-sm"
            />
          </div>

          {/* Role */}
          <div className="space-y-1.5">
            <Label htmlFor="create-user-role" className="text-xs font-semibold text-muted-foreground">
              {t("users_field_role")}
            </Label>
            <Select value={role} onValueChange={(v: "user" | "admin") => setRole(v)}>
              <SelectTrigger id="create-user-role" className="h-9 text-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="user">{t("users_role_user")}</SelectItem>
                <SelectItem value="admin">{t("users_role_admin")}</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {err && <MessageLine kind="error" message={err} />}

          <DialogFooter>
            <Button type="submit" disabled={submitting} className="gap-1.5">
              {submitting ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Plus className="h-4 w-4" />
              )}
              {t("users_create_btn")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
