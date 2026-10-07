import { Checkbox } from "@/components/ui/checkbox";
import { useId, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import {
  AlertTriangle,
  Archive,
  CalendarClock,
  CheckCircle2,
  Clock,
  Globe,
  Info,
  Loader2,
  Megaphone,
  MinusCircle,
  Pencil,
  Play,
  Plus,
  Power,
  PowerOff,
  RotateCw,
  Terminal,
  Trash2,
  XCircle,
  type LucideIcon,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { BackLink } from "@/components/BackLink";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { ConfirmFooter } from "@/components/ConfirmFooter";
import { InlineError, MessageLine } from "@/components/MessageLine";
import { PhaseBadge, shownPhase, startFailure } from "@/components/PhaseBadge";
import { EmptyState, ErrorState, Loading, NotYours, RefreshError } from "@/components/States";
import { PageHeader } from "@/components/PageHeader";
import { api, humanizeError } from "@/lib/api";
import { useAsync, usePolling, STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage, ownershipPending } from "@/lib/ownership";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";
import type {
  Schedule,
  ScheduleAction,
  ScheduleEveryMinutes,
  ScheduleInput,
  ScheduleResult,
  ScheduleWarnMinutes,
} from "@/lib/types";

// The choices internal/api/schedules.go accepts (scheduleInput.apply).
const EVERY_OPTIONS: ScheduleEveryMinutes[] = [15, 30, 60, 120, 180, 240, 360, 480, 720];
const WARN_OPTIONS: ScheduleWarnMinutes[] = [0, 1, 5, 10, 15, 30];
const MAX_LABEL = 64; // runes
const MAX_COMMAND_BYTES = 1024; // maxConsoleCommandLen, after the leading "/" goes

// Picker order: the common chores first.
const ACTIONS: ScheduleAction[] = ["restart", "backup", "command", "stop", "start"];
const ACTION_ICONS: Record<ScheduleAction, LucideIcon> = {
  command: Terminal,
  restart: RotateCw,
  stop: PowerOff,
  start: Power,
  backup: Archive,
};
// The actions that take the server down: only these may warn the players first,
// and running one by hand disconnects them without that warning.
const WARNS = new Set<ScheduleAction>(["restart", "stop", "backup"]);

// Weekday bitmask, bit N = Go's time.Weekday N (0 = Sunday).
const ALL_DAYS = 127;
const WEEKDAYS = 62;
const WEEKEND = 65;
const DAY_PRESETS: [number, string][] = [
  [ALL_DAYS, "days_all"],
  [WEEKDAYS, "days_weekdays"],
  [WEEKEND, "days_weekend"],
];

// The fixed last_detail texts of the runner (schedulerunner.go), shown in the
// panel's language; any other detail (a console reply, an error with its
// cause) is shown as it came.
const DETAIL_KEYS: Record<string, string> = {
  "the server has a new owner since this schedule was saved; save it again to use it": "owner_changed",
  "the server was not running": "detail_not_running",
  "the server was already stopped": "detail_already_stopped",
  "the server was already running": "detail_already_running",
  "the server no longer exists": "detail_gone",
  "the server has no world yet": "detail_no_world",
  "felis-api was not running at the scheduled time": "detail_missed",
  "someone started the server before the run finished": "detail_started_meanwhile",
  "felis-api stopped in the middle of this run": "detail_interrupted",
  "the backup store is full; ask an administrator to free space": "detail_store_full",
  "the server console could not be reached": "detail_console",
  "the cluster is at its running-server cap": "detail_cap",
  "the server is being given up or deleted": "detail_retiring",
};

const RESULT_STYLE: Record<Exclude<ScheduleResult, "">, { icon: LucideIcon; className: string }> = {
  ok: { icon: CheckCircle2, className: "text-emerald-600 dark:text-emerald-400" },
  skipped: { icon: MinusCircle, className: "text-muted-foreground" },
  failed: { icon: XCircle, className: "text-destructive" },
  missed: { icon: AlertTriangle, className: "text-amber-600 dark:text-amber-400" },
};

/** Form is the create/edit dialog's state; toInput turns it into the wire body. */
interface Form {
  action: ScheduleAction;
  command: string;
  mode: "daily" | "interval";
  time: string; // HH:MM, for mode "daily"
  every: ScheduleEveryMinutes; // for mode "interval"
  weekdays: number;
  timezone: string;
  warn: ScheduleWarnMinutes;
  label: string;
  enabled: boolean;
}

function browserZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

function hhmm(minutes: number): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(Math.floor(minutes / 60))}:${pad(minutes % 60)}`;
}

function newForm(over: Partial<Form> = {}): Form {
  return {
    action: "restart",
    command: "",
    mode: "daily",
    time: "04:00",
    every: 60,
    weekdays: ALL_DAYS,
    timezone: browserZone(),
    warn: 5,
    label: "",
    enabled: true,
    ...over,
  };
}

function fromSchedule(s: Schedule): Form {
  return {
    action: s.action,
    command: s.command,
    mode: s.every_minutes ? "interval" : "daily",
    time: hhmm(s.minute_of_day),
    every: s.every_minutes || 60,
    weekdays: s.weekdays,
    timezone: s.timezone,
    warn: s.warn_minutes,
    label: s.label,
    enabled: s.enabled,
  };
}

// toInput sends the fields as typed: felis-api trims them and drops a
// command's leading "/" itself, so what it stores is what it validated.
function toInput(f: Form): ScheduleInput {
  const [h, m] = f.time.split(":").map(Number);
  return {
    label: f.label,
    action: f.action,
    command: f.action === "command" ? f.command : "",
    every_minutes: f.mode === "interval" ? f.every : 0,
    minute_of_day: f.mode === "daily" ? h * 60 + m : 0,
    weekdays: f.weekdays,
    timezone: f.timezone,
    warn_minutes: f.warn,
    enabled: f.enabled,
  };
}

function knownZone(zone: string): boolean {
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

// zoneList is what the time zone field suggests: the browser's zone first.
function zoneList(browser: string): string[] {
  let all: string[] = [];
  try {
    all = (Intl as unknown as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  } catch {
    // An older browser lists nothing; the field still takes any name.
  }
  return [browser, ...all.filter((z) => z !== browser)];
}

type Field = "command" | "time" | "days" | "timezone" | "label";

// problems mirrors scheduleInput.apply, so a form the server would refuse
// cannot be sent. The server stays the judge: whatever slips through comes back
// as a message in the dialog.
function problems(f: Form, t: TFunction): Partial<Record<Field, string>> {
  const out: Partial<Record<Field, string>> = {};
  if (f.action === "command") {
    const command = f.command.trim().replace(/^\//, "").trim();
    if (!command) out.command = t("err_command_required");
    else if (new TextEncoder().encode(command).length > MAX_COMMAND_BYTES) {
      out.command = t("err_command_long", { max: MAX_COMMAND_BYTES });
    }
  }
  if (f.mode === "daily" && !/^\d\d:\d\d$/.test(f.time)) out.time = t("err_time_required");
  if (f.weekdays === 0) out.days = t("err_days");
  const zone = f.timezone.trim();
  if (!zone) out.timezone = t("err_timezone_required");
  else if (!knownZone(zone)) out.timezone = t("err_timezone");
  if ([...f.label.trim()].length > MAX_LABEL) out.label = t("err_label_long", { max: MAX_LABEL });
  return out;
}

// weekOrder is the weekdays in the locale's order (Monday first in Chinese).
function weekOrder(t: TFunction): number[] {
  const first = t("first_day") === "1" ? 1 : 0;
  return [0, 1, 2, 3, 4, 5, 6].map((i) => (i + first) % 7);
}

function daysText(mask: number, t: TFunction): string {
  const preset = DAY_PRESETS.find(([m]) => m === mask);
  if (preset) return t(preset[1]);
  return weekOrder(t)
    .filter((d) => mask & (1 << d))
    .map((d) => t(`day_${d}`))
    .join(t("day_sep"));
}

function everyText(minutes: number, t: TFunction): string {
  return minutes < 60 ? t("every_minutes", { count: minutes }) : t("every_hours", { count: minutes / 60 });
}

function optionText(minutes: number, t: TFunction): string {
  return minutes < 60 ? t("opt_minutes", { count: minutes }) : t("opt_hours", { count: minutes / 60 });
}

// summary is the one-line "when" of a schedule: "Every day at 04:00",
// "Every 30 minutes · Weekdays".
function summary(s: Schedule, t: TFunction): string {
  const days = daysText(s.weekdays, t);
  if (!s.every_minutes) return t("when_daily", { days, time: hhmm(s.minute_of_day) });
  const every = everyText(s.every_minutes, t);
  return s.weekdays === ALL_DAYS ? every : t("when_interval_days", { every, days });
}

// runTimes previews an interval's runs: it counts from midnight in its zone.
function runTimes(every: number): string {
  const times: string[] = [];
  for (let m = 0; m < 24 * 60; m += every) times.push(hhmm(m));
  return times.length <= 4 ? times.join(", ") : `${times.slice(0, 3).join(", ")} … ${times[times.length - 1]}`;
}

function titleOf(s: Schedule, t: TFunction): string {
  return s.label || t(`action_${s.action}`);
}

function FieldNote({ error, hint }: { error?: string; hint?: string }) {
  if (error) return <p className="text-xs text-destructive">{error}</p>;
  return hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null;
}

/** ScheduleDialog creates a schedule, or edits `editing`. It stays open on a
 *  refusal and shows the server's reason above its buttons. */
function ScheduleDialog({
  serverName,
  editing,
  initial,
  onClose,
  onSaved,
}: {
  serverName: string;
  editing: Schedule | null;
  initial: Form;
  onClose: () => void;
  onSaved: (s: Schedule) => void;
}) {
  const { t } = useTranslation("schedules");
  const { t: tc } = useTranslation("common");
  const id = useId();
  const [form, setForm] = useState(initial);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const browser = browserZone();
  const zones = useMemo(() => zoneList(browser), [browser]);
  const bad = problems(form, t);
  const labelLength = [...form.label.trim()].length;

  function set<K extends keyof Form>(key: K, value: Form[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  // Only a restart, stop or backup warns, and only a command repeats more
  // often than hourly: a switch clears what the new action cannot keep.
  function setAction(action: ScheduleAction) {
    setForm((f) => ({
      ...f,
      action,
      warn: WARNS.has(action) ? f.warn : 0,
      every: action === "command" || f.every >= 60 ? f.every : 60,
    }));
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      const input = toInput(form);
      onSaved(
        editing
          ? await api.updateSchedule(serverName, editing.id, input)
          : await api.createSchedule(serverName, input),
      );
    } catch (e) {
      setError(humanizeError(e));
      setSaving(false);
    }
  }

  const everyOptions = form.action === "command" ? EVERY_OPTIONS : EVERY_OPTIONS.filter((m) => m >= 60);

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !saving) onClose();
      }}
    >
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-xl" hideClose={saving}>
        <DialogHeader>
          <DialogTitle>{editing ? t("edit_title") : t("create_title")}</DialogTitle>
          <DialogDescription>{t("dialog_desc")}</DialogDescription>
        </DialogHeader>

        <div className="grid gap-5">
          <fieldset className="grid gap-2">
            <legend className="mb-2 text-sm font-medium">{t("field_action")}</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {ACTIONS.map((a) => {
                const Icon = ACTION_ICONS[a];
                const checked = form.action === a;
                return (
                  <label
                    key={a}
                    className={cn(
                      "flex cursor-pointer items-start gap-3 rounded-md border p-3 text-sm transition-colors has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring",
                      checked ? "border-primary/60 bg-primary/5" : "border-border hover:bg-accent",
                    )}
                  >
                    <input
                      type="radio"
                      className="sr-only"
                      name={`${id}-action`}
                      value={a}
                      checked={checked}
                      onChange={() => setAction(a)}
                      aria-labelledby={`${id}-action-${a}`}
                      aria-describedby={`${id}-action-${a}-desc`}
                    />
                    <Icon className={cn("mt-0.5 h-4 w-4 shrink-0", checked ? "text-primary" : "text-muted-foreground")} />
                    <span className="grid gap-0.5">
                      <span id={`${id}-action-${a}`} className="font-medium">
                        {t(`action_${a}`)}
                      </span>
                      <span id={`${id}-action-${a}-desc`} className="text-xs text-muted-foreground">
                        {t(`action_${a}_desc`)}
                      </span>
                    </span>
                  </label>
                );
              })}
            </div>
          </fieldset>

          {form.action === "command" && (
            <div className="grid gap-2">
              <Label htmlFor={`${id}-command`}>{t("field_command")}</Label>
              <Input
                id={`${id}-command`}
                className="font-mono"
                value={form.command}
                onChange={(e) => set("command", e.target.value)}
                placeholder={t("command_placeholder")}
                autoComplete="off"
                spellCheck={false}
                aria-invalid={!!bad.command}
              />
              <FieldNote error={bad.command} hint={t("command_hint")} />
            </div>
          )}

          <fieldset className="grid gap-3">
            <legend className="mb-2 text-sm font-medium">{t("field_when")}</legend>
            <div className="inline-flex w-fit rounded-lg bg-muted p-1">
              {(["daily", "interval"] as const).map((m) => (
                <label
                  key={m}
                  className={cn(
                    "cursor-pointer rounded px-3 py-1.5 text-sm font-medium transition-colors has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring",
                    form.mode === m ? "bg-card text-primary" : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <input
                    type="radio"
                    className="sr-only"
                    name={`${id}-mode`}
                    checked={form.mode === m}
                    onChange={() => set("mode", m)}
                  />
                  {t(`mode_${m}`)}
                </label>
              ))}
            </div>
            {form.mode === "daily" ? (
              <div className="grid gap-2">
                <Label htmlFor={`${id}-time`}>{t("field_time")}</Label>
                <Input
                  id={`${id}-time`}
                  type="time"
                  className="w-36"
                  value={form.time}
                  onChange={(e) => set("time", e.target.value)}
                  aria-invalid={!!bad.time}
                />
                <FieldNote error={bad.time} />
              </div>
            ) : (
              <div className="grid gap-2">
                <Label htmlFor={`${id}-every`}>{t("field_every")}</Label>
                <Select value={String(form.every)} onValueChange={(v) => set("every", Number(v) as ScheduleEveryMinutes)}>
                  <SelectTrigger id={`${id}-every`} className="w-44">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {everyOptions.map((m) => (
                      <SelectItem key={m} value={String(m)}>
                        {optionText(m, t)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">
                  {t("interval_preview", { times: runTimes(form.every) })}
                  {form.action !== "command" && ` ${t("interval_hourly_limit")}`}
                </p>
              </div>
            )}
          </fieldset>

          <fieldset className="grid gap-2">
            <legend className="mb-2 text-sm font-medium">{t("field_days")}</legend>
            <div className="flex flex-wrap gap-1.5">
              {weekOrder(t).map((d) => {
                const on = (form.weekdays & (1 << d)) !== 0;
                return (
                  <button
                    key={d}
                    type="button"
                    aria-pressed={on}
                    aria-label={t(`day_full_${d}`)}
                    onClick={() => set("weekdays", form.weekdays ^ (1 << d))}
                    className={cn(
                      "h-9 min-w-10 rounded-md border px-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                      on
                        ? "border-primary bg-primary text-primary-foreground"
                        : "border-input text-muted-foreground hover:bg-accent hover:text-foreground",
                    )}
                  >
                    {t(`day_chip_${d}`)}
                  </button>
                );
              })}
            </div>
            <div className="flex flex-wrap gap-1.5">
              {DAY_PRESETS.map(([mask, key]) => (
                <Button
                  key={key}
                  type="button"
                  size="sm"
                  variant={form.weekdays === mask ? "default" : "ghost"}
                  className="h-7"
                  aria-pressed={form.weekdays === mask}
                  onClick={() => set("weekdays", mask)}
                >
                  {t(key)}
                </Button>
              ))}
            </div>
            <FieldNote error={bad.days} />
          </fieldset>

          <div className="grid gap-2">
            <Label htmlFor={`${id}-zone`}>{t("field_timezone")}</Label>
            <Input
              id={`${id}-zone`}
              list={`${id}-zones`}
              value={form.timezone}
              onChange={(e) => set("timezone", e.target.value)}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={!!bad.timezone}
            />
            <datalist id={`${id}-zones`}>
              {zones.map((z) => (
                <option key={z} value={z} />
              ))}
            </datalist>
            {bad.timezone ? (
              <FieldNote error={bad.timezone} />
            ) : (
              <p className="text-xs text-muted-foreground">{t("timezone_hint", { zone: browser })}</p>
            )}
            {form.timezone !== browser && (
              <button
                type="button"
                className="w-fit text-xs font-medium text-primary underline-offset-2 hover:underline"
                onClick={() => set("timezone", browser)}
              >
                {t("timezone_use_browser")}
              </button>
            )}
          </div>

          {WARNS.has(form.action) && (
            <div className="grid gap-2">
              <Label htmlFor={`${id}-warn`}>{t("field_warn")}</Label>
              <Select value={String(form.warn)} onValueChange={(v) => set("warn", Number(v) as ScheduleWarnMinutes)}>
                <SelectTrigger id={`${id}-warn`} className="w-56">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {WARN_OPTIONS.map((m) => (
                    <SelectItem key={m} value={String(m)}>
                      {m === 0 ? t("warn_none") : t("warn_option", { count: m })}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">{t("warn_hint")}</p>
            </div>
          )}

          {form.action === "backup" && (
            <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs text-amber-700 dark:text-amber-300">
              <Info className="mt-px h-3.5 w-3.5 shrink-0" />
              {t("backup_note")}
            </p>
          )}

          <div className="grid gap-2">
            <div className="flex items-baseline justify-between gap-2">
              <Label htmlFor={`${id}-label`}>{t("field_label")}</Label>
              <span className={cn("text-xs tabular-nums", bad.label ? "text-destructive" : "text-muted-foreground")}>
                {labelLength}/{MAX_LABEL}
              </span>
            </div>
            <Input
              id={`${id}-label`}
              value={form.label}
              onChange={(e) => set("label", e.target.value)}
              placeholder={t("label_placeholder")}
              aria-invalid={!!bad.label}
            />
            <FieldNote error={bad.label} />
          </div>

          <label className="flex cursor-pointer items-start gap-3 rounded-md border border-border p-3 text-sm">
            <Checkbox

              className="mt-0.5"
              checked={form.enabled}
              onChange={(e) => set("enabled", e.target.checked)}
              aria-labelledby={`${id}-enabled`}
              aria-describedby={`${id}-enabled-hint`}
            />
            <span className="grid gap-0.5">
              <span id={`${id}-enabled`} className="font-medium">
                {t("field_enabled")}
              </span>
              <span id={`${id}-enabled-hint`} className="text-xs text-muted-foreground">
                {t("enabled_hint")}
              </span>
            </span>
          </label>
        </div>

        <InlineError message={error} />
        <ConfirmFooter
          onCancel={onClose}
          onConfirm={() => void save()}
          disabled={Object.keys(bad).length > 0}
          loading={saving}
          cancelLabel={tc("cancel")}
          confirmLabel={editing ? t("save") : t("create")}
          confirmVariant="default"
        />
      </DialogContent>
    </Dialog>
  );
}

/** Disabled buttons take no pointer events, so the reason rides a wrapper. */
function Why({ why, children }: { why?: string; children: React.ReactNode }) {
  return (
    <span title={why} className="inline-flex">
      {children}
    </span>
  );
}

/** ScheduleRow is one schedule: what, when, how the last run went, and its
 *  actions. A run in progress locks it (the API answers 409 schedule_running). */
function ScheduleRow({
  s,
  now,
  locale,
  browser,
  toggling,
  onToggle,
  onRun,
  onEdit,
  onDelete,
}: {
  s: Schedule;
  now: number;
  locale: string;
  browser: string;
  toggling: boolean;
  onToggle: () => void;
  onRun: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation("schedules");
  const Icon = ACTION_ICONS[s.action];
  const title = titleOf(s, t);
  const busy = s.run_state !== "";
  const why = busy ? t("busy_hint") : undefined;
  const result = s.last_result ? RESULT_STYLE[s.last_result] : null;
  const detailKey = DETAIL_KEYS[s.last_detail];
  const detail = detailKey
    ? t(detailKey)
    : s.last_result === "ok" && s.last_detail
      ? t("reply", { text: s.last_detail })
      : s.last_detail;

  return (
    <li className="flex flex-col gap-3 p-4 sm:flex-row sm:items-start sm:justify-between">
      <div className="flex min-w-0 flex-1 gap-3">
        <div
          className={cn(
            "mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-md",
            s.enabled ? "bg-primary/10 text-primary" : "bg-muted text-muted-foreground",
          )}
        >
          <Icon className="h-4 w-4" />
        </div>
        <div className="min-w-0 flex-1 space-y-1.5">
          <div className="flex flex-wrap items-center gap-1.5">
            <p className={cn("min-w-0 truncate text-sm font-medium", !s.enabled && "text-muted-foreground")}>{title}</p>
            {s.label && <Badge variant="muted">{t(`action_${s.action}`)}</Badge>}
            {!s.enabled && <Badge variant="muted">{t("off_badge")}</Badge>}
            {s.warn_minutes > 0 && (
              <Badge variant="outline" className="gap-1 font-normal text-muted-foreground">
                <Megaphone className="h-3 w-3" />
                {t("warn_badge", { count: s.warn_minutes })}
              </Badge>
            )}
            {busy && (
              <Badge className="gap-1">
                <Loader2 className="h-3 w-3 animate-spin" />
                {t(`run_state_${s.run_state}`)}
              </Badge>
            )}
          </div>
          <p className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted-foreground">
            <span className="inline-flex items-center gap-1.5">
              <Clock className="h-3.5 w-3.5 shrink-0" />
              {summary(s, t)}
            </span>
            {s.timezone !== browser && (
              <span className="inline-flex items-center gap-1 text-xs" title={t("tz_differs", { zone: s.timezone, browser })}>
                <Globe className="h-3 w-3 shrink-0" />
                {s.timezone}
              </span>
            )}
          </p>
          {s.action === "command" && (
            <code className="block w-fit max-w-full truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs" title={s.command}>
              {s.command}
            </code>
          )}
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
            {s.next_run_at ? (
              <span title={formatAbsolute(s.next_run_at, locale)}>
                {t("next_run", { when: formatRelative(s.next_run_at, now, locale) })}
              </span>
            ) : (
              <span>{t("next_run_off")}</span>
            )}
            {result && s.last_run_at ? (
              <span className="inline-flex items-center gap-1.5">
                <span className={cn("inline-flex items-center gap-1 font-medium", result.className)}>
                  <result.icon className="h-3.5 w-3.5" />
                  {t(`result_${s.last_result}`)}
                </span>
                <span title={formatAbsolute(s.last_run_at, locale)}>
                  {t("last_run", { when: formatRelative(s.last_run_at, now, locale) })}
                </span>
              </span>
            ) : s.last_run_at ? (
              // A claim sets last_run_at to its start and clears the result, so
              // this is the run in progress.
              <span title={formatAbsolute(s.last_run_at, locale)}>
                {t("run_started_at", { when: formatRelative(s.last_run_at, now, locale) })}
              </span>
            ) : (
              <span>{t("never_run")}</span>
            )}
          </div>
          {detail && (
            <p
              className={cn(
                "line-clamp-2 break-words text-xs",
                s.last_result === "failed" ? "text-destructive" : "text-muted-foreground",
              )}
              title={s.last_detail}
            >
              {detail}
            </p>
          )}
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2 pl-12 sm:flex-col sm:items-end sm:pl-0">
        <Why why={why}>
          <button
            type="button"
            role="switch"
            aria-checked={s.enabled}
            aria-label={t("toggle_label", { name: title })}
            disabled={busy || toggling}
            onClick={onToggle}
            className={cn(
              "relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
              s.enabled ? "bg-primary" : "bg-input",
            )}
          >
            <span
              className={cn(
                "inline-block h-4 w-4 rounded-full bg-background shadow transition-transform",
                s.enabled ? "translate-x-[18px]" : "translate-x-0.5",
              )}
            />
          </button>
        </Why>
        <div className="flex flex-wrap gap-1.5">
          <Why why={why}>
            <Button size="sm" variant="outline" onClick={onRun} disabled={busy}>
              <Play className="h-3.5 w-3.5" />
              {t("run_now")}
            </Button>
          </Why>
          <Why why={why}>
            <Button size="sm" variant="ghost" onClick={onEdit} disabled={busy}>
              <Pencil className="h-3.5 w-3.5" />
              {t("edit")}
            </Button>
          </Why>
          <Why why={why}>
            <Button
              size="sm"
              variant="ghost"
              className="text-destructive hover:text-destructive"
              onClick={onDelete}
              disabled={busy}
            >
              <Trash2 className="h-3.5 w-3.5" />
              {t("delete")}
            </Button>
          </Why>
        </div>
      </div>
    </li>
  );
}

export function ServerSchedules() {
  const { name = "" } = useParams();
  const { t, i18n } = useTranslation("schedules");
  const { isAdmin, loading: tierLoading } = useTier();
  const statusQ = useAsync(() => api.status(name), [name]);
  const mineQ = useAsync(() => (isAdmin ? Promise.resolve([]) : api.myServers()), [isAdmin, name]);
  // Ownership resolves from /me/servers for a non-admin; the list is read only
  // once the viewer is known to own the server (the route answers 403 otherwise).
  const pending = ownershipPending(tierLoading, isAdmin, mineQ.data, mineQ.error);
  const owned = canManage(isAdmin, mineQ.data, name);
  const listQ = useAsync(() => (owned ? api.listSchedules(name) : Promise.resolve(null)), [name, owned]);
  // A run in progress steps on every few seconds (and takes the server with it),
  // so both reads follow it closely; otherwise slowly, for the next runs and the
  // results of runs that came due.
  const running = (listQ.data?.schedules ?? []).some((s) => s.run_state !== "");
  const pollMs = running ? STATUS_POLL_FAST_MS : STATUS_POLL_SLOW_MS;
  usePolling(statusQ.reload, pollMs);
  usePolling(listQ.reload, pollMs);

  const [msg, setMsg] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const [editor, setEditor] = useState<{ editing: Schedule | null; form: Form } | null>(null);
  const [runTarget, setRunTarget] = useState<Schedule | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Schedule | null>(null);
  const [toggling, setToggling] = useState<number | null>(null);

  function openCreate(over: Partial<Form> = {}) {
    setMsg(null);
    setEditor({ editing: null, form: newForm(over) });
  }

  // The switch saves the schedule as it is with `enabled` flipped: a PUT takes
  // the whole input, and it also hands the schedule to the current owner.
  async function toggle(s: Schedule) {
    setToggling(s.id);
    setMsg(null);
    try {
      await api.updateSchedule(name, s.id, toInput({ ...fromSchedule(s), enabled: !s.enabled }));
      setMsg({ kind: "success", text: t(s.enabled ? "toggled_off" : "toggled_on", { name: titleOf(s, t) }) });
      listQ.reload();
    } catch (e) {
      setMsg({ kind: "error", text: humanizeError(e) });
    } finally {
      setToggling(null);
    }
  }

  const back = <BackLink to={`/servers/${name}`} label={t("back_to_console")} />;

  if (statusQ.loading && !statusQ.data) {
    return (
      <>
        {back}
        <Loading />
      </>
    );
  }
  if (statusQ.error && !statusQ.data) {
    return (
      <>
        {back}
        <ErrorState error={statusQ.error} onRetry={statusQ.reload} />
      </>
    );
  }
  if (!statusQ.data) return back;

  const now = Date.now();
  const locale = i18n.language;
  const browser = browserZone();
  const list = listQ.data;
  const full = !!list && list.schedules.length >= list.limit;
  const templates: [string, LucideIcon, Partial<Form>][] = [
    ["template_restart", RotateCw, {}],
    ["template_backup", Archive, { action: "backup", time: "05:00" }],
    [
      "template_announce",
      Megaphone,
      { action: "command", command: t("template_announce_command"), mode: "interval", every: 30, warn: 0 },
    ],
  ];

  const header = (
    <PageHeader
      icon={CalendarClock}
      title={statusQ.data.displayName || statusQ.data.name}
      subtitle={t("title")}
      actions={
        <div className="flex items-center gap-2">
          {owned && list && (
            <Why why={full ? t("limit_reached", { limit: list.limit }) : undefined}>
              <Button size="sm" onClick={() => openCreate()} disabled={full}>
                <Plus className="h-4 w-4" />
                {t("new_task")}
              </Button>
            </Why>
          )}
          <PhaseBadge
            phase={shownPhase(statusQ.data)}
            failure={startFailure(statusQ.data)}
            autoRestarts={statusQ.data.autoRestarts}
          />
        </div>
      }
      className="mb-6"
    />
  );

  let body: React.ReactNode;
  if (listQ.error && !list) {
    body =
      (listQ.error as { code?: string }).code === "schedules_unavailable" ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-2 py-12 text-center">
            <CalendarClock className="h-7 w-7 text-muted-foreground" />
            <p className="font-medium">{t("unavailable_title")}</p>
            <p className="max-w-md text-sm text-muted-foreground">{t("unavailable_body")}</p>
          </CardContent>
        </Card>
      ) : (
        <ErrorState error={listQ.error} onRetry={listQ.reload} />
      );
  } else if (!list) {
    body = <Loading />;
  } else {
    body = (
      <>
        {!!listQ.error && <RefreshError error={listQ.error} />}
        {list.schedules.length === 0 ? (
          <EmptyState title={t("empty_title")} hint={t("empty_hint")}>
            <div className="flex flex-wrap justify-center gap-2 px-4">
              {templates.map(([key, Icon, over]) => (
                <Button key={key} size="sm" variant="outline" onClick={() => openCreate(over)}>
                  <Icon className="h-4 w-4" />
                  {t(key)}
                </Button>
              ))}
            </div>
          </EmptyState>
        ) : (
          <Card className="overflow-hidden">
            <ul className="divide-y divide-border">
              {list.schedules.map((s) => (
                <ScheduleRow
                  key={s.id}
                  s={s}
                  now={now}
                  locale={locale}
                  browser={browser}
                  toggling={toggling === s.id}
                  onToggle={() => void toggle(s)}
                  onRun={() => setRunTarget(s)}
                  onEdit={() => {
                    setMsg(null);
                    setEditor({ editing: s, form: fromSchedule(s) });
                  }}
                  onDelete={() => setDeleteTarget(s)}
                />
              ))}
            </ul>
          </Card>
        )}
        <Card className="border-dashed bg-transparent">
          <CardContent className="text-xs text-muted-foreground">
            <p className="mb-2 flex items-center gap-1.5 font-medium text-foreground">
              <Info className="h-3.5 w-3.5" />
              {t("notes_title")}
            </p>
            <ul className="list-disc space-y-1 pl-5">
              <li>{t("note_timing")}</li>
              <li>{t("note_state")}</li>
              <li>{t("note_owner")}</li>
              <li>{t("note_run_now")}</li>
            </ul>
          </CardContent>
        </Card>
      </>
    );
  }

  return (
    <>
      {back}
      {header}
      {!!statusQ.error && <RefreshError error={statusQ.error} className="mb-4" />}
      {pending ? (
        <Loading />
      ) : mineQ.error ? (
        <ErrorState error={mineQ.error} onRetry={mineQ.reload} />
      ) : !owned ? (
        <NotYours title={t("not_yours_title")} body={t("not_yours_body")} />
      ) : (
        <div className="space-y-4">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <p className="text-sm text-muted-foreground">{t("subtitle")}</p>
            {list && (
              <span className="text-xs tabular-nums text-muted-foreground">
                {t("count", { count: list.schedules.length, limit: list.limit })}
              </span>
            )}
          </div>
          {msg && <MessageLine kind={msg.kind} message={msg.text} />}
          {body}
        </div>
      )}

      {editor && (
        <ScheduleDialog
          serverName={name}
          editing={editor.editing}
          initial={editor.form}
          onClose={() => setEditor(null)}
          onSaved={(s) => {
            setMsg({ kind: "success", text: t(editor.editing ? "saved" : "created", { name: titleOf(s, t) }) });
            setEditor(null);
            listQ.reload();
          }}
        />
      )}

      {runTarget && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open) setRunTarget(null);
          }}
          title={t("run_title", { name: titleOf(runTarget, t) })}
          description={
            <>
              <span className="block">{t(`run_body_${runTarget.action}`)}</span>
              {runTarget.action === "command" && (
                <code className="mt-2 block break-all rounded bg-muted px-2 py-1 font-mono text-xs text-foreground">
                  {runTarget.command}
                </code>
              )}
              {WARNS.has(runTarget.action) && (
                <span className="mt-3 flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2.5 text-amber-700 dark:text-amber-300">
                  <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
                  {t("run_kick")}
                </span>
              )}
              <span className="mt-3 block">{t("run_keeps_next")}</span>
            </>
          }
          confirmLabel={t("run_confirm")}
          confirmVariant={WARNS.has(runTarget.action) ? "destructive" : "default"}
          onConfirm={async () => {
            await api.runSchedule(name, runTarget.id);
            setMsg({ kind: "success", text: t("run_started", { name: titleOf(runTarget, t) }) });
            listQ.reload();
          }}
        />
      )}

      {deleteTarget && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open) setDeleteTarget(null);
          }}
          title={t("delete_title", { name: titleOf(deleteTarget, t) })}
          description={t("delete_body")}
          confirmLabel={t("delete_confirm")}
          onConfirm={async () => {
            await api.deleteSchedule(name, deleteTarget.id);
            setMsg({ kind: "success", text: t("deleted", { name: titleOf(deleteTarget, t) }) });
            listQ.reload();
          }}
        />
      )}
    </>
  );
}
