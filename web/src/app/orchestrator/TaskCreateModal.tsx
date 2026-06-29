"use client";

import { useEffect, useRef, useState } from "react";
import type { CostForecast, McpServer, MCPChoice, TaskCreate, TaskTemplate } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { applyTemplateVars, promptableVars } from "@/app/shared/lib/taskTemplates";
import { validateTaskForm } from "@/app/shared/lib/validation";
import { describeCronExpression } from "@/app/shared/lib/cron";
import { useToast } from "@/app/shared/ui/Toast";
import { ModelPicker } from "@/app/shared/ui/ModelPicker";
import { McpServerPicker } from "@/app/shared/ui/McpServerPicker";
import { FileUpload, type FileUploadHandle } from "@/app/shared/ui/FileUpload";
import { CostForecastPanel } from "./CostForecastPanel";

// TaskCreateModal — the create-task form. React port of moc's tasks.js +
// modals.js create-task modal, with ONE structural change from moc: the
// `target_node_name` input is GONE, replaced by the shared
// <McpServerPicker mode="task"> (enable/disable per MCP + per-MCP credential
// account dropdown). The global concurrency cap setting also lives here, under
// Advanced Settings.

const DEFAULT_PRIMARY_MODEL = "anthropic/claude-opus-4.8";
const DEFAULT_FALLBACK_MODEL = "moonshotai/kimi-k2.6";

const SCHEDULE_PRESETS = [
  { label: "Weekdays 9am", cron: "0 9 * * 1-5" },
  { label: "Weekly Mon", cron: "0 9 * * 1" },
  { label: "Mon & Thu 1pm", cron: "0 13 * * 1,4" },
  { label: "Wed 5am", cron: "0 5 * * 3" },
];

const MAX_ITER_OPTIONS = [
  { value: "", label: "500 (Default)" },
  { value: "250", label: "250" },
  { value: "100", label: "100" },
  { value: "__custom__", label: "Custom" },
];

export type TaskCreateModalProps = {
  open: boolean;
  servers: McpServer[];
  onClose: () => void;
  onCreated: () => void;
};

export function TaskCreateModal({ open, servers, onClose, onCreated }: TaskCreateModalProps) {
  const { showToast } = useToast();

  const [prompt, setPrompt] = useState("");
  const [description, setDescription] = useState("");
  const [tagsInput, setTagsInput] = useState("");
  const [persona, setPersona] = useState("");
  const [emails, setEmails] = useState<string[]>([]);
  const [customEmail, setCustomEmail] = useState("");
  const [scheduledDate, setScheduledDate] = useState("");
  const [scheduledTime, setScheduledTime] = useState("09:00");
  const [recurrence, setRecurrence] = useState("");
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [model, setModel] = useState(DEFAULT_PRIMARY_MODEL);
  const [fallbackModel, setFallbackModel] = useState(DEFAULT_FALLBACK_MODEL);
  const [maxIterSelect, setMaxIterSelect] = useState("");
  const [maxIterCustom, setMaxIterCustom] = useState("");
  const [captainsLog, setCaptainsLog] = useState(false);
  const [allowNetwork, setAllowNetwork] = useState(false);
  // Pre-run shell gate (#269): empty = no gate (legacy unconditional promotion).
  // runIfError holds the server-side validation message (empty string = none).
  const [runIfCommand, setRunIfCommand] = useState("");
  const [runIfOnError, setRunIfOnError] = useState<"run" | "skip">("run");
  const [runIfTimeout, setRunIfTimeout] = useState(30);

  // The NEW per-task MCP selection (replaces target_node_name).
  const [mcpSelection, setMcpSelection] = useState<MCPChoice[]>([]);

  const [errors, setErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);
  // Cost-estimate state (#233): the latest forecast and whether a request is in
  // flight. The estimate is advisory and never gates Submit.
  const [estimating, setEstimating] = useState(false);
  const [forecast, setForecast] = useState<CostForecast | null>(null);

  const fileHandle = useRef<FileUploadHandle | null>(null);

  // Task templates (#262): the bundle's read-only catalog of pre-filled task
  // shapes. Fetched once when the modal opens; an empty catalog (or a fetch
  // failure) simply suppresses the template section — the blank-form behavior is
  // always available.
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
        // Templates are a convenience; a failure must not block task creation.
        if (!cancelled) setTemplates([]);
      });
    return () => {
      cancelled = true;
    };
  }, [open]);

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
    setRecurrence(t.recurrence ?? "");
    setModel(t.model ?? DEFAULT_PRIMARY_MODEL);
    setFallbackModel(t.fallback_model ?? DEFAULT_FALLBACK_MODEL);
    setAllowNetwork(Boolean(t.allow_network));
    setCaptainsLog(Boolean(t.instruction_self_improve));
    if (typeof t.max_iterations === "number") {
      const known = MAX_ITER_OPTIONS.some((o) => o.value === String(t.max_iterations));
      if (known) {
        setMaxIterSelect(String(t.max_iterations));
        setMaxIterCustom("");
      } else {
        setMaxIterSelect("__custom__");
        setMaxIterCustom(String(t.max_iterations));
      }
    } else {
      setMaxIterSelect("");
      setMaxIterCustom("");
    }
    if (t.recurrence || t.persona || t.allow_network || t.instruction_self_improve) {
      setAdvancedOpen(true);
    }
  };

  if (!open) return null;

  const cronDescription = describeCronExpression(recurrence);

  const scheduledFor =
    scheduledDate ? `${scheduledDate}T${scheduledTime || "09:00"}` : "";

  const maxIterations = maxIterSelect === "__custom__" ? maxIterCustom : maxIterSelect;

  const addEmail = () => {
    const e = customEmail.trim().toLowerCase();
    if (!e || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(e)) {
      showToast("Please enter a valid email address", "error");
      return;
    }
    if (!emails.includes(e)) setEmails([...emails, e]);
    setCustomEmail("");
  };

  const buildFinalPrompt = (): string => {
    const base = prompt.trim();
    if (emails.length === 0) return base;
    const yaml = emails.map((e) => `    - ${e}`).join("\n");
    return `${base}\n\n---\nCRITICAL ACTION\nemail:\n  action: send_report\n  tool: email\n  instruction: "The following action is MANDATORY after completing the core task."\n  description: "Send the full report and findings to the listed recipients."\n  recipients:\n${yaml}\n---`;
  };

  // buildTaskData assembles the TaskCreate body shared by submit and the cost
  // estimate. Returns null (after toasting) on a malformed scheduled time so the
  // caller can abort. Files are handled separately by submit (the estimate does
  // not need them).
  const buildTaskData = (finalPrompt: string): TaskCreate | null => {
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
    if (maxIterations) taskData.max_iterations = Number.parseInt(maxIterations, 10);
    if (captainsLog) taskData.instruction_self_improve = true;
    if (allowNetwork) taskData.allow_network = true;
    if (mcpSelection.length > 0) taskData.mcp_selection = mcpSelection;
    if (scheduledFor) {
      try {
        taskData.scheduled_for = new Date(scheduledFor).toISOString();
      } catch {
        showToast("Invalid scheduled time format", "error");
        return null;
      }
    }
    if (recurrence) taskData.recurrence = recurrence;
    // Pre-run shell gate (#269): only attach when a command is set so the
    // default (no gate) round-trips as run_if: null.
    if (runIfCommand.trim()) {
      taskData.run_if = {
        command: runIfCommand.trim(),
        on_error: runIfOnError,
        timeout_seconds: runIfTimeout,
      };
    }
    return taskData;
  };

  // estimate fetches a pre-submission cost forecast (#233) for the current form
  // values and shows it inline. Advisory only — it never blocks Submit.
  const estimate = async () => {
    const finalPrompt = buildFinalPrompt();
    if (!finalPrompt.trim()) {
      showToast("Enter a prompt to estimate cost", "error");
      return;
    }
    const taskData = buildTaskData(finalPrompt);
    if (!taskData) return;
    setEstimating(true);
    setForecast(null);
    try {
      const fc = await orchestratorApi.estimateTask(taskData);
      setForecast(fc);
    } catch (err) {
      showToast(`Estimate failed: ${(err as Error).message}`, "error");
    } finally {
      setEstimating(false);
    }
  };

  const submit = async () => {
    const finalPrompt = buildFinalPrompt();
    const validation = validateTaskForm({
      prompt: finalPrompt,
      model,
      fallback_model: fallbackModel,
      max_iterations: maxIterations,
      recurrence,
      scheduled_for: scheduledFor,
    });
    if (!validation.valid) {
      setErrors(validation.errors as Record<string, string>);
      showToast("Please fix validation errors", "error");
      return;
    }
    setErrors({});

    const taskData = buildTaskData(finalPrompt);
    if (!taskData) return; // a build-time error (e.g. bad date) already toasted

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
      onClose();
    } catch (err) {
      showToast(`Error: ${(err as Error).message}`, "error");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay is-open" role="dialog" aria-modal="true" aria-label="Create New Task">
      <div className="modal">
        <div className="modal-header">
          <h3>Create New Task</h3>
          <button type="button" className="icon-action modal-close" aria-label="Close modal" onClick={onClose}>
            ×
          </button>
        </div>
        <div className="modal-body">
          <form
            id="createTaskForm"
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            {/* Task templates (#262) — pre-filled starting points. Suppressed
                entirely when the bundle ships none. "Start from scratch" (the
                blank form below) is always available. */}
            {templates.length > 0 ? (
              <div className="form-group" data-testid="task-template-section">
                <div className="form-label-row">
                  <span className="form-label">Start from a template</span>
                  <span className="optional-badge">Optional</span>
                </div>
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
              </div>
            ) : null}

            {/* Prompt */}
            <div className="form-group">
              <label htmlFor="promptTextarea">Prompt / Command</label>
              <textarea
                id="promptTextarea"
                name="prompt"
                required
                maxLength={100000}
                placeholder="Enter the command or prompt for the runner..."
                value={prompt}
                onChange={(e) => setPrompt(e.target.value)}
              />
              {errors.prompt ? (
                <div className="validation-error" data-testid="error-prompt">
                  {errors.prompt}
                </div>
              ) : null}
            </div>

            {/* Documentation (#281) — optional operator notes, collapsed by default */}
            <div className="form-group">
              <details>
                <summary>Documentation (optional)</summary>
                <textarea
                  id="descriptionTextarea"
                  name="description"
                  maxLength={10000}
                  placeholder="Why this task exists, what it costs, side effects, the runbook if it fails, who owns it… (Markdown)"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                />
                <label htmlFor="tagsInput">Tags (comma-separated)</label>
                <input
                  id="tagsInput"
                  name="tags"
                  type="text"
                  placeholder="nightly, prod, data-pipeline"
                  value={tagsInput}
                  onChange={(e) => setTagsInput(e.target.value)}
                />
                <label htmlFor="personaInput">Persona (bundle persona name; blank = default)</label>
                <input
                  id="personaInput"
                  name="persona"
                  type="text"
                  placeholder="security-auditor"
                  value={persona}
                  onChange={(e) => setPersona(e.target.value)}
                />
              </details>
            </div>

            {/* Email recipients */}
            <div className="form-group">
              <div className="form-label-row">
                <span className="form-label">Email Results To</span>
                <span className="optional-badge">Optional</span>
              </div>
              <div className="chips-container" role="group" aria-label="Email recipients">
                {emails.map((e) => (
                  <span key={e} className="chip chip-email selected" data-email={e}>
                    {e}
                    <button
                      type="button"
                      className="chip-delete"
                      aria-label={`Remove ${e}`}
                      onClick={() => setEmails(emails.filter((x) => x !== e))}
                    >
                      ×
                    </button>
                  </span>
                ))}
              </div>
              <div className="custom-email-row">
                <input
                  type="email"
                  placeholder="Add custom email..."
                  aria-label="Custom email address"
                  maxLength={254}
                  value={customEmail}
                  onChange={(e) => setCustomEmail(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      addEmail();
                    }
                  }}
                />
                <button type="button" className="btn btn-secondary" aria-label="Add custom email" onClick={addEmail}>
                  Add
                </button>
              </div>
            </div>

            {/* MCP servers — replaces the old Target Agent input */}
            <div className="form-group" data-testid="task-mcp-section">
              <div className="form-label-row">
                <span className="form-label">MCP Servers</span>
                <span className="optional-badge">Optional</span>
              </div>
              <div className="advanced-setting-meta">
                Enable the MCP servers this task may use, and pick the credential account for each.
              </div>
              <McpServerPicker
                mode="task"
                servers={servers}
                selection={mcpSelection}
                onChange={setMcpSelection}
              />
            </div>

            {/* Upload */}
            <div className="form-group">
              <div className="form-label-row">
                <label>Upload Documents</label>
                <span className="optional-badge">Optional</span>
              </div>
              <FileUpload registerHandle={(h) => (fileHandle.current = h)} />
            </div>

            {/* Schedule date/time */}
            <div className="form-group">
              <div className="form-label-row">
                <label htmlFor="scheduledForDate">Schedule Date</label>
                <span className="optional-badge">Optional</span>
              </div>
              <div className="schedule-datetime-group">
                <input
                  id="scheduledForDate"
                  type="date"
                  aria-label="Schedule date"
                  value={scheduledDate}
                  onChange={(e) => setScheduledDate(e.target.value)}
                />
                <label htmlFor="scheduledForTime" className="schedule-time-label">
                  at
                </label>
                <input
                  id="scheduledForTime"
                  type="time"
                  aria-label="Schedule time"
                  value={scheduledTime}
                  onChange={(e) => setScheduledTime(e.target.value)}
                />
              </div>
              {errors.scheduled_for ? (
                <div className="validation-error" data-testid="error-scheduled">
                  {errors.scheduled_for}
                </div>
              ) : null}
            </div>

            {/* Recurrence presets */}
            <div className="form-group">
              <div className="form-label-row">
                <span className="form-label">Recurrence</span>
                <span className="optional-badge">Optional</span>
              </div>
              <div className="schedule-presets" role="radiogroup" aria-label="Schedule presets">
                {SCHEDULE_PRESETS.map((p) => (
                  <button
                    key={p.cron}
                    type="button"
                    className={`preset-btn${recurrence === p.cron ? " active" : ""}`}
                    data-cron={p.cron}
                    onClick={() => setRecurrence(p.cron)}
                  >
                    <div className="preset-label">{p.label}</div>
                    <div className="preset-cron">{p.cron}</div>
                  </button>
                ))}
              </div>
            </div>

            {/* Advanced settings */}
            <div className="form-group task-advanced-settings">
              <button
                type="button"
                className="advanced-toggle"
                aria-expanded={advancedOpen}
                onClick={() => setAdvancedOpen((o) => !o)}
              >
                <span className="arrow" aria-hidden="true">
                  {advancedOpen ? "▼" : "▶"}
                </span>
                <span>Advanced Task Settings</span>
              </button>

              {advancedOpen ? (
                <div data-testid="advanced-section">
                  <div className="form-group advanced-section-group">
                    <label htmlFor="recurrenceInput">Custom Cron Expression</label>
                    <input
                      id="recurrenceInput"
                      type="text"
                      name="recurrence"
                      maxLength={100}
                      placeholder="e.g. 0 9 * * 1-5 (Weekdays at 9am)"
                      value={recurrence}
                      onChange={(e) => setRecurrence(e.target.value)}
                    />
                    {cronDescription ? (
                      <div className="cron-description" aria-live="polite">
                        {cronDescription}
                      </div>
                    ) : null}
                    {errors.recurrence ? (
                      <div className="validation-error" data-testid="error-recurrence">
                        {errors.recurrence}
                      </div>
                    ) : null}
                  </div>

                  <div className="form-group advanced-section-group">
                    <label htmlFor="taskModelInput">Primary Model</label>
                    <ModelPicker
                      id="taskModelInput"
                      value={model}
                      onChange={setModel}
                      placeholder="anthropic/claude-opus-4.8"
                    />
                    {errors.model ? (
                      <div className="validation-error" data-testid="error-model">
                        {errors.model}
                      </div>
                    ) : null}
                  </div>

                  <div className="form-group advanced-section-group">
                    <label htmlFor="taskFallbackModelInput">Fallback Model</label>
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

                  <div className="form-group advanced-section-group">
                    <label htmlFor="taskMaxIterationsSelect">Max Iterations</label>
                    <select
                      id="taskMaxIterationsSelect"
                      value={maxIterSelect}
                      onChange={(e) => setMaxIterSelect(e.target.value)}
                    >
                      {MAX_ITER_OPTIONS.map((o) => (
                        <option key={o.value || "default"} value={o.value}>
                          {o.label}
                        </option>
                      ))}
                    </select>
                    {maxIterSelect === "__custom__" ? (
                      <input
                        type="number"
                        min={1}
                        max={10000}
                        step={1}
                        placeholder="Enter a custom iteration cap"
                        aria-label="Custom max iterations"
                        value={maxIterCustom}
                        onChange={(e) => setMaxIterCustom(e.target.value)}
                      />
                    ) : null}
                    {errors.max_iterations ? (
                      <div className="validation-error" data-testid="error-max-iterations">
                        {errors.max_iterations}
                      </div>
                    ) : null}
                  </div>

                  <div className="form-group advanced-section-group">
                    <div className="advanced-switch-row">
                      <label className="toggle-switch">
                        <input
                          type="checkbox"
                          checked={captainsLog}
                          onChange={(e) => setCaptainsLog(e.target.checked)}
                        />
                        <span className="toggle-slider" />
                      </label>
                      <div className="advanced-setting-meta">
                        Captain&apos;s Log — persistent agent memory and self-improvement PRs.
                      </div>
                    </div>
                    <div className="advanced-switch-row">
                      <label className="toggle-switch">
                        <input
                          type="checkbox"
                          checked={allowNetwork}
                          onChange={(e) => setAllowNetwork(e.target.checked)}
                        />
                        <span className="toggle-slider" />
                      </label>
                      <div className="advanced-setting-meta">
                        Allow network egress — let this task&apos;s sandbox reach the internet (off = sealed, <code>--network=none</code>).
                      </div>
                    </div>
                  </div>

                  <div className="form-group advanced-section-group">
                    <div className="form-label-row">
                      <span className="form-label">Pre-run gate (run_if)</span>
                      <span className="optional-badge">Optional</span>
                    </div>
                    <div className="advanced-setting-meta" style={{ marginBottom: "0.4rem" }}>
                      A host-side shell command evaluated before the task is promoted. The task runs
                      only when the command exits with the expected code; otherwise the occurrence is
                      skipped. Runs as the fleet process user with a restricted PATH — treat the command
                      as trusted.
                    </div>
                    <input
                      id="runIfCommandInput"
                      type="text"
                      name="run_if_command"
                      maxLength={2000}
                      placeholder='e.g. git -C /workspace log --since="24 hours ago" --oneline | grep -q .'
                      value={runIfCommand}
                      onChange={(e) => setRunIfCommand(e.target.value)}
                      aria-label="Pre-run shell command"
                    />
                    <div className="advanced-switch-row" style={{ marginTop: "0.4rem" }}>
                      <label htmlFor="runIfTimeoutInput" className="advanced-setting-meta">
                        Timeout (s):
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
                        style={{ width: "5rem" }}
                        aria-label="Pre-run gate timeout seconds"
                      />
                      <label htmlFor="runIfOnErrorSelect" className="advanced-setting-meta">
                        On error:
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
                  </div>
                </div>
              ) : null}
            </div>

            {forecast ? <CostForecastPanel forecast={forecast} /> : null}

            <div className="task-create-actions">
              <button
                type="button"
                className="btn btn-secondary"
                aria-label="Estimate cost"
                disabled={estimating}
                onClick={() => void estimate()}
              >
                {estimating ? "Estimating…" : "Estimate Cost"}
              </button>
              <button type="submit" className="btn btn-primary" aria-label="Launch task" disabled={submitting}>
                {submitting ? "Launching…" : "Launch Task"}
              </button>
            </div>
          </form>
        </div>
      </div>
    </div>
  );
}

export default TaskCreateModal;
