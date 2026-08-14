"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import type { CostForecast, McpServer, MCPChoice, Task, TaskCreate, TaskTemplate } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { applyTemplateVars, humanizeVarName, promptableVars } from "@/app/shared/lib/taskTemplates";
import { validateTaskForm, validateCronExpression, describeEmailError } from "@/app/shared/lib/validation";
import { isValidEmail } from "@/app/shared/lib/format";
import { describeCronExpression } from "@/app/shared/lib/cron";
import { Icon } from "@/app/shared/ui/Icon";
import { nextCronOccurrence, formatNextRun } from "@/app/shared/lib/cronNext";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { useToast } from "@/app/shared/ui/Toast";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { ModelPicker } from "@/app/shared/ui/ModelPicker";
import { McpServerPicker } from "@/app/shared/ui/McpServerPicker";
import { FileUpload, type FileUploadHandle, type FileEntry } from "@/app/shared/ui/FileUpload";
import { CostForecastPanel } from "./CostForecastPanel";
import { PromptLibrary } from "@/app/shared/ui/PromptLibrary";

// TaskCreateModal — the create-task form, matching the elevated New Task design
// (states: default · filled · advanced · validation · submitting · scrolled ·
// dirty-close; dark + light). Every live capability survives the redesign:
// prompt, context (documentation · tags · persona), the schedule (now a
// segmented Run now · Run once · Repeat mode — the old both-fields-filled state
// is unrepresentable in the UI; the API still accepts both), email recipients
// (chip input), the full MCP roster with credential accounts, attachments,
// templates, and the Advanced cluster (model · limits · behavior · gate).
//
// Lifecycle contracts:
//  - Esc / ✕ / Cancel / outside-click run requestClose(): clean forms close
//    instantly; dirty forms get the in-modal "Discard this task?" guard; while
//    submitting, close is deferred entirely.
//  - Discard and a successful create RESET the form (the guard's "will be lost"
//    copy is only honest because they do).
//  - Validation runs on blur and on submit; errors render adjacent to their
//    field; Launch is disabled ONLY while the prompt is empty (visible reason
//    in the footer) — with field errors it stays enabled and the footer counts
//    what's blocking.

// TITLE_MAX_LENGTH mirrors the server's maxTaskTitleChars, so an over-long
// title is caught in the form instead of coming back as a 400.
const TITLE_MAX_LENGTH = 120;

// PROMPT_AUTOGROW_MAX_PX caps how tall the prompt field grows on its OWN. It is
// deliberately modest — the schedule, tools and Launch button live below it, and
// an auto-grown field that swallows the form makes them unreachable. Deliberate
// enlargement (the drag grip, the Expand toggle) is bounded far higher by the
// stylesheet's max-height instead.
const PROMPT_AUTOGROW_MAX_PX = 240;

// The expanded-editor preference is per browser: an operator who edits long
// protocol prompts wants the tall pane every time, not once per modal.
const PROMPT_EXPANDED_STORAGE_KEY = "fleet-task-prompt-expanded";

const DEFAULT_PRIMARY_MODEL = "deepseek/deepseek-v4-flash-0731";
const DEFAULT_FALLBACK_MODEL = "x-ai/grok-4.6";

const SCHEDULE_PRESETS = [
  { label: "Weekdays 9am", cron: "0 9 * * 1-5" },
  { label: "Weekly Mon", cron: "0 9 * * 1" },
  { label: "Mon & Thu 1pm", cron: "0 13 * * 1,4" },
  { label: "Wed 5am", cron: "0 5 * * 3" },
];

type ScheduleMode = "now" | "once" | "repeat";
type RepeatEditor = "simple" | "cron";
// When the repeat chain stops: never, at the end of a calendar date, or after
// a total number of runs.
type RepeatEndMode = "never" | "date" | "count";
type SimpleFrequency = "daily" | "weekdays" | "weekly";

const WEEKDAYS = [
  { value: "1", label: "Monday" },
  { value: "2", label: "Tuesday" },
  { value: "3", label: "Wednesday" },
  { value: "4", label: "Thursday" },
  { value: "5", label: "Friday" },
  { value: "6", label: "Saturday" },
  { value: "0", label: "Sunday" },
];

type SimpleSchedule = { frequency: SimpleFrequency; time: string; weekdays: string[] };

function simpleScheduleCron(frequency: SimpleFrequency, time: string, weekdays: string[]): string {
  const [hour = "9", minute = "0"] = (time || "09:00").split(":");
  const day = frequency === "weekdays" ? "1-5" : frequency === "weekly" ? weekdays.join(",") : "*";
  return `${Number(minute)} ${Number(hour)} * * ${day}`;
}

// parseSimpleSchedule hydrates the friendly editor from the subset it can
// represent. Anything more complex stays in Advanced cron rather than being
// shown inaccurately by simpler controls.
function parseSimpleSchedule(raw: string): SimpleSchedule | null {
  const [minuteRaw, hourRaw, monthDay, month, weekDay, ...rest] = raw.trim().split(/\s+/);
  const minute = Number(minuteRaw);
  const hour = Number(hourRaw);
  if (
    rest.length > 0 ||
    !Number.isInteger(minute) ||
    minute < 0 ||
    minute > 59 ||
    !Number.isInteger(hour) ||
    hour < 0 ||
    hour > 23 ||
    monthDay !== "*" ||
    month !== "*"
  ) {
    return null;
  }
  const time = `${String(hour).padStart(2, "0")}:${String(minute).padStart(2, "0")}`;
  if (weekDay === "*") return { frequency: "daily", time, weekdays: ["1"] };
  if (weekDay === "1-5") return { frequency: "weekdays", time, weekdays: ["1"] };
  if (!weekDay) return null;
  const requested = weekDay.split(",");
  if (requested.length === 0 || requested.some((day) => !/^[0-6]$/.test(day))) return null;
  const weekdays = WEEKDAYS.map((day) => day.value).filter((day) => requested.includes(day));
  if (weekdays.length !== new Set(requested).size) return null;
  return { frequency: "weekly", time, weekdays };
}

const SCHEDULE_MODES: Array<{ id: ScheduleMode; label: string }> = [
  { id: "now", label: "Run now" },
  { id: "once", label: "Run once" },
  { id: "repeat", label: "Repeat" },
];

export type TaskCreateModalProps = {
  open: boolean;
  servers: McpServer[];
  serversLoading?: boolean;
  onClose: () => void;
  onCreated: () => void;
  // Edit mode: when set, the form opens prefilled from this task and Save
  // updates it instead of creating. Pending/scheduled tasks edit in place
  // (PUT — the definition, i.e. every future run, with a run-once escape
  // hatch for recurring tasks); terminal tasks resubmit as a new one-off run
  // with the changes as overrides.
  editTask?: Task | null;
  onUpdated?: () => void;
};

// Statuses that already finished: editing one resubmits (POST /rerun with
// overrides) instead of rewriting history.
const EDIT_TERMINAL = new Set(["success", "error", "cancelled", "dead_lettered"]);

// taskToFormValues maps a task (or null for create mode) onto the form's
// initial state. Pure: the modal remounts per edit target (parent keys the
// component on the task id), so useState initializers read these once.
function taskToFormValues(task: Task | null) {
  const rec = task?.recurrence ?? "";
  const parsed = parseSimpleSchedule(rec);
  let scheduleMode: ScheduleMode = "now";
  let scheduledDate = "";
  let scheduledTime = "09:00";
  if (rec) {
    scheduleMode = "repeat";
  } else if (task?.scheduled_for) {
    const d = new Date(task.scheduled_for);
    if (!Number.isNaN(d.getTime())) {
      const pad = (n: number) => String(n).padStart(2, "0");
      scheduleMode = "once";
      scheduledDate = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
      scheduledTime = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    }
  }
  return {
    title: task?.title ?? "",
    prompt: task?.prompt ?? "",
    description: task?.description ?? "",
    tagsInput: (task?.tags ?? []).join(", "),
    persona: task?.persona ?? "",
    scheduleMode,
    scheduledDate,
    scheduledTime,
    recurrence: rec,
    repeatEditor: (rec && !parsed ? "cron" : "simple") as RepeatEditor,
    endMode: (task?.recurrence_until
      ? "date"
      : typeof task?.recurrence_remaining === "number"
        ? "count"
        : "never") as RepeatEndMode,
    endDate: task?.recurrence_until ? String(task.recurrence_until).slice(0, 10) : "",
    endCount:
      typeof task?.recurrence_remaining === "number" ? String(task.recurrence_remaining) : "",
    simpleFrequency: parsed?.frequency ?? ("weekdays" as SimpleFrequency),
    simpleTime: parsed?.time ?? "09:00",
    simpleWeekdays: parsed?.weekdays ?? ["1"],
    model: task?.model || DEFAULT_PRIMARY_MODEL,
    fallbackModel: task?.fallback_model || DEFAULT_FALLBACK_MODEL,
    maxIterations:
      typeof task?.max_iterations === "number" ? String(task.max_iterations) : "",
    captainsLog: Boolean(task?.instruction_self_improve),
    allowNetwork: Boolean(task?.allow_network),
    // Sub-agent delegation (#1043) defaults ON: a fresh form starts true, and an
    // edited/cloned task only starts false when it explicitly opted out (the
    // server always serializes the field, so undefined means "old payload" =
    // the default).
    allowDelegation: task ? task.allow_delegation !== false : true,
    carryContext: Boolean(task?.carry_context),
    mcpSelection: task?.mcp_selection ?? [],
    runIfCommand: task?.run_if?.command ?? "",
    runIfOnError: (task?.run_if?.on_error ?? "run") as "run" | "skip",
    runIfTimeout: task?.run_if?.timeout_seconds ?? 30,
    // The form has no exit-code field; carried so an edit echoes the stored
    // gate faithfully (a lossy echo reads as a run_if change server-side and
    // 403s non-admin edits of unrelated fields).
    runIfExitCode: task?.run_if?.exit_code_is ?? 0,
    expectedDuration:
      typeof task?.expected_duration_minutes === "number" &&
      task.expected_duration_minutes > 0
        ? String(task.expected_duration_minutes)
        : "",
    sandboxMemory:
      typeof task?.sandbox_limits?.memory_mb === "number" && task.sandbox_limits.memory_mb > 0
        ? String(task.sandbox_limits.memory_mb)
        : "",
    sandboxCpus:
      typeof task?.sandbox_limits?.cpus === "number" && task.sandbox_limits.cpus > 0
        ? String(task.sandbox_limits.cpus)
        : "",
    sandboxPids:
      typeof task?.sandbox_limits?.pids === "number" && task.sandbox_limits.pids > 0
        ? String(task.sandbox_limits.pids)
        : "",
    contextOpen: Boolean(task?.description || task?.tags?.length || task?.persona),
    toolsOpen: Boolean(task?.mcp_selection?.length),
  };
}

// Reveal — the one disclosure anatomy every collapsible group shares (chevron +
// label + muted contents hint + a right-aligned live slot). The body stays
// mounted and animates via the 0fr→1fr grid trick (140ms height + fade; the
// global reduced-motion block snaps it); `inert` keeps collapsed content out of
// the tab order.
function Reveal({
  id,
  label,
  hint,
  right,
  open,
  onToggle,
  children,
}: {
  id: string;
  label: string;
  hint: string;
  right?: React.ReactNode;
  open: boolean;
  onToggle: () => void;
  children: React.ReactNode;
}) {
  return (
    <div className={`task-reveal${open ? " is-open" : ""}`}>
      <button
        type="button"
        className="task-reveal-header"
        aria-expanded={open}
        aria-controls={id}
        onClick={onToggle}
      >
        <svg
          className="task-reveal-chevron"
          width="12"
          height="12"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2.2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <path d="M6 9l6 6 6-6" />
        </svg>
        <span className="task-reveal-label">{label}</span>
        <span className="task-reveal-hint">{hint}</span>
        <span className="task-reveal-spacer" />
        {right}
      </button>
      <div className="task-reveal-body" id={id} inert={open ? undefined : true}>
        <div className="task-reveal-inner">
          <div className="task-reveal-content">{children}</div>
        </div>
      </div>
    </div>
  );
}

function Spinner() {
  return (
    <svg
      className="task-spinner"
      width="13"
      height="13"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <path d="M21 12a9 9 0 1 1-9-9" />
    </svg>
  );
}

export function TaskCreateModal({
  open,
  servers,
  serversLoading,
  onClose,
  onCreated,
  editTask,
  onUpdated,
}: TaskCreateModalProps) {
  const { showToast } = useToast();
  const editing = !!editTask;
  const editTerminal = editing && EDIT_TERMINAL.has(editTask?.status ?? "");
  // Which scope a recurring edit applies to: asked via an in-modal chooser.
  const [scopeOpen, setScopeOpen] = useState(false);
  // Mount-time form values (blank for create, prefilled for edit). The parent
  // keys this component on the edit target, so a different task remounts with
  // fresh state — no prefill effects needed.
  const [init] = useState(() => taskToFormValues(editTask ?? null));

  const [title, setTitle] = useState(init.title);
  const [prompt, setPrompt] = useState(init.prompt);
  const [description, setDescription] = useState(init.description);
  const [tagsInput, setTagsInput] = useState(init.tagsInput);
  const [persona, setPersona] = useState(init.persona);

  const [emails, setEmails] = useState<string[]>([]);
  const [emailInput, setEmailInput] = useState("");
  const [emailError, setEmailError] = useState("");

  const [scheduleMode, setScheduleMode] = useState<ScheduleMode>(init.scheduleMode);
  const [scheduledDate, setScheduledDate] = useState(init.scheduledDate);
  const [scheduledTime, setScheduledTime] = useState(init.scheduledTime);
  const [recurrence, setRecurrence] = useState(init.recurrence);
  const [repeatEditor, setRepeatEditor] = useState<RepeatEditor>(init.repeatEditor);
  const [simpleFrequency, setSimpleFrequency] = useState<SimpleFrequency>(init.simpleFrequency);
  const [simpleTime, setSimpleTime] = useState(init.simpleTime);
  const [simpleWeekdays, setSimpleWeekdays] = useState<string[]>(init.simpleWeekdays);
  const [endMode, setEndMode] = useState<RepeatEndMode>(init.endMode);
  const [endDate, setEndDate] = useState(init.endDate);
  const [endCount, setEndCount] = useState(init.endCount);

  const [contextOpen, setContextOpen] = useState(init.contextOpen);
  const [toolsOpen, setToolsOpen] = useState(init.toolsOpen);
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const [model, setModel] = useState(init.model);
  const [fallbackModel, setFallbackModel] = useState(init.fallbackModel);
  const [maxIterations, setMaxIterations] = useState(init.maxIterations);
  const [captainsLog, setCaptainsLog] = useState(init.captainsLog);
  const [allowNetwork, setAllowNetwork] = useState(init.allowNetwork);
  const [allowDelegation, setAllowDelegation] = useState(init.allowDelegation);
  const [carryContext, setCarryContext] = useState(init.carryContext);
  // Pre-run shell gate (#269): empty = no gate (unconditional promotion).
  const [runIfCommand, setRunIfCommand] = useState(init.runIfCommand);
  const [runIfOnError, setRunIfOnError] = useState<"run" | "skip">(init.runIfOnError);
  const [runIfTimeout, setRunIfTimeout] = useState(init.runIfTimeout);
  // SLA expected duration (#274): blank = no SLA. Stored as a string so the
  // empty/typing states round-trip cleanly; parsed to int on submit.
  const [expectedDuration, setExpectedDuration] = useState(init.expectedDuration);
  // Per-task extended-thinking override (#220): "" = inherit the deployment
  // default, "0" = off, a positive value = this task's budget in tokens.
  const [thinkingBudget, setThinkingBudget] = useState("");
  const [sandboxMemory, setSandboxMemory] = useState(init.sandboxMemory);
  const [sandboxCpus, setSandboxCpus] = useState(init.sandboxCpus);
  const [sandboxPids, setSandboxPids] = useState(init.sandboxPids);

  // The per-task MCP selection (replaces the legacy target_node_name).
  const [mcpSelection, setMcpSelection] = useState<MCPChoice[]>(init.mcpSelection);
  const [fileCount, setFileCount] = useState(0);

  const [errors, setErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);

  // Cost-estimate state (#233): advisory, never gates Launch. The result lives
  // inline in the footer; `estimateKey` remembers the schedule+model snapshot it
  // was computed against so the figure dims once those change (stale until
  // re-run). estimateNote is the no-forecast inline slot (empty prompt/failure).
  const [estimating, setEstimating] = useState(false);
  const [forecast, setForecast] = useState<CostForecast | null>(null);
  const [estimateKey, setEstimateKey] = useState("");
  const [estimateNote, setEstimateNote] = useState("");

  // Dirty-close guard state (+ where to send focus back on "Keep editing").
  const [discardOpen, setDiscardOpen] = useState(false);
  const keepEditingRef = useRef<HTMLButtonElement | null>(null);
  const resumeFocusRef = useRef<HTMLElement | null>(null);

  // Scrolled chrome: header/footer cast a shadow only while body content passes
  // under them.
  const [scrolledTop, setScrolledTop] = useState(false);
  const [scrolledBottom, setScrolledBottom] = useState(false);

  const fileHandle = useRef<FileUploadHandle | null>(null);
  const modalRef = useRef<HTMLDivElement | null>(null);
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const promptRef = useRef<HTMLTextAreaElement | null>(null);
  // Prompt sizing. `expanded` is the tall-pane toggle (persisted); manualHeight
  // records that the operator dragged the grip, after which auto-grow must stop
  // — otherwise the next keystroke would snap their chosen height away. lastAuto
  // is how a drag is TOLD APART from our own writes: any height we did not set
  // came from the user.
  // Lazily seeded from storage, matching UpcomingPanel's view toggle. It is read
  // at MOUNT: the parent keys this modal per edit target, so each Edit picks the
  // preference up, while the long-lived create instance adopts a change made
  // elsewhere on the next page load.
  const [promptExpanded, setPromptExpanded] = useState(() => {
    if (typeof window === "undefined") return false;
    try {
      return window.localStorage.getItem(PROMPT_EXPANDED_STORAGE_KEY) === "1";
    } catch {
      return false;
    }
  });
  const promptManualHeight = useRef(false);
  const promptLastAutoHeight = useRef<string | null>(null);
  const emailInputRef = useRef<HTMLInputElement | null>(null);
  // Outside-click close: only when both mousedown AND click land on the overlay
  // itself (a drag from inside the form that ends on the overlay must not
  // close).
  const overlayMouseDownOnSelf = useRef(false);

  // Task templates (#262): the bundle's read-only catalog of pre-filled task
  // shapes. Fetched once when the modal opens; an empty catalog (or a fetch
  // failure) suppresses the section — the blank form is always available.
  const [templates, setTemplates] = useState<TaskTemplate[]>([]);
  // Template variable fill: the template the user picked that still has
  // unresolved {variables} (built-ins like {date} never land here — they
  // substitute silently). Non-null swaps the card grid for the inline fill
  // form; templateVarValues carries the per-variable input state.
  const [pendingTemplate, setPendingTemplate] = useState<TaskTemplate | null>(null);
  const [templateVarValues, setTemplateVarValues] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    orchestratorApi
      .taskTemplates()
      .then((list) => {
        if (!cancelled) setTemplates(Array.isArray(list) ? list : []);
      })
      .catch(() => {
        if (!cancelled) setTemplates([]);
      });
    return () => {
      cancelled = true;
    };
  }, [open]);

  const scheduledFor = scheduledDate ? `${scheduledDate}T${scheduledTime || "09:00"}` : "";

  const createDirty =
    title.trim() !== "" ||
    prompt.trim() !== "" ||
    description.trim() !== "" ||
    tagsInput.trim() !== "" ||
    persona.trim() !== "" ||
    emails.length > 0 ||
    emailInput.trim() !== "" ||
    scheduleMode !== "now" ||
    recurrence.trim() !== "" ||
    endMode !== "never" ||
    scheduledDate !== "" ||
    mcpSelection.length > 0 ||
    fileCount > 0 ||
    model !== DEFAULT_PRIMARY_MODEL ||
    fallbackModel !== DEFAULT_FALLBACK_MODEL ||
    maxIterations.trim() !== "" ||
    expectedDuration.trim() !== "" ||
    thinkingBudget.trim() !== "" ||
    sandboxMemory.trim() !== "" ||
    sandboxCpus.trim() !== "" ||
    sandboxPids.trim() !== "" ||
    captainsLog ||
    allowNetwork ||
    !allowDelegation ||
    carryContext ||
    runIfCommand.trim() !== "";

  // Edit mode compares the live form against the prefilled values (the
  // component remounts per edit target via the parent's key, so `init` is the
  // mount-time truth); create mode keeps its semantic any-field-set check.
  const formSnapshot = JSON.stringify([
    title,
    prompt,
    description,
    tagsInput,
    persona,
    scheduleMode,
    scheduledDate,
    scheduledTime,
    recurrence,
    endMode,
    endDate,
    endCount,
    model,
    fallbackModel,
    maxIterations,
    captainsLog,
    allowNetwork,
    allowDelegation,
    carryContext,
    runIfCommand,
    runIfOnError,
    runIfTimeout,
    expectedDuration,
    sandboxMemory,
    sandboxCpus,
    sandboxPids,
    mcpSelection,
  ]);
  const initSnapshot = JSON.stringify([
    init.title,
    init.prompt,
    init.description,
    init.tagsInput,
    init.persona,
    init.scheduleMode,
    init.scheduledDate,
    init.scheduledTime,
    init.recurrence,
    init.endMode,
    init.endDate,
    init.endCount,
    init.model,
    init.fallbackModel,
    init.maxIterations,
    init.captainsLog,
    init.allowNetwork,
    init.allowDelegation,
    init.carryContext,
    init.runIfCommand,
    init.runIfOnError,
    init.runIfTimeout,
    init.expectedDuration,
    init.sandboxMemory,
    init.sandboxCpus,
    init.sandboxPids,
    init.mcpSelection,
  ]);
  const dirty = editing ? formSnapshot !== initSnapshot : createDirty;

  const resetForm = useCallback(() => {
    setPrompt("");
    setDescription("");
    setTagsInput("");
    setPersona("");
    setEmails([]);
    setEmailInput("");
    setEmailError("");
    setScheduleMode("now");
    setScheduledDate("");
    setScheduledTime("09:00");
    setRecurrence("");
    setRepeatEditor("simple");
    setSimpleFrequency("weekdays");
    setSimpleTime("09:00");
    setSimpleWeekdays(["1"]);
    setContextOpen(false);
    setToolsOpen(false);
    setAdvancedOpen(false);
    setModel(DEFAULT_PRIMARY_MODEL);
    setFallbackModel(DEFAULT_FALLBACK_MODEL);
    setMaxIterations("");
    setCaptainsLog(false);
    setAllowNetwork(false);
    setCarryContext(false);
    setRunIfCommand("");
    setRunIfOnError("run");
    setRunIfTimeout(30);
    setExpectedDuration("");
    setThinkingBudget("");
    setSandboxMemory("");
    setSandboxCpus("");
    setSandboxPids("");
    setMcpSelection([]);
    setPendingTemplate(null);
    setTemplateVarValues({});
    setErrors({});
    setForecast(null);
    setEstimateKey("");
    setEstimateNote("");
    setDiscardOpen(false);
    fileHandle.current?.reset();
    setFileCount(0);
  }, []);

  // requestClose is the ONE close path (Esc via useDialogA11y, ✕, Cancel,
  // outside-click). Submitting defers; an open guard treats Esc as "Keep
  // editing"; a dirty form gets the guard; a clean form closes instantly.
  const requestClose = useCallback(() => {
    if (submitting) return;
    if (discardOpen) {
      setDiscardOpen(false);
      resumeFocusRef.current?.focus();
      return;
    }
    if (dirty) {
      resumeFocusRef.current =
        (document.activeElement as HTMLElement | null) ?? promptRef.current;
      setDiscardOpen(true);
      return;
    }
    onClose();
  }, [submitting, discardOpen, dirty, onClose]);

  useDialogA11y(open, modalRef, requestClose, { initialFocusRef: promptRef });

  // Focus the guard's safe action when it opens.
  useEffect(() => {
    if (discardOpen) keepEditingRef.current?.focus();
  }, [discardOpen]);

  const updateScrollShadows = useCallback(() => {
    const el = bodyRef.current;
    if (!el) return;
    setScrolledTop(el.scrollTop > 1);
    setScrolledBottom(el.scrollHeight - el.scrollTop - el.clientHeight > 1);
  }, []);

  useEffect(() => {
    if (!open) return;
    updateScrollShadows();
  }, [open, contextOpen, toolsOpen, advancedOpen, templates, updateScrollShadows]);

  // applyTemplate picks a template card. Built-in variables ({date},
  // fillFormFromTemplate seeds every form field from the template payload.
  // Built-in variables ({date}, {user_name}) substitute automatically; values
  // the user left blank keep their {token} placeholder in the prompt, visible
  // rather than silently dropped (applyTemplateVars contract). Every field
  // stays editable afterward — this only seeds the form. The task is still
  // created through the ordinary submit/createTask path.
  const fillFormFromTemplate = (tpl: TaskTemplate, userValues: Record<string, string>) => {
    const ctx = { userName: undefined as string | undefined };
    const t = tpl.task ?? {};
    setPrompt(t.prompt ? applyTemplateVars(t.prompt, userValues, ctx) : "");
    // Seed the title from the template's own name — that is what the operator
    // just picked the task by. Editable afterwards like every seeded field.
    setTitle(t.title ?? tpl.name ?? "");
    setDescription(t.description ?? "");
    setTagsInput((t.tags ?? []).join(", "));
    setPersona(t.persona ?? "");
    const templateRecurrence = t.recurrence ?? "";
    const parsedSchedule = parseSimpleSchedule(templateRecurrence);
    setRecurrence(templateRecurrence);
    setRepeatEditor(templateRecurrence && !parsedSchedule ? "cron" : "simple");
    if (parsedSchedule) {
      setSimpleFrequency(parsedSchedule.frequency);
      setSimpleTime(parsedSchedule.time);
      setSimpleWeekdays(parsedSchedule.weekdays);
    }
    setScheduleMode(t.recurrence ? "repeat" : "now");
    setScheduledDate("");
    setModel(t.model ?? DEFAULT_PRIMARY_MODEL);
    setFallbackModel(t.fallback_model ?? DEFAULT_FALLBACK_MODEL);
    setAllowNetwork(Boolean(t.allow_network));
    setAllowDelegation(t.allow_delegation !== false);
    setCarryContext(Boolean(t.carry_context));
    setCaptainsLog(Boolean(t.instruction_self_improve));
    setExpectedDuration(
      typeof t.expected_duration_minutes === "number" && t.expected_duration_minutes > 0
        ? String(t.expected_duration_minutes)
        : "",
    );
    setThinkingBudget(
      typeof t.thinking_budget_tokens === "number" ? String(t.thinking_budget_tokens) : "",
    );
    setSandboxMemory(
      typeof t.sandbox_limits?.memory_mb === "number" && t.sandbox_limits.memory_mb > 0
        ? String(t.sandbox_limits.memory_mb)
        : "",
    );
    setSandboxCpus(
      typeof t.sandbox_limits?.cpus === "number" && t.sandbox_limits.cpus > 0
        ? String(t.sandbox_limits.cpus)
        : "",
    );
    setSandboxPids(
      typeof t.sandbox_limits?.pids === "number" && t.sandbox_limits.pids > 0
        ? String(t.sandbox_limits.pids)
        : "",
    );
    setMaxIterations(typeof t.max_iterations === "number" ? String(t.max_iterations) : "");
    if (t.description || t.tags?.length || t.persona) setContextOpen(true);
    if (
      t.persona ||
      t.allow_network ||
      t.allow_delegation === false ||
      t.carry_context ||
      t.instruction_self_improve ||
      t.sandbox_limits
    ) {
      setAdvancedOpen(true);
    }
  };

  // applyTemplate picks a template card. A template whose {variables} are all
  // built-ins fills immediately; anything still unresolved swaps the card grid
  // for the inline fill form (no native dialogs).
  const applyTemplate = (tpl: TaskTemplate) => {
    if (promptableVars(tpl.variables ?? [], {}).length === 0) {
      fillFormFromTemplate(tpl, {});
      return;
    }
    setTemplateVarValues({});
    setPendingTemplate(tpl);
  };

  const confirmTemplateVars = () => {
    if (!pendingTemplate) return;
    fillFormFromTemplate(pendingTemplate, templateVarValues);
    setPendingTemplate(null);
  };

  // autoGrowPrompt sizes the field to its content, but yields to the operator:
  // once they expand it or drag it, their height stands until they reset it.
  const autoGrowPrompt = (el: HTMLTextAreaElement) => {
    if (promptExpanded) {
      // The .is-expanded rule owns the height; an inline value would beat it.
      el.style.height = "";
      promptLastAutoHeight.current = null;
      return;
    }
    if (promptManualHeight.current) return;
    el.style.height = "auto";
    const next = `${Math.min(el.scrollHeight, PROMPT_AUTOGROW_MAX_PX)}px`;
    el.style.height = next;
    promptLastAutoHeight.current = next;
  };

  // Size the field for content the operator did NOT type: the prefill when
  // editing an existing task, a template, a prompt-library insert. Without this
  // the edit form opened a 200-line protocol into a three-row box — the whole
  // reason this field needed to become adjustable.
  useEffect(() => {
    const el = promptRef.current;
    if (!el || !open) return;
    autoGrowPrompt(el);
    // autoGrowPrompt is recreated per render; the sizing inputs are what matter.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [prompt, promptExpanded, open]);

  // Tell a drag of the native grip apart from our own height writes. jsdom has
  // no ResizeObserver (unit tests) — there the drag simply is not detectable,
  // which costs those tests nothing.
  useEffect(() => {
    const el = promptRef.current;
    if (!el || !open || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => {
      const current = el.style.height;
      if (!current || current === promptLastAutoHeight.current) return;
      promptManualHeight.current = true;
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [open]);

  // togglePromptExpanded doubles as the reset for a dragged height: clearing the
  // inline value hands sizing back to the class rule (expanded) or to auto-grow
  // (collapsed), so an operator who dragged themselves into a bad size has one
  // obvious way out.
  const togglePromptExpanded = () => {
    const next = !promptExpanded;
    setPromptExpanded(next);
    promptManualHeight.current = false;
    promptLastAutoHeight.current = null;
    if (promptRef.current) promptRef.current.style.height = "";
    try {
      window.localStorage.setItem(PROMPT_EXPANDED_STORAGE_KEY, next ? "1" : "0");
    } catch {
      /* storage blocked — the toggle still works for this session */
    }
  };

  if (!open) return null;

  // ── Derived display state ─────────────────────────────────────────────────

  const cronDescription = describeCronExpression(recurrence);
  const cronNext = cronDescription ? nextCronOccurrence(recurrence) : null;

  // The {variables} the picked template still needs from the user (built-ins
  // never appear — they substitute silently at apply time).
  const templatePendingVars = pendingTemplate
    ? promptableVars(pendingTemplate.variables ?? [], {})
    : [];

  const contextCount = [description, tagsInput, persona].filter((v) => v.trim() !== "").length;

  const advancedCount = [
    model !== DEFAULT_PRIMARY_MODEL,
    fallbackModel !== DEFAULT_FALLBACK_MODEL,
    maxIterations.trim() !== "",
    expectedDuration.trim() !== "",
    thinkingBudget.trim() !== "",
    sandboxMemory.trim() !== "",
    sandboxCpus.trim() !== "",
    sandboxPids.trim() !== "",
    captainsLog,
    allowNetwork,
    // Delegation is ON by default (#1043): the opt-OUT is the non-default state
    // worth surfacing in the Advanced badge.
    !allowDelegation,
    carryContext,
    runIfCommand.trim() !== "",
  ].filter(Boolean).length;

  const enabledServers = mcpSelection.length;
  const toolsSummary = serversLoading
    ? "Loading servers…"
    : enabledServers === 0 && fileCount === 0
      ? "Sandbox only — no tools"
      : [
          enabledServers > 0 ? `${enabledServers} server${enabledServers === 1 ? "" : "s"}` : "",
          fileCount > 0 ? `${fileCount} file${fileCount === 1 ? "" : "s"}` : "",
        ]
          .filter(Boolean)
          .join(" · ");

  const errorCount = Object.keys(errors).length + (emailError ? 1 : 0);
  const promptEmpty = prompt.trim() === "";

  // The dirty-close guard names what would be lost.
  const dirtyParts: string[] = [];
  if (prompt.trim()) dirtyParts.push("the prompt");
  if (contextCount > 0) dirtyParts.push("the context notes");
  if (scheduleMode !== "now" || recurrence.trim() || scheduledDate) dirtyParts.push("the schedule");
  if (emails.length > 0)
    dirtyParts.push(`${emails.length} email recipient${emails.length === 1 ? "" : "s"}`);
  if (enabledServers > 0)
    dirtyParts.push(`${enabledServers} enabled server${enabledServers === 1 ? "" : "s"}`);
  if (fileCount > 0) dirtyParts.push(`${fileCount} attachment${fileCount === 1 ? "" : "s"}`);
  if (advancedCount > 0) dirtyParts.push("the advanced settings");
  const dirtyList =
    dirtyParts.length <= 1
      ? (dirtyParts[0] ?? "your changes")
      : `${dirtyParts.slice(0, -1).join(", ")}, and ${dirtyParts[dirtyParts.length - 1]}`;

  // ── Field helpers ─────────────────────────────────────────────────────────

  const setFieldError = (field: string, message: string) => {
    setErrors((prev) => {
      const next = { ...prev };
      if (message) next[field] = message;
      else delete next[field];
      return next;
    });
  };

  const blurValidateCron = () => {
    if (scheduleMode !== "repeat") return;
    if (!recurrence.trim()) {
      setFieldError("recurrence", "");
      return;
    }
    const v = validateCronExpression(recurrence);
    setFieldError("recurrence", v.valid ? "" : v.message);
  };

  const blurValidateScheduled = () => {
    if (scheduleMode !== "once") return;
    setFieldError("scheduled_for", scheduledDate ? "" : "Pick a date and time.");
  };

  const commitEmail = (): boolean => {
    const raw = emailInput.trim().toLowerCase();
    if (!raw) {
      setEmailError("");
      return true;
    }
    if (!isValidEmail(raw)) {
      setEmailError(describeEmailError(raw));
      return false;
    }
    if (!emails.includes(raw)) setEmails((prev) => [...prev, raw]);
    setEmailInput("");
    setEmailError("");
    return true;
  };

  const buildFinalPrompt = (recipients: string[]): string => {
    const base = prompt.trim();
    if (recipients.length === 0) return base;
    const yaml = recipients.map((e) => `    - ${e}`).join("\n");
    return `${base}\n\n---\nCRITICAL ACTION\nemail:\n  action: send_report\n  tool: email\n  instruction: "The following action is MANDATORY after completing the core task."\n  description: "Send the full report and findings to the listed recipients."\n  recipients:\n${yaml}\n---`;
  };

  // buildTaskData assembles the TaskCreate body shared by submit and the cost
  // estimate. Only the active schedule mode's field is sent — Run now sends
  // neither. Files are handled separately by submit (the estimate does not need
  // them).
  const buildTaskData = (finalPrompt: string): TaskCreate => {
    const taskData: TaskCreate = { prompt: finalPrompt };
    if (title.trim()) taskData.title = title.trim();
    if (description.trim()) taskData.description = description;
    const tags = tagsInput
      .split(",")
      .map((t) => t.trim().toLowerCase())
      .filter(Boolean);
    if (tags.length > 0) taskData.tags = tags;
    if (persona.trim()) taskData.persona = persona.trim();
    if (model) taskData.model = model;
    if (fallbackModel) taskData.fallback_model = fallbackModel;
    if (maxIterations.trim()) taskData.max_iterations = Number.parseInt(maxIterations, 10);
    if (captainsLog) taskData.instruction_self_improve = true;
    if (allowNetwork) taskData.allow_network = true;
    // Delegation defaults ON server-side (#1043): omit when on, send the
    // explicit false only for an opt-out — the tri-state server field treats a
    // missing key as the default.
    if (!allowDelegation) taskData.allow_delegation = false;
    if (carryContext) taskData.carry_context = true;
    if (mcpSelection.length > 0) taskData.mcp_selection = mcpSelection;
    if (scheduleMode === "once" && scheduledFor) {
      taskData.scheduled_for = new Date(scheduledFor).toISOString();
    }
    if (scheduleMode === "repeat" && recurrence.trim()) {
      taskData.recurrence = recurrence.trim();
      // End-of-day local time so "ends on July 31" includes July 31's run.
      if (endMode === "date" && endDate) {
        taskData.recurrence_until = new Date(`${endDate}T23:59:59`).toISOString();
      }
      if (endMode === "count" && endCount.trim()) {
        const n = Number.parseInt(endCount, 10);
        if (Number.isFinite(n) && n >= 1) taskData.recurrence_remaining = n;
      }
    }
    // Pre-run shell gate (#269): only attach when a command is set so the
    // default (no gate) round-trips as run_if: null.
    if (runIfCommand.trim()) {
      taskData.run_if = {
        command: runIfCommand.trim(),
        on_error: runIfOnError,
        timeout_seconds: runIfTimeout,
      };
      if (init.runIfExitCode !== 0) {
        taskData.run_if.exit_code_is = init.runIfExitCode;
      }
    }
    if (expectedDuration.trim()) {
      const mins = Number.parseInt(expectedDuration, 10);
    if (Number.isFinite(mins) && mins > 0) taskData.expected_duration_minutes = mins;
    }
    if (thinkingBudget.trim()) {
      const budget = Number.parseInt(thinkingBudget, 10);
      if (Number.isFinite(budget) && budget >= 0) taskData.thinking_budget_tokens = budget;
    }
    const memoryMb = Number.parseInt(sandboxMemory, 10);
    const cpus = Number.parseFloat(sandboxCpus);
    const pids = Number.parseInt(sandboxPids, 10);
    const limits: { memory_mb?: number; cpus?: number; pids?: number } = {};
    if (sandboxMemory.trim() && Number.isFinite(memoryMb) && memoryMb > 0) limits.memory_mb = memoryMb;
    if (sandboxCpus.trim() && Number.isFinite(cpus) && cpus > 0) limits.cpus = cpus;
    if (sandboxPids.trim() && Number.isFinite(pids) && pids > 0) limits.pids = pids;
    if (limits.memory_mb || limits.cpus || limits.pids) taskData.sandbox_limits = limits;
    return taskData;
  };

  const validateAll = (): Record<string, string> => {
    // Validate the TYPED prompt (the email block appended later must not mask
    // an empty prompt).
    const validation = validateTaskForm({
      prompt,
      model,
      fallback_model: fallbackModel,
      max_iterations: maxIterations,
      recurrence: scheduleMode === "repeat" ? recurrence : "",
      scheduled_for: scheduleMode === "once" ? scheduledFor : "",
    });
    const next: Record<string, string> = { ...(validation.errors as Record<string, string>) };
    if (title.trim().length > TITLE_MAX_LENGTH)
      next.title = `Title cannot exceed ${TITLE_MAX_LENGTH} characters.`;
    if (scheduleMode === "once" && !scheduledDate) next.scheduled_for = "Pick a date and time.";
    if (scheduleMode === "repeat" && !recurrence.trim())
      next.recurrence = "Type a cron expression or pick a preset below.";
    if (scheduleMode === "repeat" && endMode === "date" && !endDate)
      next.recurrence_end = "Pick the date the repeat ends.";
    if (scheduleMode === "repeat" && endMode === "count") {
      const n = Number.parseInt(endCount, 10);
      if (!Number.isFinite(n) || n < 1) next.recurrence_end = "Enter how many runs (at least 1).";
    }
    return next;
  };

  const ERROR_FOCUS_TARGETS: Record<string, string> = {
    title: "titleInput",
    prompt: "promptTextarea",
    scheduled_for: "scheduledForDate",
    recurrence: "recurrenceInput",
    model: "taskModelInput",
    fallback_model: "taskFallbackModelInput",
    max_iterations: "taskMaxIterations",
    run_if: "runIfCommandInput",
  };

  const focusFirstError = (errs: Record<string, string>) => {
    for (const field of Object.keys(ERROR_FOCUS_TARGETS)) {
      if (errs[field]) {
        if (field === "model" || field === "fallback_model" || field === "max_iterations") {
          setAdvancedOpen(true);
        }
        // Defer so a just-opened reveal has rendered its content.
        requestAnimationFrame(() => document.getElementById(ERROR_FOCUS_TARGETS[field])?.focus());
        return;
      }
    }
  };

  // The schedule+model snapshot an estimate belongs to (the design dims a
  // result computed against different values until re-run).
  const currentEstimateKey = JSON.stringify([
    scheduleMode,
    scheduledFor,
    recurrence,
    model,
    fallbackModel,
    maxIterations,
  ]);
  const estimateStale = forecast != null && estimateKey !== currentEstimateKey;

  // estimate fetches a pre-submission cost forecast (#233) for the current form
  // values and shows it inline in the footer. Advisory only — never blocks
  // Launch. Failures render inline (no toast).
  const estimate = async () => {
    if (promptEmpty) {
      setForecast(null);
      setEstimateNote("Add a prompt first.");
      return;
    }
    const taskData = buildTaskData(buildFinalPrompt(emails));
    setEstimating(true);
    setForecast(null);
    setEstimateNote("");
    try {
      const fc = await orchestratorApi.estimateTask(taskData);
      setForecast(fc);
      setEstimateKey(currentEstimateKey);
    } catch (err) {
      // Surface the server's reason inline: a validation rejection ("prompt
      // cannot exceed 100000 characters", "persona X is not in the loaded
      // client bundle", …) is actionable, and hiding it behind a generic
      // "try again" sent operators bug-hunting for what was a clear 400.
      const detail = (err as Error).message;
      setEstimateNote(detail ? `Estimate failed: ${detail}` : "Estimate failed — try again.");
      setForecast(null);
      console.warn("estimate failed:", detail);
    } finally {
      setEstimating(false);
    }
  };

  // submit handles all three save paths. scope applies only to a recurring
  // in-place edit: undefined asks (opens the chooser), "definition" PUTs the
  // task (every future run), "once" resubmits a one-off with the changes and
  // leaves the schedule untouched. Terminal tasks always resubmit.
  const submit = async (scope?: "definition" | "once") => {
    if (submitting) return;
    // Commit a half-typed email first so it isn't silently dropped — but keep
    // validating the rest either way, so one Launch surfaces EVERY problem and
    // focus lands on the first of them.
    const emailOk = commitEmail();
    const errs = validateAll();
    setErrors(errs);
    if (Object.keys(errs).length > 0 || !emailOk) {
      if (Object.keys(errs).length > 0) focusFirstError(errs);
      else emailInputRef.current?.focus();
      return;
    }

    // A recurring task edited in place needs a scope decision first.
    if (editing && !editTerminal && editTask?.recurrence && !scope) {
      setScopeOpen(true);
      return;
    }

    const taskData = buildTaskData(buildFinalPrompt(emails));
    setSubmitting(true);
    try {
      if (fileHandle.current?.hasFiles()) {
        const filenames = await fileHandle.current.uploadAll((file) =>
          orchestratorApi.uploadFile(file),
        );
        taskData.files = filenames;
      }
      if (editing && editTask) {
        if (editTerminal || scope === "once") {
          // POST /rerun with overrides: a fresh one-off copy, source untouched.
          // Only fields the rerun endpoint accepts as overrides are sent.
          const created = await orchestratorApi.rerunTask(editTask.id, {
            prompt: taskData.prompt,
            title: taskData.title ?? "",
            description: taskData.description ?? "",
            model: taskData.model,
            fallback_model: taskData.fallback_model,
            ...(taskData.max_iterations != null
              ? { max_iterations: taskData.max_iterations }
              : {}),
            allow_network: Boolean(taskData.allow_network),
            allow_delegation: allowDelegation,
            tags: taskData.tags ?? [],
            persona: taskData.persona ?? "",
            ...(taskData.thinking_budget_tokens != null
              ? { thinking_budget: taskData.thinking_budget_tokens }
              : {}),
          });
          showToast(
            `Resubmitted as task ${created.id.slice(0, 8)}… — running now`,
            "success",
          );
        } else {
          // PUT rewrites the definition. mcp_selection/tags must ALWAYS be
          // present on an edit: the server only applies them when non-nil, so
          // an empty array is how "cleared" round-trips (omitting would
          // silently keep the old value).
          taskData.mcp_selection = mcpSelection;
          taskData.tags = taskData.tags ?? [];
          await orchestratorApi.updateTask(editTask.id, taskData);
          showToast(
            editTask.recurrence
              ? "Task updated — applies to all future runs."
              : "Task updated.",
            "success",
          );
        }
        onUpdated?.();
      } else {
        await orchestratorApi.createTask(taskData);
        showToast("Task created successfully!", "success");
        onCreated();
      }
      resetForm();
      onClose();
    } catch (err) {
      const msg = (err as Error).message;
      // The one server-side validation surface the form can't pre-check fully.
      if (/run_if/i.test(msg)) {
        setFieldError("run_if", msg);
        setAdvancedOpen(true);
      }
      showToast(`Error: ${msg}`, "error");
    } finally {
      setSubmitting(false);
    }
  };

  // Segmented-control keyboard support (roving radio group).
  const onSegmentKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>) => {
    const idx = SCHEDULE_MODES.findIndex((m) => m.id === scheduleMode);
    let next = -1;
    if (e.key === "ArrowRight" || e.key === "ArrowDown") next = (idx + 1) % SCHEDULE_MODES.length;
    if (e.key === "ArrowLeft" || e.key === "ArrowUp")
      next = (idx - 1 + SCHEDULE_MODES.length) % SCHEDULE_MODES.length;
    if (e.key === "Home") next = 0;
    if (e.key === "End") next = SCHEDULE_MODES.length - 1;
    if (next < 0) return;
    e.preventDefault();
    setScheduleMode(SCHEDULE_MODES[next].id);
    (e.currentTarget.parentElement?.children[next] as HTMLElement | undefined)?.focus();
  };

  const changeScheduleMode = (mode: ScheduleMode) => {
    setScheduleMode(mode);
    if (mode === "repeat" && !recurrence.trim()) {
      setRecurrence(simpleScheduleCron(simpleFrequency, simpleTime, simpleWeekdays));
    }
    setErrors((prev) => {
      const next = { ...prev };
      delete next.recurrence;
      delete next.scheduled_for;
      return next;
    });
  };

  const showSimpleRepeatEditor = () => {
    const parsed = parseSimpleSchedule(recurrence);
    if (!parsed) return;
    setSimpleFrequency(parsed.frequency);
    setSimpleTime(parsed.time);
    setSimpleWeekdays(parsed.weekdays);
    setRepeatEditor("simple");
  };

  const footerStatus = submitting
    ? null
    : errorCount > 0
      ? {
          className: "task-footer-status task-footer-status--error",
          text: `Fix ${errorCount} field${errorCount === 1 ? "" : "s"} above`,
        }
      : promptEmpty
        ? { className: "task-footer-status", text: "Add a prompt to launch" }
        : null;

  return (
    <div
      className="modal-overlay is-open"
      onMouseDown={(e) => {
        overlayMouseDownOnSelf.current = e.target === e.currentTarget;
      }}
      onClick={(e) => {
        if (overlayMouseDownOnSelf.current && e.target === e.currentTarget) requestClose();
      }}
    >
      <div
        className="modal modal-lg task-modal"
        role="dialog"
        aria-modal="true"
        aria-label={editing ? "Edit Task" : "Create New Task"}
        ref={modalRef}
        tabIndex={-1}
        onDragOver={(e) => {
          // The whole dialog is the drop target for attachments.
          if (e.dataTransfer?.types?.includes("Files")) e.preventDefault();
        }}
        onDrop={(e) => {
          if (e.dataTransfer?.files?.length) {
            e.preventDefault();
            setToolsOpen(true);
            fileHandle.current?.addFiles(e.dataTransfer.files);
          }
        }}
      >
        <div
          className={`modal-header${scrolledTop ? " is-scrolled" : ""}${submitting ? " is-dimmed" : ""}`}
          inert={discardOpen || scopeOpen ? true : undefined}
        >
          <div className="modal-header-text">
            <h3>{editing ? "Edit Task" : "Create New Task"}</h3>
            <p className="modal-subtitle">
              {editing
                ? editTerminal
                  ? "This task already ran — saving resubmits it as a new one-off run with your changes."
                  : editTask?.recurrence
                    ? "Editing the schedule definition — you'll choose the scope when you save."
                    : "Editing a task that hasn't started yet."
                : "Define what runs, when it runs, and what it may touch."}
            </p>
          </div>
          <CloseButton label="Close modal" onClick={requestClose} />
        </div>

        <div
          className={`modal-body${submitting ? " is-dimmed" : ""}`}
          ref={bodyRef}
          onScroll={updateScrollShadows}
          inert={discardOpen || scopeOpen ? true : undefined}
        >
          <form
            id="createTaskForm"
            noValidate
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            <fieldset className="task-form" disabled={submitting}>
              {/* Task templates (#262) — pre-filled starting points. Suppressed
                  entirely when the bundle ships none. A card whose prompt still
                  has unresolved {variables} swaps the grid for an inline fill
                  form — no native prompt() dialogs. */}
              {!editing && pendingTemplate ? (
                <section className="task-group" data-testid="template-var-fill">
                  <div className="task-group-label">
                    Fill in: {pendingTemplate.name}
                  </div>
                  <div className="field-grid">
                    {templatePendingVars.map((name, i) => (
                      <div className="form-group" key={name}>
                        <label className="task-field-label" htmlFor={`templateVar-${name}`}>
                          {humanizeVarName(name)}
                        </label>
                        <input
                          id={`templateVar-${name}`}
                          name={`templateVar-${name}`}
                          type="text"
                          placeholder={`{${name}}`}
                          autoFocus={i === 0}
                          data-testid={`template-var-input-${name}`}
                          value={templateVarValues[name] ?? ""}
                          onChange={(e) =>
                            setTemplateVarValues((v) => ({ ...v, [name]: e.target.value }))
                          }
                          onKeyDown={(e) => {
                            if (e.key === "Enter") {
                              e.preventDefault();
                              confirmTemplateVars();
                            }
                          }}
                        />
                      </div>
                    ))}
                  </div>
                  <div className="template-var-actions">
                    <button
                      type="button"
                      className="btn btn-secondary"
                      data-testid="template-var-back"
                      onClick={() => setPendingTemplate(null)}
                    >
                      Back
                    </button>
                    <button
                      type="button"
                      className="btn btn-primary"
                      data-testid="template-var-apply"
                      onClick={confirmTemplateVars}
                    >
                      Apply template
                    </button>
                  </div>
                </section>
              ) : !editing && templates.length > 0 ? (
                <section className="task-group" data-testid="task-template-section">
                  <div className="task-group-label">Start from a template</div>
                  <div className="template-card-grid" role="group" aria-label="Task templates">
                    {templates.map((tpl) => (
                      <button
                        key={tpl.name}
                        type="button"
                        className="template-card"
                        data-testid="template-card"
                        onClick={() => applyTemplate(tpl)}
                      >
                        {tpl.icon ? (
                          <span className="template-card-icon" aria-hidden="true">
                            {tpl.icon}
                          </span>
                        ) : null}
                        <span className="template-card-text">
                          <span className="template-card-name">{tpl.name}</span>
                          {tpl.description ? (
                            <span className="template-card-desc">{tpl.description}</span>
                          ) : null}
                        </span>
                      </button>
                    ))}
                  </div>
                </section>
              ) : null}

              {/* ── Title ──────────────────────────────────────────────
                  Sits ABOVE the prompt on purpose: it is the field that
                  replaces writing a title line at the top of the prompt, so
                  it has to be the thing you meet first. Optional — an
                  untitled task still lists by its prompt's first line. */}
              <section className="task-group">
                <div className="task-label-row">
                  <label className="task-label" htmlFor="titleInput">
                    Title
                  </label>
                  <span className="task-optional-badge">Optional</span>
                </div>
                <input
                  id="titleInput"
                  name="title"
                  type="text"
                  className={`task-title-input${errors.title ? " has-error" : ""}`}
                  maxLength={TITLE_MAX_LENGTH}
                  placeholder="Daily pacing summary"
                  aria-describedby={errors.title ? "title-error title-help" : "title-help"}
                  value={title}
                  onChange={(e) => {
                    setTitle(e.target.value);
                    if (errors.title) setFieldError("title", "");
                  }}
                />
                {errors.title ? (
                  <div className="validation-error" id="title-error" data-testid="error-title">
                    {errors.title}
                  </div>
                ) : null}
                <p className="field-hint" id="title-help">
                  How this job is listed in the Operations Center. Every run of it — including
                  future scheduled ones — carries the same title. Left empty, the list falls back
                  to the first line of the prompt.
                </p>
              </section>

              {/* ── Prompt ─────────────────────────────────────────────── */}
              <section className="task-group">
                <div className="task-label-row">
                  <label className="task-label" htmlFor="promptTextarea">
                    Prompt
                  </label>
                  <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <PromptLibrary
                      currentText={prompt}
                      // Seed the title from the library entry's name — for a
                      // bundle prompt that name IS the `name:` header operators
                      // were reading off the top of the prompt to identify the
                      // job. Only when the title is still empty: an operator who
                      // already titled the task keeps their wording.
                      onInsert={(content, name) => {
                        setPrompt(content);
                        if (name && !title.trim()) setTitle(name.slice(0, TITLE_MAX_LENGTH));
                      }}
                    />
                    <button
                      type="button"
                      className="task-prompt-resize-btn"
                      data-testid="prompt-expand-toggle"
                      aria-pressed={promptExpanded}
                      aria-controls="promptTextarea"
                      aria-label={promptExpanded ? "Collapse prompt editor" : "Expand prompt editor"}
                      data-tip-top={promptExpanded ? "Collapse the editor" : "Expand the editor"}
                      onClick={togglePromptExpanded}
                    >
                      {promptExpanded ? (
                        <Icon name="compress" className="size-4" />
                      ) : (
                        <Icon name="expand" className="size-4" />
                      )}
                    </button>
                    <span className="task-required-badge">Required</span>
                  </span>
                </div>
                <textarea
                  id="promptTextarea"
                  name="prompt"
                  ref={promptRef}
                  className={`task-prompt-input${errors.prompt ? " has-error" : ""}${
                    promptExpanded ? " is-expanded" : ""
                  }`}
                  required
                  rows={3}
                  maxLength={100000}
                  placeholder="Pull yesterday's sales report, compare it to the plan, and email a pacing summary…"
                  aria-describedby={errors.prompt ? "prompt-error prompt-help" : "prompt-help"}
                  value={prompt}
                  onChange={(e) => {
                    setPrompt(e.target.value);
                    autoGrowPrompt(e.target);
                    if (errors.prompt && e.target.value.trim().length >= 3) {
                      setFieldError("prompt", "");
                    }
                  }}
                  onBlur={() => {
                    if (prompt.trim() && prompt.trim().length < 3) {
                      setFieldError("prompt", "Prompt must be at least 3 characters");
                    }
                  }}
                />
                {errors.prompt ? (
                  <div className="validation-error" id="prompt-error" data-testid="error-prompt">
                    {errors.prompt}
                  </div>
                ) : null}
                <p className="field-hint" id="prompt-help">
                  What should this task do each run? Be concrete — sources, checks, and the output
                  you expect.
                </p>
              </section>

              {/* ── Context reveal (documentation · tags · persona, #281) ── */}
              <Reveal
                id="task-context-body"
                label="Context"
                hint="documentation · tags · persona"
                open={contextOpen}
                onToggle={() => setContextOpen((o) => !o)}
                right={
                  contextCount > 0 ? (
                    <span className="task-count-chip">{contextCount} set</span>
                  ) : undefined
                }
              >
                <div className="form-group">
                  <label className="task-field-label" htmlFor="descriptionTextarea">
                    Documentation
                  </label>
                  <textarea
                    id="descriptionTextarea"
                    name="description"
                    className="task-doc-input"
                    rows={3}
                    maxLength={10000}
                    placeholder="Notes for operators… (Markdown)"
                    aria-describedby="description-help"
                    value={description}
                    onChange={(e) => setDescription(e.target.value)}
                  />
                  <p className="field-hint" id="description-help">
                    Why this task exists, what it costs, side effects, the runbook if it fails, who
                    owns it. Shown to operators — never injected into the agent&apos;s prompt.
                  </p>
                </div>
                <div className="field-grid">
                  <div className="form-group">
                    <label className="task-field-label" htmlFor="tagsInput">
                      Tags
                    </label>
                    <input
                      id="tagsInput"
                      name="tags"
                      type="text"
                      placeholder="nightly, prod, data-pipeline"
                      aria-describedby="tags-help"
                      value={tagsInput}
                      onChange={(e) => setTagsInput(e.target.value)}
                    />
                    <p className="field-hint" id="tags-help">
                      Comma-separated; used for filtering.
                    </p>
                  </div>
                  <div className="form-group">
                    <label className="task-field-label" htmlFor="personaInput">
                      Persona
                    </label>
                    <input
                      id="personaInput"
                      name="persona"
                      type="text"
                      placeholder="security-auditor"
                      aria-describedby="persona-help"
                      value={persona}
                      onChange={(e) => setPersona(e.target.value)}
                    />
                    <p className="field-hint" id="persona-help">
                      Persona name from your workspace; blank = default.
                    </p>
                  </div>
                </div>
              </Reveal>

              {/* ── Schedule ───────────────────────────────────────────── */}
              <section className="task-group">
                <div className="task-label-row">
                  <span className="task-label" id="schedule-label">
                    Schedule
                  </span>
                </div>
                <div className="task-segment" role="radiogroup" aria-labelledby="schedule-label">
                  {SCHEDULE_MODES.map((m) => (
                    <button
                      key={m.id}
                      type="button"
                      role="radio"
                      aria-checked={scheduleMode === m.id}
                      tabIndex={scheduleMode === m.id ? 0 : -1}
                      className={`task-segment-btn${scheduleMode === m.id ? " is-active" : ""}`}
                      onClick={() => changeScheduleMode(m.id)}
                      onKeyDown={onSegmentKeyDown}
                    >
                      {m.label}
                    </button>
                  ))}
                </div>

                {scheduleMode === "now" ? (
                  <p className="task-schedule-caption">Runs immediately after launch.</p>
                ) : null}

                {scheduleMode === "once" ? (
                  <div className="task-schedule-once">
                    <div className="schedule-datetime-group">
                      <label className="task-schedule-field" htmlFor="scheduledForDate">
                        <span>Date</span>
                        <input
                          id="scheduledForDate"
                          type="date"
                          aria-label="Schedule date"
                          className={errors.scheduled_for ? "has-error" : ""}
                          value={scheduledDate}
                          onChange={(e) => {
                            setScheduledDate(e.target.value);
                            if (e.target.value) setFieldError("scheduled_for", "");
                          }}
                          onBlur={blurValidateScheduled}
                        />
                      </label>
                      <label className="task-schedule-field" htmlFor="scheduledForTime">
                        <span>Time</span>
                        <input
                          id="scheduledForTime"
                          type="time"
                          aria-label="Schedule time"
                          value={scheduledTime}
                          onChange={(e) => setScheduledTime(e.target.value)}
                          onBlur={blurValidateScheduled}
                        />
                      </label>
                    </div>
                    {errors.scheduled_for ? (
                      <div className="task-inline-error" data-testid="error-scheduled">
                        <svg
                          width="13"
                          height="13"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="2"
                          strokeLinecap="round"
                          aria-hidden="true"
                        >
                          <circle cx="12" cy="12" r="9" />
                          <path d="M12 8v4M12 15.5v.5" />
                        </svg>
                        {errors.scheduled_for}
                      </div>
                    ) : (
                      <p className="task-schedule-caption">
                        Runs once at the chosen time, in your local timezone.
                      </p>
                    )}
                  </div>
                ) : null}

                {scheduleMode === "repeat" ? (
                  <div className="task-schedule-repeat">
                    <div className="repeat-editor-tabs" role="tablist" aria-label="Repeat input method">
                      <button
                        type="button"
                        role="tab"
                        aria-selected={repeatEditor === "simple"}
                        className={repeatEditor === "simple" ? "is-active" : ""}
                        disabled={repeatEditor === "cron" && parseSimpleSchedule(recurrence) === null}
                        title={
                          repeatEditor === "cron" && parseSimpleSchedule(recurrence) === null
                            ? "This cron schedule is too complex for the simple editor"
                            : undefined
                        }
                        onClick={showSimpleRepeatEditor}
                      >
                        Simple schedule
                      </button>
                      <button type="button" role="tab" aria-selected={repeatEditor === "cron"} className={repeatEditor === "cron" ? "is-active" : ""} onClick={() => setRepeatEditor("cron")}>
                        Advanced cron
                      </button>
                    </div>
                    {repeatEditor === "simple" ? (
                      <div className="simple-schedule-grid" role="tabpanel">
                        <label className="task-schedule-field">
                          <span>Repeat</span>
                          <select
                            aria-label="Repeat frequency"
                            value={simpleFrequency}
                            onChange={(e) => {
                              const frequency = e.target.value as SimpleFrequency;
                              setSimpleFrequency(frequency);
                              setRecurrence(simpleScheduleCron(frequency, simpleTime, simpleWeekdays));
                              setFieldError("recurrence", "");
                            }}
                          >
                            <option value="daily">Every day</option>
                            <option value="weekdays">Every weekday</option>
                            <option value="weekly">Every week</option>
                          </select>
                        </label>
                        {simpleFrequency === "weekly" ? (
                          <fieldset className="weekly-days">
                            <legend>On</legend>
                            <div role="group" aria-label="Repeat weekdays">
                              {WEEKDAYS.map((day) => {
                                const selected = simpleWeekdays.includes(day.value);
                                return (
                                  <button
                                    key={day.value}
                                    type="button"
                                    aria-label={`Run on ${day.label}`}
                                    aria-pressed={selected}
                                    disabled={selected && simpleWeekdays.length === 1}
                                    className={selected ? "is-selected" : ""}
                                    onClick={() => {
                                      const weekdays = selected
                                        ? simpleWeekdays.filter((value) => value !== day.value)
                                        : WEEKDAYS.map((item) => item.value).filter(
                                            (value) => value === day.value || simpleWeekdays.includes(value),
                                          );
                                      setSimpleWeekdays(weekdays);
                                      setRecurrence(simpleScheduleCron(simpleFrequency, simpleTime, weekdays));
                                      setFieldError("recurrence", "");
                                    }}
                                  >
                                    {day.label.slice(0, 3)}
                                  </button>
                                );
                              })}
                            </div>
                          </fieldset>
                        ) : null}
                        <label className="task-schedule-field">
                          <span>At</span>
                          <input
                            type="time"
                            aria-label="Repeat time"
                            value={simpleTime}
                            onChange={(e) => {
                              setSimpleTime(e.target.value);
                              setRecurrence(
                                simpleScheduleCron(simpleFrequency, e.target.value, simpleWeekdays),
                              );
                              setFieldError("recurrence", "");
                            }}
                          />
                        </label>
                      </div>
                    ) : (
                      <label className="task-schedule-field task-cron-field" role="tabpanel" htmlFor="recurrenceInput">
                        <span>Cron expression</span>
                        <input
                          id="recurrenceInput"
                          type="text"
                          name="recurrence"
                          className={`task-cron-input${errors.recurrence ? " has-error" : ""}`}
                          maxLength={100}
                          placeholder="0 9 * * 1-5"
                          aria-label="Cron expression"
                          aria-describedby={errors.recurrence ? "recurrence-error" : "recurrence-echo"}
                          value={recurrence}
                          onChange={(e) => {
                            setRecurrence(e.target.value);
                            if (errors.recurrence) {
                              const v = validateCronExpression(e.target.value);
                              if (v.valid && e.target.value.trim()) setFieldError("recurrence", "");
                            }
                          }}
                          onBlur={blurValidateCron}
                        />
                      </label>
                    )}
                    {errors.recurrence ? (
                      <div
                        className="task-inline-error"
                        id="recurrence-error"
                        data-testid="error-recurrence"
                      >
                        <svg
                          width="13"
                          height="13"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="2"
                          strokeLinecap="round"
                          aria-hidden="true"
                        >
                          <circle cx="12" cy="12" r="9" />
                          <path d="M12 8v4M12 15.5v.5" />
                        </svg>
                        {errors.recurrence}
                      </div>
                    ) : null}
                    {repeatEditor === "cron" ? (
                      <div className="schedule-presets" role="group" aria-label="Schedule presets">
                        {SCHEDULE_PRESETS.map((p) => (
                          <button
                            key={p.cron}
                            type="button"
                            className={`preset-btn${recurrence === p.cron ? " active" : ""}`}
                            aria-pressed={recurrence === p.cron}
                            data-cron={p.cron}
                            onClick={() => {
                              setRecurrence(recurrence === p.cron ? "" : p.cron);
                              setFieldError("recurrence", "");
                            }}
                          >
                            <span className="preset-label">{p.label}</span>
                            <span className="preset-cron">{p.cron}</span>
                          </button>
                        ))}
                      </div>
                    ) : null}
                    {!errors.recurrence && cronDescription ? (
                      <div className="task-cron-echo" id="recurrence-echo" aria-live="polite">
                        <svg
                          width="13"
                          height="13"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="2.4"
                          strokeLinecap="round"
                          strokeLinejoin="round"
                          aria-hidden="true"
                        >
                          <path d="M5 12l5 5L20 6" />
                        </svg>
                        <span>
                          <strong>{cronNext ? `Next run ${formatNextRun(cronNext)}` : "Schedule ready"}</strong>
                          <span>{cronDescription} · local time</span>
                        </span>
                      </div>
                    ) : null}
                    {!errors.recurrence && !cronDescription ? (
                      <p className="field-hint" id="recurrence-echo">
                        Choose a schedule above or enter an advanced cron expression.
                      </p>
                    ) : null}
                    <div className="simple-schedule-grid" data-testid="repeat-end">
                      <label className="task-schedule-field">
                        <span>End repeat</span>
                        <select
                          aria-label="End repeat"
                          value={endMode}
                          onChange={(e) => {
                            setEndMode(e.target.value as RepeatEndMode);
                            setFieldError("recurrence_end", "");
                          }}
                        >
                          <option value="never">Never</option>
                          <option value="date">On a date</option>
                          <option value="count">After a number of runs</option>
                        </select>
                      </label>
                      {endMode === "date" ? (
                        <label className="task-schedule-field">
                          <span>Ends on</span>
                          <input
                            type="date"
                            aria-label="Repeat end date"
                            value={endDate}
                            onChange={(e) => {
                              setEndDate(e.target.value);
                              setFieldError("recurrence_end", "");
                            }}
                          />
                        </label>
                      ) : null}
                      {endMode === "count" ? (
                        <label className="task-schedule-field">
                          <span>Total runs</span>
                          <input
                            type="number"
                            min={1}
                            max={10000}
                            aria-label="Total runs before the repeat ends"
                            value={endCount}
                            onChange={(e) => {
                              setEndCount(e.target.value);
                              setFieldError("recurrence_end", "");
                            }}
                          />
                        </label>
                      ) : null}
                    </div>
                    {errors.recurrence_end ? (
                      <div className="task-inline-error" data-testid="error-recurrence-end">
                        {errors.recurrence_end}
                      </div>
                    ) : null}
                  </div>
                ) : null}
              </section>

              {/* ── Email results ──────────────────────────────────────── */}
              <section className="task-group">
                <div className="task-label-row">
                  <label className="task-label" htmlFor="emailChipInput">
                    Email results
                  </label>
                </div>
                <div
                  className={`task-chipfield${emailError ? " has-error" : ""}`}
                  onClick={() => emailInputRef.current?.focus()}
                  role="group"
                  aria-label="Email recipients"
                >
                  {emails.map((e) => (
                    <span key={e} className="task-chip" data-email={e}>
                      {e}
                      <button
                        type="button"
                        className="task-chip-delete"
                        aria-label={`Remove ${e}`}
                        onClick={() => setEmails(emails.filter((x) => x !== e))}
                      >
                        <svg
                          width="10"
                          height="10"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="2.4"
                          strokeLinecap="round"
                          aria-hidden="true"
                        >
                          <path d="M18 6L6 18M6 6l12 12" />
                        </svg>
                      </button>
                    </span>
                  ))}
                  <input
                    id="emailChipInput"
                    ref={emailInputRef}
                    type="email"
                    className="task-chipfield-input"
                    placeholder={emails.length === 0 ? "Add email — press Enter" : "Add email…"}
                    maxLength={254}
                    aria-describedby={emailError ? "email-error email-help" : "email-help"}
                    value={emailInput}
                    onChange={(e) => {
                      setEmailInput(e.target.value);
                      if (emailError) setEmailError("");
                    }}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === ",") {
                        e.preventDefault();
                        commitEmail();
                      } else if (e.key === "Backspace" && emailInput === "" && emails.length > 0) {
                        setEmails(emails.slice(0, -1));
                      }
                    }}
                    onBlur={() => {
                      if (emailInput.trim()) commitEmail();
                    }}
                  />
                </div>
                {emailError ? (
                  <div className="task-inline-error" id="email-error" data-testid="error-email">
                    <svg
                      width="13"
                      height="13"
                      viewBox="0 0 24 24"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="2"
                      strokeLinecap="round"
                      aria-hidden="true"
                    >
                      <circle cx="12" cy="12" r="9" />
                      <path d="M12 8v4M12 15.5v.5" />
                    </svg>
                    {emailError}
                  </div>
                ) : (
                  <p className="field-hint" id="email-help">
                    The agent is instructed to email its report to these recipients after each run.
                  </p>
                )}
              </section>

              {/* ── Tools & files reveal ───────────────────────────────── */}
              <Reveal
                id="task-tools-body"
                label="Tools & files"
                hint="MCP servers · attachments"
                open={toolsOpen}
                onToggle={() => setToolsOpen((o) => !o)}
                right={<span className="task-reveal-summary">{toolsSummary}</span>}
              >
                <div className="form-group" data-testid="task-mcp-section">
                  <span className="task-field-label">MCP servers</span>
                  <p className="field-hint task-hint-tight">
                    Everything off stays sandbox-only. Each enabled server acts as the credential
                    account you pick.
                  </p>
                  <McpServerPicker
                    mode="task"
                    servers={servers}
                    selection={mcpSelection}
                    onChange={setMcpSelection}
                  />
                </div>
                <div className="form-group">
                  <span className="task-field-label">Attachments</span>
                  <FileUpload
                    registerHandle={(h) => (fileHandle.current = h)}
                    onEntriesChange={(entries: FileEntry[]) =>
                      setFileCount(entries.filter((e) => e.status !== "invalid").length)
                    }
                  />
                </div>
              </Reveal>

              {/* ── Advanced reveal ────────────────────────────────────── */}
              <Reveal
                id="task-advanced-body"
                label="Advanced"
                hint="model · limits · behavior · gate"
                open={advancedOpen}
                onToggle={() => setAdvancedOpen((o) => !o)}
                right={
                  advancedCount > 0 ? (
                    <span className="task-count-chip">{advancedCount} set</span>
                  ) : undefined
                }
              >
                <div data-testid="advanced-section" className="task-advanced">
                  <div className="task-subcluster">
                    <div className="task-eyebrow">Model</div>
                    <div className="field-grid">
                      <div className="form-group">
                        <label className="task-field-label" htmlFor="taskModelInput">
                          Primary
                        </label>
                        <ModelPicker
                          id="taskModelInput"
                          value={model}
                          onChange={setModel}
                          placeholder="deepseek/deepseek-v4-flash-0731"
                        />
                        {errors.model ? (
                          <div className="validation-error" data-testid="error-model">
                            {errors.model}
                          </div>
                        ) : null}
                      </div>
                      <div className="form-group">
                        <label className="task-field-label" htmlFor="taskFallbackModelInput">
                          Fallback
                        </label>
                        <ModelPicker
                          id="taskFallbackModelInput"
                          value={fallbackModel}
                          onChange={setFallbackModel}
                          placeholder="moonshotai/kimi-k2.6"
                        />
                        {errors.fallback_model ? (
                          <div className="validation-error" data-testid="error-fallback-model">
                            {errors.fallback_model}
                          </div>
                        ) : null}
                      </div>
                    </div>
                    <p className="field-hint">
                      From your workspace&apos;s providers — type to use an unlisted slug.
                    </p>
                  </div>

                  <div className="task-subcluster">
                    <div className="task-eyebrow">Limits</div>
                    <div className="task-limits-grid">
                      <label className="task-limit-label" htmlFor="taskMaxIterations">
                        Max iterations
                      </label>
                      <input
                        id="taskMaxIterations"
                        type="number"
                        min={1}
                        max={10000}
                        step={1}
                        inputMode="numeric"
                        placeholder="default"
                        aria-describedby="max-iterations-help"
                        value={maxIterations}
                        onChange={(e) => setMaxIterations(e.target.value)}
                      />
                      <span className="task-limit-help" id="max-iterations-help">
                        Blank = server default
                      </span>
                      {errors.max_iterations ? (
                        <div
                          className="validation-error task-limits-error"
                          data-testid="error-max-iterations"
                        >
                          {errors.max_iterations}
                        </div>
                      ) : null}
                      <label className="task-limit-label" htmlFor="taskExpectedDuration">
                        Expected duration
                      </label>
                      <input
                        id="taskExpectedDuration"
                        type="number"
                        min={1}
                        step={1}
                        inputMode="numeric"
                        placeholder="none"
                        aria-describedby="duration-help"
                        value={expectedDuration}
                        onChange={(e) => setExpectedDuration(e.target.value)}
                      />
                      <span className="task-limit-help" id="duration-help">
                        Minutes. Warns past 1.5×, breach at 2×. Blank = no SLA.
                      </span>
                      <label className="task-limit-label" htmlFor="taskThinkingBudget">
                        Thinking budget
                      </label>
                      <input
                        id="taskThinkingBudget"
                        type="number"
                        min={0}
                        step={1024}
                        inputMode="numeric"
                        placeholder="inherit"
                        aria-describedby="thinking-help"
                        value={thinkingBudget}
                        onChange={(e) => setThinkingBudget(e.target.value)}
                      />
                      <span className="task-limit-help" id="thinking-help">
                        Tokens, Claude models. Blank = inherit · 0 = off.
                      </span>
                      <label className="task-limit-label" htmlFor="taskSandboxMemory">
                        Sandbox memory
                      </label>
                      <input
                        id="taskSandboxMemory"
                        type="number"
                        min={128}
                        step={128}
                        inputMode="numeric"
                        placeholder="default"
                        aria-describedby="sandbox-memory-help"
                        value={sandboxMemory}
                        onChange={(e) => setSandboxMemory(e.target.value)}
                      />
                      <span className="task-limit-help" id="sandbox-memory-help">
                        MiB. Blank = global default. Floor 128.
                      </span>
                      <label className="task-limit-label" htmlFor="taskSandboxCpus">
                        Sandbox CPUs
                      </label>
                      <input
                        id="taskSandboxCpus"
                        type="number"
                        min={0.1}
                        step={0.5}
                        inputMode="decimal"
                        placeholder="default"
                        aria-describedby="sandbox-cpus-help"
                        value={sandboxCpus}
                        onChange={(e) => setSandboxCpus(e.target.value)}
                      />
                      <span className="task-limit-help" id="sandbox-cpus-help">
                        Fractional CPUs. Blank = global default.
                      </span>
                      <label className="task-limit-label" htmlFor="taskSandboxPids">
                        Sandbox PIDs
                      </label>
                      <input
                        id="taskSandboxPids"
                        type="number"
                        min={16}
                        step={16}
                        inputMode="numeric"
                        placeholder="default"
                        aria-describedby="sandbox-pids-help"
                        value={sandboxPids}
                        onChange={(e) => setSandboxPids(e.target.value)}
                      />
                      <span className="task-limit-help" id="sandbox-pids-help">
                        Max processes. Blank = global default. Floor 16.
                      </span>
                    </div>
                  </div>

                  <div className="task-subcluster">
                    <div className="task-eyebrow">Behavior</div>
                    <div className="task-switch-list">
                      <label className="switch-row">
                        <input
                          type="checkbox"
                          className="ui-switch"
                          checked={captainsLog}
                          onChange={(e) => setCaptainsLog(e.target.checked)}
                        />
                        <span className="switch-row-text">
                          <span className="switch-row-title">
                            Persistent memory · Captain&apos;s Log
                          </span>
                          <span className="switch-row-desc">
                            Saves facts with <code>remember</code> and reloads them at the start of
                            every run. Scoped to this task.
                          </span>
                        </span>
                      </label>
                      <label className="switch-row">
                        <input
                          type="checkbox"
                          className="ui-switch"
                          checked={allowNetwork}
                          onChange={(e) => setAllowNetwork(e.target.checked)}
                        />
                        <span className="switch-row-text">
                          <span className="switch-row-title">Allow network egress</span>
                          <span className="switch-row-desc">
                            Off = sealed — the sandbox has no internet access.
                          </span>
                        </span>
                      </label>
                      <label className="switch-row">
                        <input
                          type="checkbox"
                          className="ui-switch"
                          checked={allowDelegation}
                          onChange={(e) => setAllowDelegation(e.target.checked)}
                        />
                        <span className="switch-row-text">
                          <span className="switch-row-title">Allow sub-agent delegation</span>
                          <span className="switch-row-desc">
                            The agent may fan work out to governed child agents (capped at 5,
                            each ≤10% of the remaining budget). Off = this task never delegates.
                          </span>
                        </span>
                      </label>
                      <label className="switch-row">
                        <input
                          type="checkbox"
                          className="ui-switch"
                          checked={carryContext}
                          onChange={(e) => setCarryContext(e.target.checked)}
                        />
                        <span className="switch-row-text">
                          <span className="switch-row-title">Carry context across runs</span>
                          <span className="switch-row-desc">
                            Each run starts with a bounded summary of the previous run&apos;s
                            output. Recurring tasks only.
                          </span>
                        </span>
                      </label>
                    </div>
                  </div>

                  <div className="task-gate">
                    <div className="task-gate-head">
                      <span className="task-gate-title">Pre-run gate</span>
                      <span className="task-gate-chip">run_if</span>
                      <span className="task-reveal-spacer" />
                    </div>
                    <p className="task-gate-warning">
                      Runs on the host as the fleet process user — treat the command as trusted.
                    </p>
                    <input
                      id="runIfCommandInput"
                      type="text"
                      name="run_if_command"
                      className="task-gate-command"
                      maxLength={2000}
                      placeholder='git -C /workspace log --since="24 hours ago" --oneline | grep -q .'
                      value={runIfCommand}
                      onChange={(e) => setRunIfCommand(e.target.value)}
                      aria-label="Pre-run shell command"
                      aria-describedby="run-if-help"
                    />
                    <div className="task-gate-options">
                      <label htmlFor="runIfTimeoutInput" className="task-gate-option-label">
                        Timeout
                      </label>
                      <input
                        id="runIfTimeoutInput"
                        type="number"
                        min={1}
                        max={300}
                        step={1}
                        value={runIfTimeout}
                        onChange={(e) => {
                          const n = Number.parseInt(e.target.value, 10);
                          if (!Number.isNaN(n)) setRunIfTimeout(n);
                        }}
                        onBlur={() => setRunIfTimeout((t) => Math.min(300, Math.max(1, t)))}
                        aria-label="Pre-run gate timeout seconds"
                      />
                      <span className="task-gate-option-label">s</span>
                      <span className="task-gate-option-gap" />
                      <label htmlFor="runIfOnErrorSelect" className="task-gate-option-label">
                        On error
                      </label>
                      <select
                        id="runIfOnErrorSelect"
                        value={runIfOnError}
                        onChange={(e) => setRunIfOnError(e.target.value as "run" | "skip")}
                      >
                        <option value="run">run anyway</option>
                        <option value="skip">skip</option>
                      </select>
                    </div>
                    {errors.run_if ? (
                      <div className="validation-error" data-testid="error-run-if">
                        {errors.run_if}
                      </div>
                    ) : null}
                    <p className="field-hint task-hint-tight" id="run-if-help">
                      The occurrence runs only when this exits 0 — otherwise it&apos;s skipped.
                    </p>
                  </div>
                </div>
              </Reveal>
            </fieldset>
          </form>
        </div>

        {/* Fixed footer — estimate hugged left, actions hugged right; the
            submit button stays wired to the form via the `form` attribute. */}
        <div
          className={`modal-footer task-footer${scrolledBottom ? " is-scrolled" : ""}`}
          inert={discardOpen || scopeOpen ? true : undefined}
        >
          <button
            type="button"
            className="btn btn-secondary task-estimate-btn"
            aria-label="Estimate cost"
            disabled={estimating || submitting}
            onClick={() => void estimate()}
          >
            Estimate Cost
          </button>
          <span className="task-estimate-slot" role="status" aria-live="polite">
            {estimating ? (
              <span className="task-estimate-value">
                <Spinner /> Estimating…
              </span>
            ) : forecast ? (
              <span className={`task-estimate-result${estimateStale ? " is-stale" : ""}`}>
                <span className="task-estimate-value" tabIndex={0}>
                  {forecast.pricing_known && forecast.estimated_total_cost_usd != null
                    ? `≈ $${forecast.estimated_total_cost_usd.toFixed(2)} per run`
                    : "Cost unknown for this model"}
                  {forecast.would_hit_ceiling ? (
                    <span className="task-estimate-ceiling">
                      {" "}
                      · may hit the ${forecast.max_cost_ceiling_usd.toFixed(2)} ceiling
                    </span>
                  ) : null}
                </span>
                <span className="task-estimate-popover">
                  <CostForecastPanel forecast={forecast} />
                  {estimateStale ? (
                    <p className="task-estimate-stale-note">
                      The form changed since this estimate — run it again.
                    </p>
                  ) : null}
                </span>
              </span>
            ) : estimateNote ? (
              <span className="task-estimate-note">{estimateNote}</span>
            ) : null}
          </span>
          <span className="task-footer-spacer" />
          {footerStatus ? (
            <span className={footerStatus.className} id="task-footer-status">
              {footerStatus.text}
            </span>
          ) : null}
          <button
            type="button"
            className="btn btn-secondary"
            aria-label="Cancel"
            disabled={submitting}
            onClick={requestClose}
          >
            Cancel
          </button>
          <button
            type="submit"
            form="createTaskForm"
            className="btn btn-primary task-launch-btn"
            aria-label={editing ? "Save task changes" : "Launch task"}
            aria-describedby={footerStatus ? "task-footer-status" : undefined}
            disabled={promptEmpty || submitting}
          >
            {submitting ? (
              <>
                <Spinner /> {editing ? "Saving…" : "Launching…"}
              </>
            ) : editing ? (
              editTerminal ? (
                "Resubmit with changes"
              ) : (
                "Save changes"
              )
            ) : (
              "Launch Task"
            )}
          </button>
        </div>

        {/* Recurring-edit scope chooser: PUT rewrites every future run; the
            run-once path resubmits a one-off copy and leaves the schedule
            untouched. Asked at save time so the user decides with the final
            values in front of them. */}
        {scopeOpen ? (
          <div className="task-discard-overlay">
            <div
              className="task-discard-card task-scope-card"
              role="alertdialog"
              aria-modal="true"
              aria-labelledby="edit-scope-title"
              aria-describedby="edit-scope-message"
              data-testid="edit-scope-chooser"
            >
              <div className="task-discard-title" id="edit-scope-title">
                Apply changes to…
              </div>
              <div className="task-discard-message" id="edit-scope-message">
                This task repeats on a schedule. Save the changes for every
                future run, or run once now with these changes and leave the
                schedule as it is?
              </div>
              <div className="task-discard-actions">
                <button
                  type="button"
                  className="btn btn-secondary"
                  onClick={() => setScopeOpen(false)}
                >
                  Back
                </button>
                <button
                  type="button"
                  className="btn btn-secondary"
                  data-testid="edit-scope-once"
                  onClick={() => {
                    setScopeOpen(false);
                    void submit("once");
                  }}
                >
                  Run once now
                </button>
                <button
                  type="button"
                  className="btn btn-primary"
                  data-testid="edit-scope-definition"
                  onClick={() => {
                    setScopeOpen(false);
                    void submit("definition");
                  }}
                >
                  All future runs
                </button>
              </div>
            </div>
          </div>
        ) : null}

        {/* Dirty-close guard — an in-modal confirm naming what would be lost.
            "Keep editing" returns focus to where the close was requested from. */}
        {discardOpen ? (
          <div className="task-discard-overlay">
            <div
              className="task-discard-card"
              role="alertdialog"
              aria-modal="true"
              aria-labelledby="discard-title"
              aria-describedby="discard-message"
            >
              <div className="task-discard-title" id="discard-title">
                Discard this task?
              </div>
              <div className="task-discard-message" id="discard-message">
                You have unsaved changes — {dirtyList} will be lost if you close.
              </div>
              <div className="task-discard-actions">
                <button
                  type="button"
                  ref={keepEditingRef}
                  className="btn btn-secondary"
                  onClick={() => {
                    setDiscardOpen(false);
                    resumeFocusRef.current?.focus();
                  }}
                >
                  Keep editing
                </button>
                <button
                  type="button"
                  className="btn task-discard-confirm"
                  onClick={() => {
                    resetForm();
                    onClose();
                  }}
                >
                  Discard
                </button>
              </div>
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}

export default TaskCreateModal;
