import { useState, type ChangeEvent, type FormEvent } from "react";
import { ScrollText } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AuditTable } from "@/components/AuditTable";
import { MessageLine } from "@/components/MessageLine";
import { PageHeader } from "@/components/PageHeader";
import { EmptyState, ErrorState, Loading } from "@/components/States";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, humanizeError } from "@/lib/api";
import { formatAbsolute } from "@/lib/format";
import { useAsync } from "@/lib/hooks";
import type { AuditEntry, AuditListParams, AuditPage as AuditPageData } from "@/lib/types";

const PAGE_SIZE = 50;

interface Filters {
  actor: string;
  action: string;
  server: string;
  since: string;
  until: string;
  q: string;
}

const NO_FILTERS: Filters = { actor: "", action: "", server: "", since: "", until: "", q: "" };

const FIELDS: { key: keyof Filters; type: "text" | "datetime-local" }[] = [
  { key: "actor", type: "text" },
  { key: "action", type: "text" },
  { key: "server", type: "text" },
  { key: "since", type: "datetime-local" },
  { key: "until", type: "datetime-local" },
  { key: "q", type: "text" },
];

// A datetime-local value is a wall-clock time in the browser's zone.
function toInstant(value: string): string | undefined {
  const date = new Date(value);
  return value && !Number.isNaN(date.getTime()) ? date.toISOString() : undefined;
}

function listParams(f: Filters): AuditListParams {
  return {
    actor: f.actor.trim() || undefined,
    action: f.action.trim() || undefined,
    server: f.server.trim() || undefined,
    since: toInstant(f.since),
    until: toInstant(f.until),
    q: f.q.trim() || undefined,
    limit: PAGE_SIZE,
  };
}

export function AuditPage() {
  const { t } = useTranslation("admin");
  const [draft, setDraft] = useState(NO_FILTERS);
  const [applied, setApplied] = useState(NO_FILTERS);
  // Each submit restarts the results from the first page, even with unchanged filters.
  const [run, setRun] = useState(0);
  const [opened, setOpened] = useState<AuditEntry | null>(null);

  const ordered = !draft.since || !draft.until || new Date(draft.until) > new Date(draft.since);

  const apply = (filters: Filters) => {
    setApplied(filters);
    setRun((n) => n + 1);
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (ordered) apply(draft);
  };
  const reset = () => {
    setDraft(NO_FILTERS);
    apply(NO_FILTERS);
  };
  const edit = (key: keyof Filters) => (e: ChangeEvent<HTMLInputElement>) =>
    setDraft((d) => ({ ...d, [key]: e.target.value }));

  return (
    <div className="space-y-6">
      <PageHeader icon={ScrollText} title={t("audit_title")} subtitle={t("audit_subtitle")} />

      <Card>
        <CardContent className="space-y-3 p-4">
          <form onSubmit={submit} className="space-y-3">
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
              {FIELDS.map(({ key, type }) => (
                <div key={key} className="space-y-1.5">
                  <Label htmlFor={`audit-filter-${key}`}>{t(`audit_filter_${key}`)}</Label>
                  <Input
                    id={`audit-filter-${key}`}
                    type={type}
                    value={draft[key]}
                    onChange={edit(key)}
                    placeholder={type === "text" ? t(`audit_filter_${key}_placeholder`) : undefined}
                    className="text-sm"
                  />
                </div>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">{t("audit_filter_hint")}</p>
            {!ordered && <MessageLine kind="error" message={t("audit_filter_order")} />}
            <div className="flex flex-wrap gap-2">
              <Button type="submit" disabled={!ordered}>
                {t("audit_filter_apply")}
              </Button>
              <Button type="button" variant="outline" onClick={reset}>
                {t("audit_filter_reset")}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      <AuditResults key={run} filters={applied} onOpen={setOpened} />
      <AuditDetailDialog entry={opened} onClose={() => setOpened(null)} />
    </div>
  );
}

function AuditResults({ filters, onOpen }: { filters: Filters; onOpen: (entry: AuditEntry) => void }) {
  const { t } = useTranslation("admin");
  const first = useAsync(() => api.listAudit(listParams(filters)), [filters]);
  const [later, setLater] = useState<AuditPageData[]>([]);
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreError, setMoreError] = useState<unknown>(null);

  if (!first.data) {
    return first.error ? <ErrorState error={first.error} onRetry={first.reload} /> : <Loading />;
  }
  const entries = [first.data, ...later].flatMap((page) => page.entries);
  const next = (later.at(-1) ?? first.data).next_cursor;
  if (entries.length === 0) {
    return <EmptyState title={t("audit_empty_title")} hint={t("audit_empty_hint")} />;
  }

  const loadMore = async () => {
    setLoadingMore(true);
    setMoreError(null);
    try {
      const page = await api.listAudit({ ...listParams(filters), cursor: next });
      setLater((pages) => [...pages, page]);
    } catch (e) {
      setMoreError(e);
    } finally {
      setLoadingMore(false);
    }
  };

  return (
    <>
      <Card>
        <CardContent className="p-0">
          <div className="overflow-x-auto">
            <AuditTable entries={entries} onOpen={onOpen} />
          </div>
        </CardContent>
      </Card>
      {moreError != null && <MessageLine kind="error" message={humanizeError(moreError)} />}
      <div className="flex justify-center">
        {next ? (
          <Button variant="outline" onClick={loadMore} disabled={loadingMore}>
            {loadingMore ? t("audit_loading_more") : t("audit_load_more")}
          </Button>
        ) : (
          <p className="text-xs text-muted-foreground">{t("audit_end")}</p>
        )}
      </div>
    </>
  );
}

// Show a value that is not text the way JSON writes it.
function shown(value: unknown): string {
  return typeof value === "string" ? value : JSON.stringify(value);
}

// Pairs of `name_from` and `name_to` keys describe one change.
function payloadChanges(payload: Record<string, unknown> | null): { name: string; from: string; to: string }[] {
  if (!payload) return [];
  return Object.keys(payload)
    .filter((key) => key.length > "_from".length && key.endsWith("_from") && `${key.slice(0, -5)}_to` in payload)
    .map((key) => {
      const name = key.slice(0, -5);
      return { name, from: shown(payload[key]), to: shown(payload[`${name}_to`]) };
    });
}

function payloadFields(payload: Record<string, unknown> | null): string[] {
  const fields = payload?.fields;
  return Array.isArray(fields) ? fields.map(shown) : [];
}

function AuditDetailDialog({ entry, onClose }: { entry: AuditEntry | null; onClose: () => void }) {
  return (
    <Dialog open={entry !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto">
        {entry && <AuditDetail entry={entry} />}
      </DialogContent>
    </Dialog>
  );
}

function AuditDetail({ entry }: { entry: AuditEntry }) {
  const { t, i18n } = useTranslation("admin");
  const changes = payloadChanges(entry.payload);
  const fields = payloadFields(entry.payload);
  const rows: [string, string | number | null][] = [
    ["audit_detail_time", formatAbsolute(entry.created_at, i18n.language)],
    ["audit_detail_actor", entry.actor],
    ["audit_detail_actor_user_id", entry.actor_user_id],
    ["audit_detail_source", entry.source],
    ["audit_detail_action", entry.action],
    ["audit_detail_server", entry.server_name],
    ["audit_detail_request_id", entry.request_id],
    ["audit_detail_client_ip", entry.client_ip],
    ["audit_detail_user_agent", entry.user_agent],
    ["audit_detail_id", entry.id],
  ];
  return (
    <>
      <DialogHeader>
        <DialogTitle>{t("audit_detail_title")}</DialogTitle>
        <DialogDescription>{entry.action}</DialogDescription>
      </DialogHeader>

      {(changes.length > 0 || fields.length > 0) && (
        <div className="space-y-1 rounded-md border border-border bg-muted/20 p-3 text-sm">
          {changes.map((c) => (
            <p key={c.name} className="break-all">
              {t("audit_detail_change", { name: c.name, from: c.from, to: c.to })}
            </p>
          ))}
          {fields.length > 0 && (
            <p className="break-all">{t("audit_detail_fields", { fields: fields.join(t("audit_detail_separator")) })}</p>
          )}
        </div>
      )}

      <dl className="grid grid-cols-[7.5rem_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-sm">
        {rows.map(([label, value]) => (
          <div key={label} className="contents">
            <dt className="text-muted-foreground">{t(label)}</dt>
            <dd className="break-all">{value ?? "—"}</dd>
          </div>
        ))}
      </dl>

      <div className="space-y-1.5">
        <h4 className="text-sm font-medium">{t("audit_detail_payload")}</h4>
        {entry.payload ? (
          <pre className="max-h-64 overflow-auto rounded-md bg-muted/30 p-3 font-mono text-xs">
            {JSON.stringify(entry.payload, null, 2)}
          </pre>
        ) : (
          <p className="text-sm text-muted-foreground">{t("audit_detail_no_payload")}</p>
        )}
      </div>
    </>
  );
}
