import { Checkbox } from "@/components/ui/checkbox";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import {
  AlertTriangle,
  Archive,
  ArrowDown,
  ArrowUp,
  CalendarClock,
  CheckCircle2,
  Clock,
  Globe,
  Info,
  Loader2,
  Megaphone,
  MinusCircle,
  OctagonX,
  Pencil,
  Play,
  Plus,
  Power,
  PowerOff,
  RotateCw,
  Terminal,
  Timer,
  Trash2,
  X,
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
import { SegmentedControl } from "@/components/SegmentedControl";
import { api, humanizeError } from "@/lib/api";
import { useAsync, usePolling, STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage, ownershipPending } from "@/lib/ownership";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";
import type {
  Schedule,
  ScheduleEveryMinutes,
  ScheduleInput,
  ScheduleResult,
  ScheduleStep,
  ScheduleStepType,
  ScheduleWarnMinutes,
} from "@/lib/types";

// The choices internal/api/schedules.go accepts (scheduleInput.apply).
const EVERY_OPTIONS: ScheduleEveryMinutes[] = [1, 5, 10, 15, 30, 60, 120, 180, 240, 360, 480, 720];
const WARN_OPTIONS: ScheduleWarnMinutes[] = [0, 1, 5, 10, 15, 30];
const MAX_LABEL = 64; // runes
const MAX_COMMAND_BYTES = 1024; // maxConsoleCommandLen, after the leading "/" goes
const MAX_STEPS = 10;
const MAX_DELAY_SECONDS = 3600;
const MAX_RUNS = 10000;
const HOURLY = 60; // minPowerEveryMinutes
const DEFAULT_DELAY_SECONDS = "60";

const STEP_TYPES: ScheduleStepType[] = ["command", "delay", "start", "stop", "restart", "kill", "backup"];
const STEP_ICONS: Record<ScheduleStepType, LucideIcon> = {
  command: Terminal,
  delay: Timer,
  start: Power,
  stop: PowerOff,
  restart: RotateCw,
  kill: OctagonX,
  backup: Archive,
};
// A task with one of these steps may warn the players first, and running it by hand skips that warning.
const WARNS = new Set<ScheduleStepType>(["stop", "restart", "kill", "backup"]);
// A task with one of these steps repeats at most hourly.
const POWERS = new Set<ScheduleStepType>(["start", "stop", "restart", "kill", "backup"]);

const warns = (steps: { type: ScheduleStepType }[]) => steps.some((s) => WARNS.has(s.type));
const powers = (steps: { type: ScheduleStepType }[]) => steps.some((s) => POWERS.has(s.type));

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
  "the server did not stop within 15 minutes": "detail_stop_timeout",
  "another operation kept the world busy for 15 minutes": "detail_world_busy",
  "the server failed to start": "detail_start_failed",
  "the server did not become ready within 15 minutes": "detail_ready_timeout",
  "the backup did not finish within 45 minutes": "detail_backup_timeout",
  "the backup Job is gone before it could be checked; see the Backups page": "detail_backup_gone",
};
// A failed run's detail ends with this when the run started the server again.
const RESTARTED = "; the server was started again";

const RESULT_STYLE: Record<Exclude<ScheduleResult, "">, { icon: LucideIcon; className: string }> = {
  ok: { icon: CheckCircle2, className: "text-emerald-600 dark:text-emerald-400" },
  skipped: { icon: MinusCircle, className: "text-muted-foreground" },
  failed: { icon: XCircle, className: "text-destructive" },
  missed: { icon: AlertTriangle, className: "text-amber-600 dark:text-amber-400" },
};

/** StepDraft is one step of the form; `key` keeps its row steady while steps move. */
interface StepDraft {
  key: number;
  type: ScheduleStepType;
  command: string;
  seconds: string;
}

/** Form is the create/edit dialog's state; toInput turns it into the wire body. */
interface Form {
  steps: StepDraft[];
  mode: "daily" | "interval" | "once";
  time: string; // HH:MM, for mode "daily"
  every: ScheduleEveryMinutes; // for mode "interval"
  weekdays: number;
  once: string; // YYYY-MM-DDTHH:MM on the wall clock of `timezone`, for mode "once"
  maxRuns: string; // blank for no limit
  timezone: string;
  warn: ScheduleWarnMinutes;
  label: string;
  enabled: boolean;
}

let lastStepKey = 0;

function draftOf(step: ScheduleStep): StepDraft {
  return { key: ++lastStepKey, type: step.type, command: step.command ?? "", seconds: step.seconds ? String(step.seconds) : "" };
}

const draftsOf = (steps: ScheduleStep[]) => steps.map(draftOf);

// toStep sends a command as typed: felis-api trims it and drops a leading "/" itself.
function toStep(d: StepDraft): ScheduleStep {
  if (d.type === "command") return { type: "command", command: d.command };
  if (d.type === "delay") return { type: "delay", seconds: Number(d.seconds) };
  return { type: d.type };
}

function browserZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

function hhmm(minutes: number): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(Math.floor(minutes / 60))}:${pad(minutes % 60)}`;
}

function knownZone(zone: string): boolean {
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

// wallAsUTC reads the clock of `zone` at an instant as if it were UTC.
function wallAsUTC(at: number, zone: string): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: zone,
    hourCycle: "h23",
    year: "numeric",
    month: "numeric",
    day: "numeric",
    hour: "numeric",
    minute: "numeric",
    second: "numeric",
  }).formatToParts(at);
  const n = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return Date.UTC(n("year"), n("month") - 1, n("day"), n("hour"), n("minute"), n("second"));
}

// localInput writes an instant as the datetime-local value on the clock of `zone`.
function localInput(iso: string, zone: string): string {
  const at = Date.parse(iso);
  if (Number.isNaN(at) || !knownZone(zone)) return "";
  const w = new Date(wallAsUTC(at, zone));
  const pad = (n: number, width = 2) => String(n).padStart(width, "0");
  return `${pad(w.getUTCFullYear(), 4)}-${pad(w.getUTCMonth() + 1)}-${pad(w.getUTCDate())}T${pad(w.getUTCHours())}:${pad(w.getUTCMinutes())}`;
}

const LOCAL_INPUT = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d)$/;

// instantAt is the instant at which the clock of `zone` reads a datetime-local value; null when it names none.
function instantAt(local: string, zone: string): number | null {
  const m = LOCAL_INPUT.exec(local);
  if (!m || !knownZone(zone)) return null;
  const [year, month, day, hour, minute] = m.slice(1).map(Number);
  const wall = Date.UTC(year, month - 1, day, hour, minute);
  // A second pass settles the offset when the first guess lands across a DST change.
  const first = wall - (wallAsUTC(wall, zone) - wall);
  const at = wall - (wallAsUTC(first, zone) - first);
  return Number.isFinite(at) ? at : null;
}

function newForm(over: Partial<Form> = {}): Form {
  return {
    steps: draftsOf([{ type: "restart" }]),
    mode: "daily",
    time: "04:00",
    every: 60,
    weekdays: ALL_DAYS,
    once: "",
    maxRuns: "",
    timezone: browserZone(),
    warn: 5,
    label: "",
    enabled: true,
    ...over,
  };
}

function fromSchedule(s: Schedule): Form {
  return {
    steps: draftsOf(s.steps),
    mode: s.run_once_at ? "once" : s.every_minutes ? "interval" : "daily",
    time: hhmm(s.minute_of_day),
    every: s.every_minutes || 60,
    weekdays: s.weekdays,
    once: s.run_once_at ? localInput(s.run_once_at, s.timezone) : "",
    maxRuns: s.max_runs ? String(s.max_runs) : "",
    timezone: s.timezone,
    warn: s.warn_minutes,
    label: s.label,
    enabled: s.enabled,
  };
}

// settle clears what the steps cannot keep: only a step that takes the server down warns,
// and only a task with no such step repeats more often than hourly.
function settle(f: Form): Form {
  return {
    ...f,
    warn: warns(f.steps) ? f.warn : 0,
    every: powers(f.steps) && f.every < HOURLY ? HOURLY : f.every,
  };
}

// toInput sends the fields as typed: felis-api trims them and drops a
// command's leading "/" itself, so what it stores is what it validated.
function toInput(f: Form): ScheduleInput {
  const [h, m] = f.time.split(":").map(Number);
  const once = f.mode === "once";
  const at = once ? instantAt(f.once, f.timezone) : null;
  return {
    label: f.label,
    steps: f.steps.map(toStep),
    every_minutes: f.mode === "interval" ? f.every : 0,
    minute_of_day: f.mode === "daily" ? h * 60 + m : 0,
    weekdays: once ? ALL_DAYS : f.weekdays,
    timezone: f.timezone,
    run_once_at: at === null ? null : new Date(at).toISOString(),
    max_runs: once ? 0 : Number(f.maxRuns || 0),
    warn_minutes: f.warn,
    enabled: f.enabled,
  };
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

// problems mirrors the server's checks, keyed by field name or step-<key>.
function problems(f: Form, t: TFunction, now: number, usedRuns: number): Record<string, string> {
  const out: Record<string, string> = {};
  for (const s of f.steps) {
    if (s.type === "command") {
      const command = s.command.trim().replace(/^\//, "").trim();
      if (!command) out[`step-${s.key}`] = t("err_command_required");
      else if (new TextEncoder().encode(command).length > MAX_COMMAND_BYTES) {
        out[`step-${s.key}`] = t("err_command_long", { max: MAX_COMMAND_BYTES });
      }
    } else if (s.type === "delay") {
      const seconds = Number(s.seconds);
      if (!/^\d+$/.test(s.seconds) || seconds < 1 || seconds > MAX_DELAY_SECONDS) {
        out[`step-${s.key}`] = t("err_delay", { max: MAX_DELAY_SECONDS });
      }
    }
  }
  if (!f.steps.some((s) => s.type !== "delay")) out.steps = t("err_steps_action");
  else if (f.steps.filter((s) => s.type === "backup").length > 1) out.steps = t("err_steps_backup");

  if (f.mode === "daily" && !/^\d\d:\d\d$/.test(f.time)) out.time = t("err_time_required");
  if (f.mode !== "once") {
    if (f.weekdays === 0) out.days = t("err_days");
    const runs = f.maxRuns.trim();
    if (runs && (!/^\d+$/.test(runs) || Number(runs) > MAX_RUNS)) out.maxRuns = t("err_max_runs", { max: MAX_RUNS });
    else if (f.enabled && Number(runs) > 0 && Number(runs) <= usedRuns) out.maxRuns = t("err_max_runs_used", { count: usedRuns });
  }
  const zone = f.timezone.trim();
  if (!zone) out.timezone = t("err_timezone_required");
  else if (!knownZone(zone)) out.timezone = t("err_timezone");
  if (f.mode === "once") {
    const at = out.timezone ? null : instantAt(f.once, zone);
    if (!f.once || (at === null && !out.timezone)) out.once = t("err_once_required");
    else if (at !== null && f.enabled && at <= now) out.once = t("err_once_past");
  }
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
// "Every 30 minutes · Weekdays", "Once on 2026-11-01 04:00".
function summary(s: Schedule, t: TFunction): string {
  if (s.run_once_at) {
    const when = localInput(s.run_once_at, s.timezone).replace("T", " ");
    return t("when_once", { when: when || s.run_once_at });
  }
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

function stepName(step: ScheduleStep, t: TFunction): string {
  return step.type === "delay" ? t("step_delay_of", { count: step.seconds ?? 0 }) : t(`step_${step.type}`);
}

// stepsText is "Console command → Delay 60 seconds → Restart".
function stepsText(steps: ScheduleStep[], t: TFunction): string {
  return steps.map((step) => stepName(step, t)).join(t("step_sep"));
}

function titleOf(s: Schedule, t: TFunction): string {
  return s.label || stepsText(s.steps, t);
}

// progress is what a run in progress is doing: "Step 2 of 3: Waiting for the server to stop…".
function progress(s: Schedule, t: TFunction): string {
  const state = t(`run_state_${s.run_state}`);
  return s.steps.length > 1 ? t("run_progress", { step: s.run_step + 1, total: s.steps.length, state }) : state;
}

// detailText shows a fixed runner detail in the panel's language and any other detail as it came.
function detailText(s: Schedule, t: TFunction): string {
  const restarted = s.last_detail.endsWith(RESTARTED);
  const base = restarted ? s.last_detail.slice(0, -RESTARTED.length) : s.last_detail;
  const key = DETAIL_KEYS[base];
  if (key) return restarted ? `${t(key)} ${t("detail_restarted")}` : t(key);
  if (restarted) return s.last_detail;
  return s.last_result === "ok" && s.last_detail ? t("reply", { text: s.last_detail }) : s.last_detail;
}

function commandsOf(steps: ScheduleStep[]): string[] {
  return steps.filter((step) => step.type === "command").map((step) => step.command ?? "");
}

/** Disabled buttons take no pointer events, so the reason rides a wrapper. */
function Why({ why, children }: { why?: string; children: React.ReactNode }) {
  return (
    <span title={why} className="inline-flex">
      {children}
    </span>
  );
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
  const focusAfter = useRef<string | null>(null);
  const browser = browserZone();
  const zones = useMemo(() => zoneList(browser), [browser]);
  const bad = problems(form, t, Date.now(), editing?.enabled ? editing.run_count : 0);
  const labelLength = [...form.label.trim()].length;
  const stepId = (key: number, part: string) => `${id}-step-${key}-${part}`;

  // Moving or removing a step re-renders the row the keyboard was on, so focus is placed again.
  useEffect(() => {
    const target = focusAfter.current;
    focusAfter.current = null;
    if (target) document.getElementById(target)?.focus();
  }, [form.steps]);

  function set<K extends keyof Form>(key: K, value: Form[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  function setSteps(update: (steps: StepDraft[]) => StepDraft[]) {
    setForm((f) => settle({ ...f, steps: update(f.steps) }));
  }

  function setStep(key: number, patch: Partial<StepDraft>) {
    setSteps((steps) => steps.map((s) => (s.key === key ? { ...s, ...patch } : s)));
  }

  function setType(key: number, type: ScheduleStepType) {
    setSteps((steps) =>
      steps.map((s) =>
        s.key === key ? { ...s, type, seconds: type === "delay" && !s.seconds ? DEFAULT_DELAY_SECONDS : s.seconds } : s,
      ),
    );
  }

  function addStep() {
    const step = draftOf({ type: "command" });
    focusAfter.current = stepId(step.key, "type");
    setSteps((steps) => (steps.length < MAX_STEPS ? [...steps, step] : steps));
  }

  function removeStep(key: number) {
    focusAfter.current = `${id}-add-step`;
    setSteps((steps) => (steps.length > 1 ? steps.filter((s) => s.key !== key) : steps));
  }

  function moveStep(index: number, by: -1 | 1) {
    const to = index + by;
    const last = form.steps.length - 1;
    if (to < 0 || to > last) return;
    // The button just pressed is disabled at the end of the list, so focus takes the opposite one.
    const atEnd = by < 0 ? to === 0 : to === last;
    focusAfter.current = stepId(form.steps[index].key, (by < 0) !== atEnd ? "up" : "down");
    setSteps((steps) => {
      const next = [...steps];
      [next[index], next[to]] = [next[to], next[index]];
      return next;
    });
  }

  const templates: [string, () => ScheduleStep[]][] = [
    [
      "tpl_remind_restart",
      () => [{ type: "command", command: t("tpl_remind_restart_command") }, { type: "delay", seconds: 60 }, { type: "restart" }],
    ],
    ["tpl_stop_backup_start", () => [{ type: "stop" }, { type: "backup" }, { type: "start" }]],
    ["tpl_save_backup", () => [{ type: "command", command: "save-all" }, { type: "backup" }]],
  ];

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

  const everyOptions = powers(form.steps) ? EVERY_OPTIONS.filter((m) => m >= HOURLY) : EVERY_OPTIONS;

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
          <fieldset className="grid gap-3">
            <legend className="mb-2 text-sm font-medium">{t("field_steps")}</legend>
            <div className="flex flex-wrap items-center gap-1.5">
              <span className="text-xs text-muted-foreground">{t("templates_label")}</span>
              {templates.map(([key, steps]) => (
                <Button key={key} type="button" size="sm" variant="outline" onClick={() => setSteps(() => draftsOf(steps()))}>
                  {t(key)}
                </Button>
              ))}
            </div>
            <ol className="grid gap-2">
              {form.steps.map((step, index) => {
                const n = index + 1;
                const Icon = STEP_ICONS[step.type];
                const note = bad[`step-${step.key}`];
                return (
                  <li
                    key={step.key}
                    className="grid gap-2 rounded-md border border-border p-3 sm:grid-cols-[1.5rem_11rem_minmax(0,1fr)_auto] sm:items-start"
                  >
                    <span className="hidden h-9 items-center text-xs font-medium tabular-nums text-muted-foreground sm:flex" aria-hidden="true">
                      {n}
                    </span>
                    <Select value={step.type} onValueChange={(v) => setType(step.key, v as ScheduleStepType)}>
                      <SelectTrigger id={stepId(step.key, "type")} aria-label={t("step_type_label", { n })}>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {STEP_TYPES.map((type) => (
                          <SelectItem key={type} value={type}>
                            {t(`step_${type}`)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <div className="grid gap-1">
                      {step.type === "command" && (
                        <Input
                          className="font-mono"
                          value={step.command}
                          onChange={(e) => setStep(step.key, { command: e.target.value })}
                          placeholder={t("command_placeholder")}
                          aria-label={t("step_command_label", { n })}
                          autoComplete="off"
                          spellCheck={false}
                          aria-invalid={!!note}
                        />
                      )}
                      {step.type === "delay" && (
                        <div className="flex items-center gap-2">
                          <Input
                            type="number"
                            inputMode="numeric"
                            min={1}
                            max={MAX_DELAY_SECONDS}
                            step={1}
                            className="w-28"
                            value={step.seconds}
                            onChange={(e) => setStep(step.key, { seconds: e.target.value })}
                            aria-label={t("step_seconds_label", { n })}
                            aria-invalid={!!note}
                          />
                          <span className="text-sm text-muted-foreground">{t("seconds_unit")}</span>
                        </div>
                      )}
                      {step.type !== "command" && step.type !== "delay" && (
                        <p className="flex items-start gap-1.5 py-2 text-xs text-muted-foreground">
                          <Icon className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden="true" />
                          {t(`step_${step.type}_desc`)}
                        </p>
                      )}
                      <FieldNote error={note} hint={step.type === "command" ? t("command_hint") : undefined} />
                    </div>
                    <div className="flex gap-1 sm:justify-end">
                      <Button
                        type="button"
                        size="icon"
                        variant="ghost"
                        id={stepId(step.key, "up")}
                        aria-label={t("step_up", { n })}
                        disabled={index === 0}
                        onClick={() => moveStep(index, -1)}
                      >
                        <ArrowUp />
                      </Button>
                      <Button
                        type="button"
                        size="icon"
                        variant="ghost"
                        id={stepId(step.key, "down")}
                        aria-label={t("step_down", { n })}
                        disabled={index === form.steps.length - 1}
                        onClick={() => moveStep(index, 1)}
                      >
                        <ArrowDown />
                      </Button>
                      <Button
                        type="button"
                        size="icon"
                        variant="ghost"
                        aria-label={t("step_remove", { n })}
                        disabled={form.steps.length === 1}
                        onClick={() => removeStep(step.key)}
                      >
                        <X />
                      </Button>
                    </div>
                  </li>
                );
              })}
            </ol>
            <FieldNote error={bad.steps} hint={t("steps_hint", { max: MAX_STEPS })} />
            <Why why={form.steps.length >= MAX_STEPS ? t("steps_limit", { max: MAX_STEPS }) : undefined}>
              <Button
                type="button"
                size="sm"
                variant="outline"
                id={`${id}-add-step`}
                disabled={form.steps.length >= MAX_STEPS}
                onClick={addStep}
              >
                <Plus />
                {t("add_step")}
              </Button>
            </Why>
          </fieldset>

          <fieldset className="grid gap-3">
            <legend className="mb-2 text-sm font-medium">{t("field_when")}</legend>
            <SegmentedControl label={t("field_when")} value={form.mode} onChange={(m) => set("mode", m)}
              segments={(["daily", "interval", "once"] as const).map((m) => ({ key: m, label: t(`mode_${m}`) }))} />
            {form.mode === "daily" && (
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
            )}
            {form.mode === "interval" && (
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
                  {powers(form.steps) && ` ${t("interval_hourly_limit")}`}
                </p>
              </div>
            )}
            {form.mode === "once" && (
              <div className="grid gap-2">
                <Label htmlFor={`${id}-once`}>{t("field_once")}</Label>
                <Input
                  id={`${id}-once`}
                  type="datetime-local"
                  className="w-56"
                  value={form.once}
                  onChange={(e) => set("once", e.target.value)}
                  aria-invalid={!!bad.once}
                />
                <FieldNote error={bad.once} hint={t("once_hint")} />
              </div>
            )}
          </fieldset>

          {form.mode !== "once" && (
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
          )}

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

          {form.mode !== "once" && (
            <div className="grid gap-2">
              <Label htmlFor={`${id}-max-runs`}>{t("field_max_runs")}</Label>
              <Input
                id={`${id}-max-runs`}
                type="number"
                inputMode="numeric"
                min={0}
                max={MAX_RUNS}
                step={1}
                className="w-36"
                value={form.maxRuns}
                placeholder={t("max_runs_placeholder")}
                onChange={(e) => set("maxRuns", e.target.value)}
                aria-invalid={!!bad.maxRuns}
              />
              <FieldNote error={bad.maxRuns} hint={t("max_runs_hint")} />
            </div>
          )}

          {warns(form.steps) && (
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

          {form.steps.some((s) => s.type === "backup") && (
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
  const lead = s.steps.find((step) => step.type !== "delay") ?? s.steps[0];
  const Icon = lead ? STEP_ICONS[lead.type] : Terminal;
  const title = titleOf(s, t);
  const commands = commandsOf(s.steps);
  const busy = s.run_state !== "";
  const why = busy ? t("busy_hint") : undefined;
  const result = s.last_result ? RESULT_STYLE[s.last_result] : null;
  const detail = detailText(s, t);

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
            <p className={cn("min-w-0 truncate text-sm font-medium", !s.enabled && "text-muted-foreground")} title={title}>
              {title}
            </p>
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
                {progress(s, t)}
              </Badge>
            )}
          </div>
          {s.label && <p className="text-xs text-muted-foreground">{stepsText(s.steps, t)}</p>}
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
            {s.max_runs > 0 && (
              <span className="text-xs">{t("runs_progress", { count: s.run_count, max: s.max_runs })}</span>
            )}
          </p>
          {commands.map((command, i) => (
            <code key={i} className="block w-fit max-w-full truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs" title={command}>
              {command}
            </code>
          ))}
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

  // The switch resaves the whole schedule with `enabled` flipped, and a one-time task whose moment has passed opens the editor instead.
  async function toggle(s: Schedule) {
    if (!s.enabled && s.run_once_at && Date.parse(s.run_once_at) <= Date.now()) {
      setMsg(null);
      setEditor({ editing: s, form: { ...fromSchedule(s), enabled: true } });
      return;
    }
    setToggling(s.id);
    setMsg(null);
    try {
      const input = { ...toInput({ ...fromSchedule(s), enabled: !s.enabled }), run_once_at: s.run_once_at };
      await api.updateSchedule(name, s.id, input);
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
  const templates: [string, LucideIcon, () => Partial<Form>][] = [
    ["template_restart", RotateCw, () => ({})],
    ["template_backup", Archive, () => ({ steps: draftsOf([{ type: "backup" }]), time: "05:00" })],
    [
      "template_announce",
      Megaphone,
      () => ({
        steps: draftsOf([{ type: "command", command: t("template_announce_command") }]),
        mode: "interval",
        every: 30,
        warn: 0,
      }),
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
                <Button key={key} size="sm" variant="outline" onClick={() => openCreate(over())}>
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
              <li>{t("note_steps")}</li>
              <li>{t("note_skip")}</li>
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
              <span className="block">{t("run_body", { steps: stepsText(runTarget.steps, t) })}</span>
              {commandsOf(runTarget.steps).map((command, i) => (
                <code key={i} className="mt-2 block break-all rounded bg-muted px-2 py-1 font-mono text-xs text-foreground">
                  {command}
                </code>
              ))}
              {warns(runTarget.steps) && (
                <span className="mt-3 flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2.5 text-amber-700 dark:text-amber-300">
                  <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
                  {t("run_kick")}
                </span>
              )}
              {runTarget.max_runs > 0 && (
                <span className="mt-3 block">{t("run_counts", { max: runTarget.max_runs })}</span>
              )}
              <span className="mt-3 block">{t("run_keeps_next")}</span>
            </>
          }
          confirmLabel={t("run_confirm")}
          confirmVariant={warns(runTarget.steps) ? "destructive" : "default"}
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
