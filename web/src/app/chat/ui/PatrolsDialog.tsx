import { t as tr } from "@/app/lib/i18n";
import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Icon } from "./Icon";
import type { ChatSchedule, ConversationApprovalMode } from "./history";
import {
  APPROVAL_MODE_OPTIONS,
  approvalModeLabel,
  attentionScheduleId,
  formatScheduleCadence,
  formatScheduleMeta,
  pendingApprovalText,
} from "./PatrolBanner";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { StatusChip } from "@/app/shared/ui/StatusChip";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { describeCronExpression } from "@/app/shared/lib/cron";
import { browserTimeZone, formatTimeZoneLabel } from "@/app/shared/lib/timezones";
import { PatrolSchedulePicker } from "@/app/shared/ui/PatrolSchedulePicker";
import type { UpdateScheduleBody } from "@/app/shared/lib/patrolApi";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { openedByTouch } from "./toolsPicker";

export type CreatePatrolInput = {
  title: string;
  prompt: string;
  cron?: string;
  timezone?: string;
  durationSeconds?: number;
  autoApprove: boolean;
  approvalMode?: ConversationApprovalMode;
  /** Add alongside the chat's existing patrols; without it the server
   *  updates the latest recurring patrol in place. */
  allowMultiple?: boolean;
};

type Outcome = boolean | void | Promise<boolean | void>;

// ── PatrolForm: one form for "New patrol or timer" and "Edit patrol" ───────

const REPEAT_PRESETS = [
  { id: "1h", label: tr("Every 1h"), cron: "0 * * * *" },
  { id: "2h", label: tr("Every 2h"), cron: "0 */2 * * *" },
  { id: "4h", label: tr("Every 4h"), cron: "0 */4 * * *" },
  { id: "6h", label: tr("Every 6h"), cron: "0 */6 * * *" },
  { id: "weekdays", label: tr("Weekdays 9am"), cron: "0 9 * * 1-5" },
  { id: "daily", label: tr("Daily 9am"), cron: "0 9 * * *" },
  { id: "custom", label: tr("Custom"), cron: "" },
] as const;

const ONCE_PRESETS = [
  { id: "t30m", label: tr("In 30m"), seconds: 1800 },
  { id: "t2h", label: tr("In 2h"), seconds: 7200 },
  { id: "tcustom", label: tr("Custom"), seconds: 0 },
] as const;

type PresetId =
  | (typeof REPEAT_PRESETS)[number]["id"]
  | (typeof ONCE_PRESETS)[number]["id"];

const CUSTOM_TIMER_PRESETS = [
  { label: tr("15m"), value: 15, unit: "minutes" as const },
  { label: tr("45m"), value: 45, unit: "minutes" as const },
  { label: tr("1h"), value: 1, unit: "hours" as const },
  { label: tr("3h"), value: 3, unit: "hours" as const },
  { label: tr("4h"), value: 4, unit: "hours" as const },
  { label: tr("8h"), value: 8, unit: "hours" as const },
];

const DEFAULT_TASK = tr("Check for new updates, verify key metrics, and summarize anything that needs attention.");
const DEFAULT_CUSTOM_CRON = "0 9 * * 1-5";

const LABEL_CLASS =
  "font-[family-name:var(--font-code)] text-[0.6875rem] font-medium uppercase tracking-[0.1em] text-[var(--color-text-subtle)]";

type FormStart = {
  presetId: PresetId;
  customCron: string;
  customTz: string;
  timerAmount: number;
  timerUnit: "minutes" | "hours";
};

// Where an existing schedule sits in the form: a preset when its cron (in
// the browser's zone, which is what presets use) or duration matches one,
// else Custom with its own cron/zone or duration.
function formStartFor(sc: ChatSchedule | undefined): FormStart {
  const base: FormStart = {
    presetId: "2h",
    customCron: DEFAULT_CUSTOM_CRON,
    customTz: browserTimeZone(),
    timerAmount: 45,
    timerUnit: "minutes",
  };
  if (!sc) return base;
  if (sc.kind === "timer") {
    const secs = sc.duration_seconds ?? 0;
    const once = ONCE_PRESETS.find((p) => p.seconds > 0 && p.seconds === secs);
    if (once) return { ...base, presetId: once.id };
    const hours = secs >= 3600 && secs % 3600 === 0;
    return {
      ...base,
      presetId: "tcustom",
      timerAmount: Math.max(1, hours ? secs / 3600 : Math.round(secs / 60)),
      timerUnit: hours ? "hours" : "minutes",
    };
  }
  const cron = (sc.cron ?? sc.cron_expr ?? "").trim();
  const tz = sc.timezone || base.customTz;
  const repeat = REPEAT_PRESETS.find((p) => p.cron && p.cron === cron);
  if (repeat && tz === base.customTz) return { ...base, presetId: repeat.id };
  return { ...base, presetId: "custom", customCron: cron || DEFAULT_CUSTOM_CRON, customTz: tz };
}

function formatRunOnce(seconds: number): string {
  const at = new Date(Date.now() + seconds * 1000);
  const time = at.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
  const sameDay = at.toDateString() === new Date().toDateString();
  return sameDay
    ? tr("Runs once · today at {0}", [time])
    : tr("Runs once · {0}", [
        at.toLocaleString(undefined, { weekday: "short", hour: "numeric", minute: "2-digit" }),
      ]);
}

export type PatrolFormValues = {
  title: string;
  prompt: string;
  kind: "patrol" | "timer";
  cron?: string;
  timezone?: string;
  durationSeconds?: number;
  approvalMode: ConversationApprovalMode;
};

export function PatrolForm({
  mode,
  schedule,
  initialPrompt,
  approvalMode,
  showApprovalMode = true,
  autonomousAccepted,
  onRequireAutonomousAccept,
  canCancel,
  onCancel,
  onSubmit,
}: {
  mode: "new" | "edit";
  /** The schedule being edited (edit mode). */
  schedule?: ChatSchedule;
  initialPrompt?: string;
  approvalMode: ConversationApprovalMode;
  showApprovalMode?: boolean;
  autonomousAccepted?: boolean;
  onRequireAutonomousAccept?: (onAccepted: () => void) => void;
  canCancel: boolean;
  onCancel?: () => void;
  onSubmit: (values: PatrolFormValues) => Outcome;
}) {
  const isEdit = mode === "edit";
  const [start] = useState(() => formStartFor(schedule));
  const [presetId, setPresetId] = useState<PresetId>(start.presetId);
  const [customCron, setCustomCron] = useState(start.customCron);
  const [customTz, setCustomTz] = useState(start.customTz);
  const [timerAmount, setTimerAmount] = useState(start.timerAmount);
  const [timerUnit, setTimerUnit] = useState<"minutes" | "hours">(start.timerUnit);
  const [task, setTask] = useState<string>(() =>
    schedule?.prompt ?? ((initialPrompt ?? "").trim() || DEFAULT_TASK),
  );
  const [label, setLabel] = useState<string>(() => (isEdit ? schedule?.title ?? "" : ""));
  const [approval, setApproval] = useState<ConversationApprovalMode>(
    isEdit ? approvalMode : approvalMode === "manual" ? "auto" : approvalMode,
  );
  const [submitting, setSubmitting] = useState(false);
  const idPrefix = `patrol-form-${mode}-${schedule?.id ?? "new"}`;

  const repeat = REPEAT_PRESETS.find((p) => p.id === presetId);
  const once = ONCE_PRESETS.find((p) => p.id === presetId);
  const kind: "patrol" | "timer" = once ? "timer" : "patrol";
  const maxTimerAmount = timerUnit === "hours" ? 168 : 1440;
  const clampedTimer = Math.min(maxTimerAmount, Math.max(1, Math.round(timerAmount || 1)));
  const durationSeconds = once
    ? once.id === "tcustom"
      ? clampedTimer * (timerUnit === "hours" ? 3600 : 60)
      : once.seconds
    : undefined;
  const cron = repeat ? (repeat.id === "custom" ? customCron.trim() : repeat.cron) : undefined;
  const timezone = repeat?.id === "custom" ? customTz.trim() || "UTC" : browserTimeZone();
  const labelPlaceholder = isEdit
    ? schedule?.title || tr("Recurring Patrol")
    : kind === "timer"
      ? tr("Reminder")
      : tr("Recurring Patrol");
  const canSubmit = task.trim().length > 0 && (kind === "timer" || Boolean(cron)) && !submitting;
  const summary =
    kind === "timer"
      ? formatRunOnce(durationSeconds ?? 0)
      : cron
        ? formatScheduleCadence({ kind: "patrol", cron } as ChatSchedule) ||
          describeCronExpression(cron)
        : tr("Choose when it should run");
  const approvalDesc = APPROVAL_MODE_OPTIONS.find((o) => o.id === approval)?.desc ?? "";

  const chip = (id: PresetId, text: string) => {
    const selected = id === presetId;
    return (
      <button
        key={id}
        type="button"
        aria-pressed={selected}
        onClick={() => setPresetId(id)}
        className="pill-btn hit-area-touch h-[34px] px-3.5 text-[0.8125rem]"
      >
        {text}
      </button>
    );
  };

  return (
    <form
      data-testid={isEdit ? "patrol-form-edit" : "patrol-form-new"}
      className="grid gap-5 rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-4"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!canSubmit) return;
        setSubmitting(true);
        try {
          await onSubmit({
            title: label.trim() || labelPlaceholder,
            prompt: task.trim(),
            kind,
            cron,
            timezone: kind === "patrol" ? timezone : undefined,
            durationSeconds,
            approvalMode: approval,
          });
        } finally {
          setSubmitting(false);
        }
      }}
    >
      <div className="flex items-center gap-2.5">
        <span aria-hidden="true" className="picker-icon-tile size-7 shrink-0">
          <Icon name={isEdit ? "edit" : "plus"} className="size-3.5" />
        </span>
        <h3 className="m-0 text-[0.9375rem] font-semibold text-[var(--color-text-primary)]">
          {isEdit ? tr("Edit patrol") : tr("New patrol or timer")}
        </h3>
      </div>

      <label className="grid gap-2">
        <span className={LABEL_CLASS}>{tr("What to check & do each run")}</span>
        <Textarea
          id={`${idPrefix}-task`}
          rows={3}
          value={task}
          onChange={(e) => setTask(e.target.value)}
          placeholder={tr("e.g. Check pacing on active deals and flag anything under 90%")}
          className="min-h-[5.25rem] resize-y bg-[var(--color-surface-1)] px-3 py-2.5 text-[0.875rem] leading-[1.5]"
        />
      </label>

      <fieldset className="m-0 grid min-w-0 gap-3 border-0 p-0">
        <legend className={`${LABEL_CLASS} mb-2.5 p-0`}>{tr("Schedule")}</legend>
        <div className="grid gap-2">
          <span className="text-[0.75rem] text-[var(--color-text-muted)]">{tr("Repeats")}</span>
          <div className="flex flex-wrap gap-2" role="group" aria-label={tr("Repeats")}>
            {REPEAT_PRESETS.map((p) => chip(p.id, p.label))}
          </div>
        </div>
        <div className="grid gap-2">
          <span className="text-[0.75rem] text-[var(--color-text-muted)]">{tr("Once (timer)")}</span>
          <div className="flex flex-wrap gap-2" role="group" aria-label={tr("Once (timer)")}>
            {ONCE_PRESETS.map((p) => chip(p.id, p.label))}
          </div>
        </div>
        {presetId === "custom" ? (
          <div data-testid="patrol-custom-builder">
            <PatrolSchedulePicker
              compact
              cron={customCron}
              timezone={customTz}
              onChangeCron={setCustomCron}
              onChangeTimezone={setCustomTz}
              testIdPrefix="patrol-popover-schedule"
            />
          </div>
        ) : null}
        {presetId === "tcustom" ? (
          <div
            data-testid="patrol-custom-timer"
            className="grid gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-inset)] p-3"
          >
            <div className="flex flex-wrap gap-1.5">
              {CUSTOM_TIMER_PRESETS.map((tp) => (
                <button
                  key={tp.label}
                  type="button"
                  aria-pressed={timerAmount === tp.value && timerUnit === tp.unit}
                  onClick={() => {
                    setTimerAmount(tp.value);
                    setTimerUnit(tp.unit);
                  }}
                  className="pill-btn hit-area-touch h-6 px-2.5 text-[0.72rem]"
                >
                  {tr("In {0}", [tp.label])}
                </button>
              ))}
            </div>
            <div className="grid grid-cols-2 gap-2">
              <label className="grid gap-1 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
                <span>{tr("Run once in")}</span>
                <Input
                  type="number"
                  min={1}
                  max={maxTimerAmount}
                  aria-label={tr("Timer duration")}
                  value={timerAmount}
                  onChange={(e) =>
                    setTimerAmount(
                      Math.min(maxTimerAmount, Math.max(1, Math.round(Number(e.target.value) || 1))),
                    )
                  }
                  className="h-9 bg-[var(--color-surface-1)] px-2.5 text-[0.8125rem]"
                />
              </label>
              <label className="grid gap-1 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
                <span>{tr("Unit")}</span>
                <NativeSelect
                  aria-label={tr("Timer unit")}
                  value={timerUnit}
                  onChange={(e) => {
                    const nextUnit = e.target.value as "minutes" | "hours";
                    const nextMax = nextUnit === "hours" ? 168 : 1440;
                    setTimerUnit(nextUnit);
                    setTimerAmount((prev) => Math.min(nextMax, Math.max(1, Math.round(prev || 1))));
                  }}
                  className="h-9 bg-[var(--color-surface-1)] pr-8 pl-2.5 text-[0.8125rem]"
                >
                  <NativeSelectOption value="minutes">{tr("Minutes")}</NativeSelectOption>
                  <NativeSelectOption value="hours">{tr("Hours")}</NativeSelectOption>
                </NativeSelect>
              </label>
            </div>
          </div>
        ) : null}
      </fieldset>

      {showApprovalMode ? (
        <div className="grid gap-2">
          <span id={`${idPrefix}-approval`} className={`${LABEL_CLASS} flex items-center gap-1.5`}>
            <Icon name="zap" className="size-3 text-[var(--color-accent)]" />
            {tr("Approval mode while patrolling")}
          </span>
          <ApprovalSegmented
            labelledBy={`${idPrefix}-approval`}
            value={approval}
            onChange={(next) => {
              if (next === "autonomous" && autonomousAccepted === false && onRequireAutonomousAccept) {
                onRequireAutonomousAccept(() => setApproval("autonomous"));
                return;
              }
              setApproval(next);
            }}
          />
          <p className="m-0 text-[0.78rem] leading-[1.45] text-[var(--color-text-muted)]">{approvalDesc}</p>
        </div>
      ) : null}

      <label className="grid gap-2">
        <span className={LABEL_CLASS}>
          {tr("Label")}{" "}
          <span className="tracking-normal normal-case text-[var(--color-text-subtle)]">{tr("(optional)")}</span>
        </span>
        <Input
          type="text"
          value={label}
          onChange={(e) => setLabel(e.target.value)}
          placeholder={labelPlaceholder}
          className="h-[42px] bg-[var(--color-surface-1)] px-3 text-[0.875rem]"
        />
      </label>

      <div className="flex flex-wrap items-center gap-2.5 border-t border-[var(--color-divider)] pt-4">
        <div className="flex min-w-[12.5rem] flex-1 items-start gap-2">
          <Icon name="clock" className="mt-px size-4 shrink-0 text-[var(--color-accent)]" />
          <div className="grid gap-0.5">
            <span data-testid="patrol-form-summary" className="text-[0.8125rem] text-[var(--color-text-primary)]">
              {summary}
            </span>
            {kind === "patrol" ? (
              <span className="text-[0.75rem] text-[var(--color-text-subtle)]">
                {tr("Times in {0}", [formatTimeZoneLabel(timezone)])}
              </span>
            ) : null}
          </div>
        </div>
        {canCancel ? (
          <Button type="button" variant="outline" onClick={onCancel}>
            {tr("Cancel")}
          </Button>
        ) : null}
        <Button type="submit" disabled={!canSubmit}>
          {submitting
            ? isEdit
              ? tr("Saving…")
              : tr("Starting…")
            : isEdit
              ? tr("Save changes")
              : kind === "timer"
                ? tr("Set timer")
                : tr("Start patrol")}
        </Button>
      </div>
    </form>
  );
}

function ApprovalSegmented({
  value,
  onChange,
  labelledBy,
  label,
  testIdPrefix,
}: {
  value: ConversationApprovalMode;
  onChange: (mode: ConversationApprovalMode) => void;
  labelledBy?: string;
  label?: string;
  testIdPrefix?: string;
}) {
  return (
    <div
      role="radiogroup"
      aria-labelledby={labelledBy}
      aria-label={label}
      className="grid grid-cols-3 gap-1 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-1"
    >
      {APPROVAL_MODE_OPTIONS.map((opt) => {
        const selected = opt.id === value;
        return (
          <button
            key={opt.id}
            type="button"
            role="radio"
            aria-checked={selected}
            data-testid={testIdPrefix ? `${testIdPrefix}-${opt.id}` : undefined}
            onClick={() => onChange(opt.id)}
            className={`h-9 rounded-[calc(var(--radius-md)-2px)] text-[0.8125rem] font-medium transition ${
              selected
                ? opt.id === "autonomous"
                  ? "bg-[var(--color-warning-tint)] text-[var(--color-warning-text)] shadow-[inset_0_0_0_1px_var(--color-warning-border)]"
                  : "bg-[var(--color-selected-bg)] text-[var(--color-text-primary)] shadow-[inset_0_0_0_1px_var(--color-selected-border)]"
                : "text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
            }`}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}

// ── PatrolsDialog: the clock's "Patrols & timers" manager ───────────────────

export function PatrolsDialog({
  schedules,
  approvalMode,
  canChangeApprovalMode,
  autonomousAccepted,
  onRequireAutonomousAccept,
  onChangeApprovalMode,
  barShown,
  onSetBarShown,
  canCreate = true,
  pendingApprovals = 0,
  onReview,
  isStreaming,
  initialPrompt,
  onRunNow,
  onTogglePause,
  onRemove,
  onSave,
  onStart,
  onClose,
}: {
  /** The conversation's armed or paused patrols and timers. */
  schedules: ChatSchedule[];
  approvalMode: ConversationApprovalMode;
  canChangeApprovalMode: boolean;
  autonomousAccepted?: boolean;
  onRequireAutonomousAccept?: (onAccepted: () => void) => void;
  onChangeApprovalMode?: (mode: ConversationApprovalMode) => void;
  barShown: boolean;
  onSetBarShown: (shown: boolean) => void;
  /** False in a sealed (lockdown) chat: existing patrols can be managed,
   *  new ones cannot be started from here. */
  canCreate?: boolean;
  pendingApprovals?: number;
  onReview?: () => void;
  isStreaming?: boolean;
  initialPrompt?: string;
  onRunNow: (scheduleId: string) => unknown;
  onTogglePause: (scheduleId: string, status: ChatSchedule["status"]) => unknown;
  onRemove: (scheduleId: string) => unknown;
  onSave: (
    scheduleId: string,
    body: UpdateScheduleBody,
    approvalMode: ConversationApprovalMode,
  ) => Outcome;
  onStart: (input: CreatePatrolInput) => Outcome;
  onClose: () => void;
}) {
  const hasPatrols = schedules.length > 0;
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [confirmRemoveId, setConfirmRemoveId] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [justAddedId, setJustAddedId] = useState<string | null>(null);
  const [nowSec, setNowSec] = useState(() => Math.floor(Date.now() / 1000));
  const knownIdsRef = useRef<Set<string> | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const firstFieldRef = useRef<HTMLButtonElement | null>(null);
  const showForm = canCreate && (!hasPatrols || adding);

  useDialogA11y(true, panelRef, onClose, {
    initialFocusRef: openedByTouch() || !hasPatrols ? panelRef : firstFieldRef,
  });

  useEffect(() => {
    const id = window.setInterval(() => setNowSec(Math.floor(Date.now() / 1000)), 10_000);
    return () => window.clearInterval(id);
  }, []);

  // Outline the card a start just added, so the new patrol is easy to find.
  useEffect(() => {
    const ids = new Set(schedules.map((s) => s.id));
    const known = knownIdsRef.current;
    if (known) {
      const added = schedules.find((s) => !known.has(s.id));
      if (added) setJustAddedId(added.id);
    }
    knownIdsRef.current = ids;
  }, [schedules]);

  const attentionId = pendingApprovals > 0 ? attentionScheduleId(schedules) : null;

  const runBusy = async (id: string, fn: () => unknown): Promise<unknown> => {
    setBusyId(id);
    try {
      return await fn();
    } finally {
      setBusyId(null);
    }
  };

  const content = (
    <div className="scrim fixed inset-0 z-[1200] flex items-stretch justify-center p-0 sm:items-center sm:p-4">
      <button
        type="button"
        tabIndex={-1}
        aria-hidden="true"
        onClick={onClose}
        className="absolute inset-0 cursor-default"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby="patrols-dialog-title"
        data-testid="patrols-dialog"
        className="picker-modal relative z-10 outline-none sm:max-w-[37.5rem] sm:[max-height:min(52rem,calc(100dvh-4rem))]"
      >
        <header className="picker-modal__head items-start gap-3.5 border-b border-[var(--color-divider)] !pb-4 sm:!pl-6">
          <span aria-hidden="true" className="picker-icon-tile size-10 shrink-0">
            <Icon name="clock" className="size-5" />
          </span>
          <div className="grid min-w-0 flex-1 gap-1">
            <h2 id="patrols-dialog-title" className="picker-modal__title">
              {tr("Patrols & timers")}
            </h2>
            <p className="m-0 text-[0.8125rem] leading-[1.45] text-[var(--color-text-muted)]">
              {tr("Wake this conversation on your cadence with full history, workspace files, and enabled connectors.")}
            </p>
          </div>
          <CloseButton label={tr("Close patrols")} onClick={onClose} className="any-pointer-coarse:size-11" />
        </header>

        <div className="picker-modal__body grid content-start gap-5 sm:!px-6 sm:!pt-5 sm:!pb-6">
          {hasPatrols ? (
            <section aria-labelledby="patrols-dialog-active" className="grid gap-2.5">
              <div className="flex flex-wrap items-center gap-2">
                <h3 id="patrols-dialog-active" className={`${LABEL_CLASS} m-0`}>
                  {tr("Active in this chat")}
                </h3>
                <span className="rounded-[var(--radius-pill)] bg-[var(--color-selected-bg)] px-[7px] py-0.5 font-[family-name:var(--font-code)] text-[0.6875rem] text-[var(--color-accent)]">
                  {schedules.length}
                </span>
                <span className="flex-1" />
                <label className="inline-flex cursor-pointer items-center gap-2.5 text-[0.78rem] text-[var(--color-text-secondary)]">
                  {tr("Show bar above composer")}
                  <Switch
                    data-testid="patrols-show-bar"
                    checked={barShown}
                    onCheckedChange={(v) => onSetBarShown(Boolean(v))}
                  />
                </label>
              </div>

              <ul className="m-0 grid list-none gap-2 p-0">
                {schedules.map((sc, idx) => {
                  const isPaused = sc.status === "paused";
                  const isExpanded = expandedId === sc.id;
                  const isEditing = editingId === sc.id;
                  const attention = sc.id === attentionId;
                  const isConfirm = confirmRemoveId === sc.id;
                  const busy = busyId === sc.id || Boolean(isStreaming);
                  const meta = [formatScheduleMeta(sc, nowSec), approvalModeLabel(approvalMode)].join(" · ");
                  return (
                    <li
                      key={sc.id}
                      data-testid="patrols-card"
                      className={`overflow-hidden rounded-[var(--radius-lg)] border bg-[var(--color-surface-2)] ${
                        justAddedId === sc.id
                          ? "border-[var(--color-accent)]"
                          : isExpanded
                            ? "border-[var(--color-selected-border)]"
                            : "border-[var(--color-border)]"
                      }`}
                    >
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 py-1.5 pr-2.5 pl-1">
                        <button
                          ref={idx === 0 ? firstFieldRef : undefined}
                          type="button"
                          aria-expanded={isExpanded}
                          onClick={() => {
                            setExpandedId(isExpanded ? null : sc.id);
                            setEditingId(null);
                            setConfirmRemoveId(null);
                            setJustAddedId(null);
                          }}
                          className="flex min-h-[52px] min-w-0 flex-[1_1_15rem] items-center gap-2.5 rounded-[var(--radius-md)] px-2 py-1.5 text-left transition hover:bg-[var(--color-overlay-soft)]"
                        >
                          <Icon
                            name="chevron-down"
                            className={`size-4 shrink-0 text-[var(--color-text-subtle)] transition-transform ${
                              isExpanded ? "" : "-rotate-90"
                            }`}
                          />
                          <span
                            aria-hidden="true"
                            className={`inline-block size-2 shrink-0 rounded-full ${
                              isPaused
                                ? "border-2 border-[var(--color-text-muted)]"
                                : attention
                                  ? "bg-[var(--color-warning)] shadow-[0_0_0_3px_var(--color-warning-tint)]"
                                  : "bg-[var(--color-status-success-fg,#10b981)]"
                            }`}
                          />
                          <span className="grid min-w-0 flex-1 gap-[3px]">
                            <span className="flex min-w-0 items-center gap-2">
                              <StatusChip tone={sc.kind === "timer" ? "neutral" : "accent"} caps className="shrink-0">
                                {sc.kind === "timer" ? tr("timer") : tr("patrol")}
                              </StatusChip>
                              <span className="min-w-0 truncate text-[0.875rem] font-semibold text-[var(--color-text-primary)]">
                                {sc.title}
                              </span>
                            </span>
                            <span className="min-w-0 truncate text-[0.78rem] text-[var(--color-text-muted)]">
                              {meta}
                            </span>
                          </span>
                        </button>
                        <div className="ml-auto flex shrink-0 items-center gap-2 pb-1 pl-8 sm:pb-0 sm:pl-0">
                        <button
                          type="button"
                          disabled={busy || isPaused}
                          onClick={() => void runBusy(sc.id, () => onRunNow(sc.id))}
                          className="pill-btn inline-flex h-9 shrink-0 items-center gap-1.5 font-medium disabled:opacity-40"
                        >
                          <Icon name="play" className="size-3" />
                          {tr("Run now")}
                        </button>
                        <button
                          type="button"
                          disabled={busy}
                          aria-label={isPaused ? tr("Resume {0}", [sc.title]) : tr("Pause {0}", [sc.title])}
                          title={isPaused ? tr("Resume {0}", [sc.title]) : tr("Pause {0}", [sc.title])}
                          onClick={() => void runBusy(sc.id, () => onTogglePause(sc.id, sc.status))}
                          className="hit-area inline-flex size-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-40"
                        >
                          <Icon name={isPaused ? "play" : "pause"} className="size-3.5" />
                        </button>
                        </div>
                      </div>

                      {attention ? (
                        <div className="mx-3 mb-3 flex items-center gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-warning-border)] bg-[var(--color-warning-tint)] py-1.5 pr-1.5 pl-3">
                          <Icon name="alert-circle" className="size-4 shrink-0 text-[var(--color-warning-icon)]" />
                          <span className="min-w-0 flex-1 text-[0.8125rem] text-[var(--color-warning-text)]">
                            {pendingApprovalText(pendingApprovals)}
                          </span>
                          {onReview ? (
                            <button
                              type="button"
                              onClick={onReview}
                              className="h-8 shrink-0 rounded-[var(--radius-md)] border border-[var(--color-warning-border)] px-3 text-[0.8125rem] font-medium text-[var(--color-warning-text)] transition hover:bg-[var(--color-warning-tint)]"
                            >
                              {tr("Review in chat")}
                            </button>
                          ) : null}
                        </div>
                      ) : null}

                      {isExpanded && !isEditing ? (
                        <div className="grid gap-4 border-t border-[var(--color-divider)] px-4 pt-3.5 pb-4">
                          {canChangeApprovalMode && onChangeApprovalMode ? (
                            <div className="grid gap-2">
                              <span className={LABEL_CLASS}>{tr("Approval mode")}</span>
                              <ApprovalSegmented
                                label={tr("Approval mode")}
                                testIdPrefix="patrols-mode"
                                value={approvalMode}
                                onChange={(next) => {
                                  if (next !== approvalMode) onChangeApprovalMode(next);
                                }}
                              />
                              <p className="m-0 text-[0.78rem] leading-[1.45] text-[var(--color-text-muted)]">
                                {APPROVAL_MODE_OPTIONS.find((o) => o.id === approvalMode)?.desc}
                                {schedules.length > 1 ? ` ${tr("Applies to every patrol in this chat.")}` : ""}
                              </p>
                            </div>
                          ) : null}
                          <div className="grid gap-1.5">
                            <span className={LABEL_CLASS}>{tr("Each run")}</span>
                            <p className="m-0 text-[0.84rem] leading-[1.5] whitespace-pre-wrap text-[var(--color-text-secondary)]">
                              {sc.prompt}
                            </p>
                          </div>
                          <div className="flex flex-wrap items-center gap-2">
                            <button
                              type="button"
                              onClick={() => {
                                setEditingId(sc.id);
                                setAdding(false);
                                setConfirmRemoveId(null);
                              }}
                              className="pill-btn inline-flex h-9 items-center gap-2 px-3.5 font-medium"
                            >
                              <Icon name="edit" className="size-3.5" />
                              {tr("Edit schedule & task")}
                            </button>
                            <span className="flex-1" />
                            <button
                              type="button"
                              data-testid="patrols-remove"
                              disabled={busy && isConfirm}
                              onClick={async () => {
                                if (!isConfirm) {
                                  setConfirmRemoveId(sc.id);
                                  return;
                                }
                                // A refused or unreachable DELETE leaves the
                                // confirm armed so one click retries it.
                                const ok = await runBusy(sc.id, () => onRemove(sc.id));
                                if (ok === false) return;
                                setConfirmRemoveId(null);
                                setExpandedId(null);
                              }}
                              className={`h-9 rounded-[var(--radius-md)] border px-3 text-[0.8125rem] font-medium transition ${
                                isConfirm
                                  ? "border-[var(--color-status-error-border)] bg-[var(--color-status-error-bg)] text-[var(--color-status-error-fg)]"
                                  : "border-transparent text-[var(--color-status-error-fg)] hover:bg-[var(--color-status-error-bg)]"
                              }`}
                            >
                              {isConfirm
                                ? sc.kind === "timer"
                                  ? tr("Confirm cancel timer")
                                  : tr("Confirm remove")
                                : sc.kind === "timer"
                                  ? tr("Cancel timer")
                                  : tr("Remove patrol")}
                            </button>
                            {isConfirm ? (
                              <span role="status" className="w-full text-right text-[0.75rem] text-[var(--color-text-muted)]">
                                {sc.kind === "timer"
                                  ? tr("The timer won't fire. Click again to confirm.")
                                  : tr("The patrol stops for good. Click again to confirm.")}
                              </span>
                            ) : null}
                          </div>
                        </div>
                      ) : null}

                      {isEditing ? (
                        <div className="border-t border-[var(--color-divider)] p-3">
                          <PatrolForm
                            mode="edit"
                            schedule={sc}
                            approvalMode={approvalMode}
                            showApprovalMode={canChangeApprovalMode}
                            autonomousAccepted={autonomousAccepted}
                            onRequireAutonomousAccept={onRequireAutonomousAccept}
                            canCancel
                            onCancel={() => setEditingId(null)}
                            onSubmit={async (v) => {
                              const ok = await onSave(sc.id, editBody(sc, v), v.approvalMode);
                              if (ok !== false) setEditingId(null);
                              return ok;
                            }}
                          />
                        </div>
                      ) : null}
                    </li>
                  );
                })}
              </ul>
            </section>
          ) : null}

          {!hasPatrols && !canCreate ? (
            <p className="m-0 text-[0.84rem] text-[var(--color-text-muted)]">
              {tr("No patrols or timers in this chat.")}
            </p>
          ) : null}

          {canCreate && hasPatrols && !adding ? (
            <button
              type="button"
              data-testid="patrols-add"
              onClick={() => {
                setAdding(true);
                setEditingId(null);
                setExpandedId(null);
                setJustAddedId(null);
              }}
              className="flex min-h-14 w-full items-center justify-center gap-2.5 rounded-[var(--radius-lg)] border border-dashed border-[var(--color-border-strong)] text-[0.875rem] font-medium text-[var(--color-accent)] transition hover:bg-[var(--color-overlay-soft)]"
            >
              <Icon name="plus" className="size-4" />
              {tr("New patrol or timer")}
            </button>
          ) : null}

          {showForm ? (
            <PatrolForm
              mode="new"
              initialPrompt={initialPrompt}
              approvalMode={approvalMode}
              autonomousAccepted={autonomousAccepted}
              onRequireAutonomousAccept={onRequireAutonomousAccept}
              canCancel={hasPatrols}
              onCancel={() => setAdding(false)}
              onSubmit={async (v) => {
                const ok = await onStart({
                  title: v.title,
                  prompt: v.prompt,
                  cron: v.cron,
                  timezone: v.timezone,
                  durationSeconds: v.durationSeconds,
                  autoApprove: v.approvalMode !== "manual",
                  approvalMode: v.approvalMode,
                  // "New patrol or timer" next to existing patrols adds one;
                  // it must never overwrite the patrol already running.
                  allowMultiple: hasPatrols || undefined,
                });
                if (ok !== false) setAdding(false);
                return ok;
              }}
            />
          ) : null}
        </div>
      </div>
    </div>
  );

  if (typeof document === "undefined") return content;
  return createPortal(content, document.body);
}

// The PATCH body for an edit: only what changed, so saving a new label or
// task never re-arms the schedule (a new cron or duration recomputes its
// next run, and a timer's duration counts from now).
export function editBody(sc: ChatSchedule, v: PatrolFormValues): UpdateScheduleBody {
  const body: UpdateScheduleBody = {};
  if (v.title !== sc.title) body.title = v.title;
  if (v.prompt !== sc.prompt) body.prompt = v.prompt;
  if (v.kind === "patrol") {
    const cur = (sc.cron ?? sc.cron_expr ?? "").trim();
    if (sc.kind === "timer" || v.cron !== cur || (v.timezone && v.timezone !== sc.timezone)) {
      body.cron = v.cron;
      body.timezone = v.timezone;
    }
  } else if (sc.kind !== "timer" || v.durationSeconds !== sc.duration_seconds) {
    body.duration_seconds = v.durationSeconds;
  }
  return body;
}
