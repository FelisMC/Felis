import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Loader2, Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { MessageLine } from "@/components/MessageLine";
import { SegmentedControl } from "@/components/SegmentedControl";
import { parseCap, showCap } from "@/components/SelfServiceQuotaCard";
import { ErrorState, Loading } from "@/components/States";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import type { QuotaView, SelfServiceQuotaSettings } from "@/lib/types";

type Mode = "default" | "custom";

/** The caps as typed: whole servers, GB of memory and storage, CPU cores. */
interface Draft {
  servers: string;
  memoryGB: string;
  cpuCores: string;
  storageGB: string;
}

function draftOf(q: QuotaView): Draft {
  return {
    servers: showCap(q.max_servers),
    memoryGB: showCap(q.max_memory_mb, 1024),
    cpuCores: showCap(q.max_cpu_milli, 1000),
    storageGB: showCap(q.max_storage_gb),
  };
}

// UserQuotaCard is one user's quota. Users without their own quota are held to the default; their own caps win over it, and going back to the default removes them.
export function UserQuotaCard({ userId }: { userId: string }) {
  const { t } = useTranslation("admin");
  const quotas = useAsync(() => api.getUserQuotas(userId), [userId]);
  const defaults = useAsync(api.getSelfServiceQuota, []);
  const [mode, setMode] = useState<Mode>("default");
  const [draft, setDraft] = useState<Draft>({ servers: "", memoryGB: "", cpuCores: "", storageGB: "" });
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  const saved = quotas.data;
  useEffect(() => {
    if (saved) {
      setMode(saved.custom ? "custom" : "default");
      setDraft(draftOf(saved));
    }
  }, [saved]);

  const caps = {
    max_servers: parseCap(draft.servers),
    max_memory_mb: parseCap(draft.memoryGB, 1024),
    max_cpu_milli: parseCap(draft.cpuCores, 1000),
    max_storage_gb: parseCap(draft.storageGB),
  };
  const valid =
    mode === "default" ||
    (Object.values(caps).every((v) => v !== undefined) && (draft.servers.trim() === "" || /^\d+$/.test(draft.servers.trim())));
  const dirty =
    !!saved &&
    (mode !== (saved.custom ? "custom" : "default") ||
      (mode === "custom" && JSON.stringify(draft) !== JSON.stringify(draftOf(saved))));

  function set<K extends keyof Draft>(key: K, value: string) {
    setDraft((d) => ({ ...d, [key]: value }));
    setMessage(null);
  }

  async function save() {
    if (!dirty || !valid || saving) return;
    setSaving(true);
    setMessage(null);
    try {
      if (mode === "default") {
        await api.clearUserQuotas(userId);
        setMessage({ kind: "success", text: t("user_quota_cleared") });
      } else {
        await api.setUserQuotas(userId, caps as Record<keyof typeof caps, number | null>);
        setMessage({ kind: "success", text: t("user_quota_saved") });
      }
      quotas.reload();
    } catch (e) {
      setMessage({ kind: "error", text: humanizeError(e) });
    } finally {
      setSaving(false);
    }
  }

  if (quotas.loading && !saved) return <Loading label={t("common:loading")} />;
  if (quotas.error) {
    return (
      <Card>
        <CardHeader><CardTitle>{t("users_quotas")}</CardTitle></CardHeader>
        <CardContent><ErrorState error={quotas.error} onRetry={quotas.reload} /></CardContent>
      </Card>
    );
  }

  const field = (id: string, key: keyof Draft, label: string) => (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        inputMode="decimal"
        placeholder={t("self_service_unlimited")}
        value={draft[key]}
        disabled={saving}
        onChange={(e) => set(key, e.target.value)}
      />
    </div>
  );

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-3">
        <CardTitle>{t("users_quotas")}</CardTitle>
        <SegmentedControl
          label={t("user_quota_source")}
          value={mode}
          onChange={(next) => { setMode(next); setMessage(null); }}
          segments={[
            { key: "default", label: t("user_quota_mode_default") },
            { key: "custom", label: t("user_quota_mode_custom") },
          ]}
        />
      </CardHeader>
      <CardContent className="space-y-4">
        {mode === "default" ? (
          <div className="space-y-2 text-sm leading-relaxed">
            <DefaultSummary settings={defaults.data} loading={defaults.loading} />
            <p className="text-xs text-muted-foreground">
              {t("user_quota_default_where")}{" "}
              <Link to="/admin/users" className="font-medium text-primary underline-offset-4 hover:underline">
                {t("user_quota_default_link")}
              </Link>
            </p>
          </div>
        ) : (
          <>
            <div className="grid gap-4 sm:grid-cols-2">
              {field("user-quota-servers", "servers", t("self_service_servers"))}
              {field("user-quota-memory", "memoryGB", t("self_service_memory"))}
              {field("user-quota-cpu", "cpuCores", t("self_service_cpu"))}
              {field("user-quota-storage", "storageGB", t("self_service_storage"))}
            </div>
            <p className="text-xs leading-relaxed text-muted-foreground">{t("user_quota_custom_hint")}</p>
            {!valid && <MessageLine kind="error" message={t("self_service_invalid")} />}
          </>
        )}
        {message && <MessageLine kind={message.kind} message={message.text} />}
      </CardContent>
      <CardFooter className="justify-end">
        <Button onClick={() => void save()} disabled={!dirty || !valid || saving} size="sm" className="gap-1.5">
          {saving ? <Loader2 className="h-4 w-4 animate-spin" /> : <Save className="h-4 w-4" />}
          {t("user_quota_save")}
        </Button>
      </CardFooter>
    </Card>
  );
}

function DefaultSummary({ settings, loading }: { settings: SelfServiceQuotaSettings | null | undefined; loading: boolean }) {
  const { t } = useTranslation("admin");
  if (loading && !settings) return <p className="text-muted-foreground">{t("self_service_loading")}</p>;
  if (!settings) return null;
  if (!settings.enabled) return <p>{t("user_quota_default_off")}</p>;
  const cap = (v: number | null, render: (n: number) => string) => (v == null ? t("self_service_unlimited") : render(v));
  return (
    <p>
      {t("user_quota_default_on", {
        caps: t("user_quota_caps", {
          servers: cap(settings.max_servers, (n) => String(n)),
          memory: cap(settings.max_memory_mb, (n) => t("user_quota_gb", { n: n / 1024 })),
          cpu: cap(settings.max_cpu_milli, (n) => t("user_quota_cores", { n: n / 1000 })),
          storage: cap(settings.max_storage_gb, (n) => t("user_quota_gb", { n })),
        }),
      })}
    </p>
  );
}
