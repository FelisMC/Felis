import { useState } from "react";
import { Plus } from "lucide-react";
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
import { useAsync } from "@/lib/hooks";
import { hostFor, type RuntimeConfig } from "@/lib/config";
import type { AutostartPolicy, CreateServerRequest } from "@/lib/types";

// CreateServerDialog IS the §15 收口 ("structured form, not free YAML"): every
// dangerous knob (hostNetwork, hostPath, arbitrary image, raw resource quantities)
// is simply absent from the UI. The image is chosen from the server-side whitelist
// (GET /images); memory/storage are constrained text the API re-validates. Nothing
// here can express a privileged pod — the form's value space IS the allowlist.

const MEMORY_OPTIONS = ["2Gi", "4Gi", "6Gi", "8Gi"];
const STORAGE_OPTIONS = ["5Gi", "10Gi", "20Gi", "50Gi"];
const POLICIES: { value: AutostartPolicy; label: string }[] = [
  { value: "ownerOnly", label: "Owner only — wake from the panel" },
  { value: "public", label: "Public — any player join wakes it" },
  { value: "allowlist", label: "Allowlist — listed players wake it" },
];

const SUBDOMAIN_RE = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/;

interface Props {
  cfg: RuntimeConfig;
  onCreated: () => void;
}

export function CreateServerDialog({ cfg, onCreated }: Props) {
  const [open, setOpen] = useState(false);
  const images = useAsync(() => api.listImages(), []);

  const [form, setForm] = useState<CreateServerRequest>({
    name: "",
    subdomain: "",
    displayName: "",
    image: "",
    memory: "4Gi",
    storage: "10Gi",
    autostartPolicy: "ownerOnly",
  });
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  function set<K extends keyof CreateServerRequest>(k: K, v: CreateServerRequest[K]) {
    setForm((f) => ({ ...f, [k]: v }));
  }

  const enabledImages = (images.data ?? []).filter((i) => i.enabled);
  const subdomainValid = SUBDOMAIN_RE.test(form.subdomain);
  const canSubmit =
    !!form.name &&
    subdomainValid &&
    !!form.image &&
    !!form.memory &&
    !!form.storage &&
    !submitting;

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const payload: CreateServerRequest = {
        ...form,
        displayName: form.displayName?.trim() || undefined,
      };
      await api.createServer(payload);
      setOpen(false);
      onCreated();
      setForm((f) => ({ ...f, name: "", subdomain: "", displayName: "" }));
    } catch (e) {
      setError(humanizeError(e));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button>
          <Plus /> New server
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create a server</DialogTitle>
          <DialogDescription>
            Pick from whitelisted images and sizes — the platform provisions the
            rest. No raw cluster config is exposed here.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="cs-name">Name</Label>
            <Input
              id="cs-name"
              placeholder="survival"
              value={form.name}
              onChange={(e) => set("name", e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="cs-sub">Subdomain</Label>
            <Input
              id="cs-sub"
              placeholder="survival"
              value={form.subdomain}
              onChange={(e) => set("subdomain", e.target.value.toLowerCase())}
            />
            <p className="text-xs text-muted-foreground">
              {form.subdomain && !subdomainValid
                ? "Lowercase letters, digits and hyphens only."
                : `Will be reachable at ${hostFor(form.subdomain || "name", cfg)}`}
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="cs-display">Display name (optional)</Label>
            <Input
              id="cs-display"
              placeholder="Survival World"
              value={form.displayName ?? ""}
              onChange={(e) => set("displayName", e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label>Image</Label>
            <Select value={form.image} onValueChange={(v) => set("image", v)}>
              <SelectTrigger>
                <SelectValue
                  placeholder={
                    images.loading
                      ? "Loading images…"
                      : enabledImages.length
                        ? "Choose an image"
                        : "No whitelisted images"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {enabledImages.map((img) => (
                  <SelectItem key={img.image_ref} value={img.image_ref}>
                    {img.image_ref}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label>Memory</Label>
              <Select value={form.memory} onValueChange={(v) => set("memory", v)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MEMORY_OPTIONS.map((m) => (
                    <SelectItem key={m} value={m}>
                      {m}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label>Storage</Label>
              <Select value={form.storage} onValueChange={(v) => set("storage", v)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {STORAGE_OPTIONS.map((s) => (
                    <SelectItem key={s} value={s}>
                      {s}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2">
            <Label>Autostart policy</Label>
            <Select
              value={form.autostartPolicy}
              onValueChange={(v) => set("autostartPolicy", v as AutostartPolicy)}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {POLICIES.map((p) => (
                  <SelectItem key={p.value} value={p.value}>
                    {p.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {error && <p className="text-sm text-destructive">{error}</p>}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => setOpen(false)} disabled={submitting}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={!canSubmit}>
            {submitting ? "Creating…" : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
