"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import type { CostForecast, McpServer, MCPChoice, TaskCreate, TaskTemplate } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { applyTemplateVars, promptableVars } from "@/app/shared/lib/taskTemplates";
import { validateTaskForm, validateCronExpression, describeEmailError } from "@/app/shared/lib/validation";
import { isValidEmail } from "@/app/shared/lib/format";
import { describeCronExpression } from "@/app/shared/lib/cron";
import { nextCronOccurrence, formatNextRun } from "@/app/shared/lib/cronNext";
import { useToast } from "@/app/shared/ui/Toast";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { ModelPicker } from "@/app/shared/ui/ModelPicker";
import { McpServerPicker } from "@/app/shared/ui/McpServerPicker";
import { FileUpload, type FileUploadHandle, type FileEntry } from "@/app/shared/ui/FileUpload";
import { CostForecastPanel } from "./CostForecastPanel";

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

const DEFAULT_PRIMARY_MODEL = "z-ai/glm-5.2";
const DEFAULT_FALLBACK_MODEL = "openai/gpt-5.6-sol";

const SCHEDULE_PRESETS = [
  { label: "Weekdays 9am", cron: "0 9 * * 1-5" },
  { label: "Weekly Mon", cron: "0 9 * * 1" },
  { label: "Mon & Thu 1pm", cron: "0 13 * * 1,4" },
  { label: "Wed 5am", cron: "0 5 * * 3" },
];

type ScheduleMode = "now" | "once" | "repeat";
type RepeatEditor = "simple" | "cron";
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
};

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

export function TaskCreateModal({ open, servers, serversLoading, onClose, onCreated }: TaskCreateModalProps) {
  const { showToast } = useToast();

  const [prompt, setPrompt] = useState("");
  const [description, setDescription] = useState("");
  const [tagsInput, setTagsInput] = useState("");
  const [persona, setPersona] = useState("");

  const [emails, setEmails] = useState<string[]>([]);
  const [emailInput, setEmailInput] = useState("");
  const [emailError, setEmailError] = useState("");

  const [scheduleMode, setScheduleMode] = useState<ScheduleMode>("now");
  const [scheduledDate, setScheduledDate] = useState("");
  const [scheduledTime, setScheduledTime] = useState("09:00");
  const [recurrence, setRecurrence] = useState("");
  const [repeatEditor, setRepeatEditor] = useState<RepeatEditor>("simple");
  const [simpleFrequency, setSimpleFrequency] = useState<SimpleFrequency>("weekdays");
  const [simpleTime, setSimpleTime] = useState("09:00");
  const [simpleWeekdays, setSimpleWeekdays] = useState<string[]>(["1"]);

  const [contextOpen, setContextOpen] = useState(false);
  const [toolsOpen, setToolsOpen] = useState(false);
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const [model, setModel] = useState(DEFAULT_PRIMARY_MODEL);
  const [fallbackModel, setFallbackModel] = useState(DEFAULT_FALLBACK_MODEL);
  const [maxIterations, setMaxIterations] = useState("");
  const [captainsLog, setCaptainsLog] = useState(false);
  const [allowNetwork, setAllowNetwork] = useState(false);
  const [carryContext, setCarryContext] = useState(false);
  // Pre-run shell gate (#269): empty = no gate (unconditional promotion).
  const [runIfCommand, setRunIfCommand] = useState("");
  const [runIfOnError, setRunIfOnError] = useState<"run" | "skip">("run");
  const [runIfTimeout, setRunIfTimeout] = useState(30);
  // SLA expected duration (#274): blank = no SLA. Stored as a string so the
  // empty/typing states round-trip cleanly; parsed to int on submit.
  const [expectedDuration, setExpectedDuration] = useState("");
  // Per-task extended-thinking override (#220): "" = inherit the deployment
  // default, "0" = off, a positive value = this task's budget in tokens.
  const [thinkingBudget, setThinkingBudget] = useState("");

  // The per-task MCP selection (replaces the legacy target_node_name).
  const [mcpSelection, setMcpSelection] = useState<MCPChoice[]>([]);
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
  const emailInputRef = useRef<HTMLInputElement | null>(null);
  // Outside-click close: only when both mousedown AND click land on the overlay
  // itself (a drag from inside the form that ends on the overlay must not
  // close).
  const overlayMouseDownOnSelf = useRef(false);

  // Task templates (#262): the bundle's read-only catalog of pre-filled task
  // shapes. Fetched once when the modal opens; an empty catalog (or a fetch
  // failure) suppresses the section — the blank form is always available.
  const [templates, setTemplates] = useState<TaskTemplate[]>([]);

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

  const dirty =
    prompt.trim() !== "" ||
    description.trim() !== "" ||
    tagsInput.trim() !== "" ||
    persona.trim() !== "" ||
    emails.length > 0 ||
    emailInput.trim() !== "" ||
    scheduleMode !== "now" ||
    recurrence.trim() !== "" ||
    scheduledDate !== "" ||
    mcpSelection.length > 0 ||
    fileCount > 0 ||
    model !== DEFAULT_PRIMARY_MODEL ||
    fallbackModel !== DEFAULT_FALLBACK_MODEL ||
    maxIterations.trim() !== "" ||
    expectedDuration.trim() !== "" ||
    thinkingBudget.trim() !== "" ||
    captainsLog ||
    allowNetwork ||
    carryContext ||
    runIfCommand.trim() !== "";

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
    setMcpSelection([]);
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

  // applyTemplate pre-fills the form from a template. Built-in variables ({date},
  // {user_name}) are substituted automatically; any remaining custom {token} is
  // collected through a small prompt() per variable, then substituted. Every
  // field stays editable afterward — this only seeds the form. The task is still
  // created through the ordinary submit/createTask path.
  const applyTemplate = (tpl: TaskTemplate) => {
    const ctx = { userName: undefined as string | undefined };
    const userValues: Record<string, string> = {};
    for (const name of promptableVars(tpl.variables ?? [], ctx)) {
      const entered = window.prompt(`Value for {${name}}`, "");
      if (entered != null && entered !== "") userValues[name] = entered;
    }
    const t = tpl.task ?? {};
    setPrompt(t.prompt ? applyTemplateVars(t.prompt, userValues, ctx) : "");
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
    setMaxIterations(typeof t.max_iterations === "number" ? String(t.max_iterations) : "");
    if (t.description || t.tags?.length || t.persona) setContextOpen(true);
    if (t.persona || t.allow_network || t.carry_context || t.instruction_self_improve) {
      setAdvancedOpen(true);
    }
  };

  if (!open) return null;

  // ── Derived display state ─────────────────────────────────────────────────

  const cronDescription = describeCronExpression(recurrence);
  const cronNext = cronDescription ? nextCronOccurrence(recurrence) : null;

  const contextCount = [description, tagsInput, persona].filter((v) => v.trim() !== "").length;

  const advancedCount = [
    model !== DEFAULT_PRIMARY_MODEL,
    fallbackModel !== DEFAULT_FALLBACK_MODEL,
    maxIterations.trim() !== "",
    expectedDuration.trim() !== "",
    thinkingBudget.trim() !== "",
    captainsLog,
    allowNetwork,
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
    if (carryContext) taskData.carry_context = true;
    if (mcpSelection.length > 0) taskData.mcp_selection = mcpSelection;
    if (scheduleMode === "once" && scheduledFor) {
      taskData.scheduled_for = new Date(scheduledFor).toISOString();
    }
    if (scheduleMode === "repeat" && recurrence.trim()) taskData.recurrence = recurrence.trim();
    // Pre-run shell gate (#269): only attach when a command is set so the
    // default (no gate) round-trips as run_if: null.
    if (runIfCommand.trim()) {
      taskData.run_if = {
        command: runIfCommand.trim(),
        on_error: runIfOnError,
        timeout_seconds: runIfTimeout,
      };
    }
    if (expectedDuration.trim()) {
      const mins = Number.parseInt(expectedDuration, 10);
    if (Number.isFinite(mins) && mins > 0) taskData.expected_duration_minutes = mins;
    }
    if (thinkingBudget.trim()) {
      const budget = Number.parseInt(thinkingBudget, 10);
      if (Number.isFinite(budget) && budget >= 0) taskData.thinking_budget_tokens = budget;
    }
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
    if (scheduleMode === "once" && !scheduledDate) next.scheduled_for = "Pick a date and time.";
    if (scheduleMode === "repeat" && !recurrence.trim())
      next.recurrence = "Type a cron expression or pick a preset below.";
    return next;
  };

  const ERROR_FOCUS_TARGETS: Record<string, string> = {
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
      setEstimateNote("Estimate failed — try again.");
      setForecast(null);
      // Surface the reason without a toast; the title carries the details.
      console.warn("estimate failed:", (err as Error).message);
    } finally {
      setEstimating(false);
    }
  };

  const submit = async () => {
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

    const taskData = buildTaskData(buildFinalPrompt(emails));
    setSubmitting(true);
    try {
      if (fileHandle.current?.hasFiles()) {
        const filenames = await fileHandle.current.uploadAll((file) =>
          orchestratorApi.uploadFile(file),
        );
        taskData.files = filenames;
      }
      await orchestratorApi.createTask(taskData);
      showToast("Task created successfully!", "success");
      onCreated();
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

  const autoGrowPrompt = (el: HTMLTextAreaElement) => {
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 240)}px`;
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
        aria-label="Create New Task"
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
          inert={discardOpen ? true : undefined}
        >
          <div className="modal-header-text">
            <h3>Create New Task</h3>
            <p className="modal-subtitle">Define what runs, when it runs, and what it may touch.</p>
          </div>
          <button
            type="button"
            className="icon-action modal-close"
            aria-label="Close modal"
            onClick={requestClose}
          >
            <svg
              width="14"
              height="14"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              aria-hidden="true"
            >
              <path d="M18 6L6 18M6 6l12 12" />
            </svg>
          </button>
        </div>

        <div
          className={`modal-body${submitting ? " is-dimmed" : ""}`}
          ref={bodyRef}
          onScroll={updateScrollShadows}
          inert={discardOpen ? true : undefined}
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
                  entirely when the bundle ships none. */}
              {templates.length > 0 ? (
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

              {/* ── Prompt ─────────────────────────────────────────────── */}
              <section className="task-group">
                <div className="task-label-row">
                  <label className="task-label" htmlFor="promptTextarea">
                    Prompt
                  </label>
                  <span className="task-required-badge">Required</span>
                </div>
                <textarea
                  id="promptTextarea"
                  name="prompt"
                  ref={promptRef}
                  className={`task-prompt-input${errors.prompt ? " has-error" : ""}`}
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
                          placeholder="z-ai/glm-5.2"
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
          inert={discardOpen ? true : undefined}
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
            aria-label="Launch task"
            aria-describedby={footerStatus ? "task-footer-status" : undefined}
            disabled={promptEmpty || submitting}
          >
            {submitting ? (
              <>
                <Spinner /> Launching…
              </>
            ) : (
              "Launch Task"
            )}
          </button>
        </div>

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
