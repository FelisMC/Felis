import { useState } from "react";
import { Plus, Loader2, Copy, Check } from "lucide-react";
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
import { api, humanizeError } from "@/lib/api";

interface Props {
  onCreated: (id: string) => void;
}

function generateRandomPassword(length = 16): string {
  const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$";
  let password = "";
  for (let i = 0; i < length; i++) {
    password += chars.charAt(Math.floor(Math.random() * chars.length));
  }
  return password;
}

export function CreateUserDialog({ onCreated }: Props) {
  const { t } = useTranslation("admin");
  const [open, setOpen] = useState(false);
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<"user" | "admin">("user");
  const [mustChange, setMustChange] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Success state fields
  const [createdUser, setCreatedUser] = useState<any | null>(null);
  const [generatedPassword, setGeneratedPassword] = useState("");
  const [copied, setCopied] = useState(false);

  function reset() {
    setUsername("");
    setEmail("");
    setRole("user");
    setMustChange(true);
    setErr(null);
    setCreatedUser(null);
    setGeneratedPassword("");
    setCopied(false);
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitting) return;
    setErr(null);

    if (!username.trim()) {
      setErr(t("users_create_validation_username"));
      return;
    }

    const genPassword = generateRandomPassword();
    setSubmitting(true);
    try {
      const u = await api.createUser({
        username: username.trim(),
        email: email.trim() || undefined,
        role,
        password: genPassword,
        must_change_password: mustChange,
      });
      setGeneratedPassword(genPassword);
      setCreatedUser(u);
    } catch (e: any) {
      if (e && e.code === "already_exists") {
        setErr(t("users_create_validation_username_taken") || "该用户名已被使用。");
      } else {
        setErr(humanizeError(e));
      }
    } finally {
      setSubmitting(false);
    }
  }

  const handleCopy = async () => {
    if (!createdUser) return;
    const text = `Username: ${createdUser.username}\nPassword: ${generatedPassword}`;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch (e) {
      // ignore
    }
  };

  const handleDone = () => {
    const id = createdUser?.id;
    setOpen(false);
    reset();
    if (id) {
      onCreated(id);
    }
  };

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
          <DialogTitle>{createdUser ? t("users_create_success_title") || "创建成功" : t("users_create_title")}</DialogTitle>
          <DialogDescription>
            {createdUser
              ? t("users_create_success_desc") || "请务必复制并妥善保管该用户的初始凭据，关闭后密码将不再显示。"
              : t("users_create_desc")}
          </DialogDescription>
        </DialogHeader>

        {createdUser ? (
          <div className="space-y-4">
            <div className="rounded-md border border-border/50 bg-muted/20 p-4 space-y-3">
              <div className="space-y-1">
                <Label className="text-xs font-semibold text-muted-foreground">{t("users_field_username")}</Label>
                <div className="font-mono text-sm font-semibold select-all">{createdUser.username}</div>
              </div>
              <div className="space-y-1">
                <Label className="text-xs font-semibold text-muted-foreground">{t("users_field_password")}</Label>
                <div className="font-mono text-sm font-semibold text-emerald-600 dark:text-emerald-400 select-all">
                  {generatedPassword}
                </div>
              </div>
            </div>

            <DialogFooter className="flex flex-row justify-end gap-2">
              <Button type="button" variant="outline" onClick={handleCopy} className="gap-1.5">
                {copied ? <Check className="h-4 w-4 text-emerald-500" /> : <Copy className="h-4 w-4" />}
                {copied ? t("common:copied") || "已复制" : t("common:copy") || "复制凭据"}
              </Button>
              <Button type="button" onClick={handleDone}>
                {t("common:done") || "完成"}
              </Button>
            </DialogFooter>
          </div>
        ) : (
          <form onSubmit={handleSubmit} className="space-y-4">
            {/* Username */}
            <div className="space-y-1.5">
              <Label className="text-xs font-semibold text-muted-foreground">
                {t("users_field_username")} *
              </Label>
              <Input
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder={t("users_create_username_placeholder")}
                className="h-9 text-sm"
                autoFocus
              />
            </div>

            {/* Email */}
            <div className="space-y-1.5">
              <Label className="text-xs font-semibold text-muted-foreground">
                {t("users_field_email")}
              </Label>
              <Input
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="user@example.com"
                className="h-9 text-sm"
              />
            </div>

            {/* Role */}
            <div className="space-y-1.5">
              <Label className="text-xs font-semibold text-muted-foreground">
                {t("users_field_role")}
              </Label>
              <Select value={role} onValueChange={(v: "user" | "admin") => setRole(v)}>
                <SelectTrigger className="h-9 text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="user">{t("users_role_user")}</SelectItem>
                  <SelectItem value="admin">{t("users_role_admin")}</SelectItem>
                </SelectContent>
              </Select>
            </div>

            {/* Must change password toggle */}
            <label className="flex items-center gap-2 cursor-pointer select-none">
              <input
                type="checkbox"
                checked={mustChange}
                onChange={(e) => setMustChange(e.target.checked)}
                className="h-4 w-4 rounded border-border"
              />
              <span className="text-sm text-foreground">
                {t("users_create_must_change")}
              </span>
            </label>

            {err && (
              <p className="rounded-md border border-destructive/20 bg-destructive/10 p-3 text-sm text-destructive">
                {err}
              </p>
            )}

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
        )}
      </DialogContent>
    </Dialog>
  );
}