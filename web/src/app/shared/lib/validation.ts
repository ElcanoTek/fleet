// Validation helpers for the orchestrator task form. Ported from moc's
// assets/js/validation.js to TypeScript. Pure functions — no DOM, no DOMPurify
// (React escapes by default, so the moc `sanitizeHtml` shim is dropped).

import { DEFAULT_UPLOAD_MAX_BYTES } from "@/app/lib/uploadLimits";

export type ValidationResult = { valid: boolean; message: string };

export function validateCronExpression(cron: unknown): ValidationResult {
  if (!cron || typeof cron !== "string") {
    return { valid: true, message: "" }; // Optional field
  }
  const trimmed = cron.trim();
  if (trimmed === "") return { valid: true, message: "" };

  const parts = trimmed.split(/\s+/);
  // 6-field expressions stay accepted (an existing power-user affordance); the
  // guidance names the canonical 5-field shape.
  if (parts.length < 5 || parts.length > 6) {
    return {
      valid: false,
      message: `Cron needs 5 fields — got ${parts.length}. Format: min · hour · day · month · weekday.`,
    };
  }
  const cronRegex = /^[0-9*\-,/]+$/;
  for (const part of parts) {
    if (!cronRegex.test(part)) {
      return {
        valid: false,
        message: "Cron has invalid characters — use numbers, *, commas, dashes, and slashes.",
      };
    }
  }
  return { valid: true, message: "" };
}

// describeEmailError names WHY an address is invalid so the form's inline error
// is specific ("missing domain") rather than a generic rejection.
export function describeEmailError(email: string): string {
  const trimmed = email.trim();
  let reason = "";
  const at = trimmed.indexOf("@");
  if (at < 0) reason = " — missing @.";
  else if (at === 0) reason = " — missing the part before @.";
  else if (!/\.[^\s@.]+$/.test(trimmed.slice(at + 1))) reason = " — missing domain.";
  return `Not a valid email: "${trimmed}"${reason ? reason : "."}`;
}

export function validatePrompt(prompt: unknown): ValidationResult {
  if (!prompt || typeof prompt !== "string") {
    return { valid: false, message: "Prompt is required" };
  }
  const trimmed = prompt.trim();
  if (trimmed.length === 0) return { valid: false, message: "Prompt cannot be empty" };
  if (trimmed.length < 3) return { valid: false, message: "Prompt must be at least 3 characters" };
  if (new TextEncoder().encode(trimmed).length > 250000) {
    return { valid: false, message: "Prompt is too long (max 250,000 bytes)" };
  }
  return { valid: true, message: "" };
}

export function validateModel(model: unknown): ValidationResult {
  if (!model || typeof model !== "string") return { valid: true, message: "" };
  const trimmed = model.trim();
  if (trimmed === "") return { valid: true, message: "" };
  if (trimmed.length > 200) return { valid: false, message: "Model is too long (max 200 characters)" };
  if (/[\r\n]/.test(trimmed)) return { valid: false, message: "Model must be a single line" };
  return { valid: true, message: "" };
}

// Per-task run cost ceiling (#1533): blank = inherit the deployment ceiling; a
// value must be a real bound — the server refuses 0 (it would read as
// "unlimited") and anything above 100000. Whether a value may EXCEED the
// deployment ceiling is a permission question the server answers (admin).
export function validateMaxCostUSD(value: unknown): ValidationResult {
  if (value === null || value === undefined) return { valid: true, message: "" };
  const trimmed = String(value).trim();
  if (trimmed === "") return { valid: true, message: "" };
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) {
    return { valid: false, message: "Max cost must be a dollar amount (up to cents)" };
  }
  const parsed = Number.parseFloat(trimmed);
  if (!(parsed > 0) || parsed > 100000) {
    return { valid: false, message: "Max cost must be above 0 and at most 100000" };
  }
  return { valid: true, message: "" };
}

export function validateMaxIterations(value: unknown): ValidationResult {
  if (value === null || value === undefined) return { valid: true, message: "" };
  const trimmed = String(value).trim();
  if (trimmed === "") return { valid: true, message: "" };
  if (!/^\d+$/.test(trimmed)) {
    return { valid: false, message: "Max iterations must be a whole number" };
  }
  const parsed = Number.parseInt(trimmed, 10);
  if (parsed < 1 || parsed > 10000) {
    return { valid: false, message: "Max iterations must be between 1 and 10000" };
  }
  return { valid: true, message: "" };
}

export type FileLike = { name: string; size?: number; type?: string };

export type FileOptions = {
  maxSize?: number;
  allowedTypes?: string[] | null;
  allowedExtensions?: string[] | null;
};

export function validateFile(file: FileLike | null | undefined, options: FileOptions = {}): ValidationResult {
  const opts = {
    maxSize: DEFAULT_UPLOAD_MAX_BYTES,
    allowedTypes: null as string[] | null,
    allowedExtensions: null as string[] | null,
    ...options,
  };
  if (!file) return { valid: true, message: "" };

  if (typeof file.size === "number" && file.size > opts.maxSize) {
    return {
      valid: false,
      message: `File size exceeds maximum allowed size (${opts.maxSize / 1024 / 1024} MB)`,
    };
  }
  if (opts.allowedTypes && file.type && !opts.allowedTypes.includes(file.type)) {
    return { valid: false, message: `File type not allowed. Allowed types: ${opts.allowedTypes.join(", ")}` };
  }
  if (opts.allowedExtensions) {
    const ext = (file.name.split(".").pop() ?? "").toLowerCase();
    if (!opts.allowedExtensions.includes(ext)) {
      return {
        valid: false,
        message: `File extension not allowed. Allowed extensions: ${opts.allowedExtensions.join(", ")}`,
      };
    }
  }
  return { valid: true, message: "" };
}

export function validateScheduledTime(datetime: unknown): ValidationResult {
  if (!datetime || typeof datetime !== "string") return { valid: true, message: "" };
  const trimmed = datetime.trim();
  if (trimmed === "") return { valid: true, message: "" };

  try {
    const date = new Date(trimmed);
    if (isNaN(date.getTime())) return { valid: false, message: "Invalid date/time format" };
    const now = new Date();
    if (date < now) return { valid: false, message: "Scheduled time cannot be in the past" };
    const fiveYearsFromNow = new Date();
    fiveYearsFromNow.setFullYear(fiveYearsFromNow.getFullYear() + 5);
    if (date > fiveYearsFromNow) return { valid: false, message: "Scheduled time is too far in the future" };
    return { valid: true, message: "" };
  } catch {
    return { valid: false, message: "Invalid date/time" };
  }
}

// The task-form payload validated before submit. Note: target_node_name is
// GONE (replaced by the MCP selection picker, which is validated separately by
// the picker component against the server's account catalog).
export type TaskFormValues = {
  prompt?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: string;
  max_cost_usd?: string;
  recurrence?: string;
  scheduled_for?: string;
};

export type TaskFormErrors = Partial<Record<keyof TaskFormValues, string>>;

export function validateTaskForm(values: TaskFormValues): { valid: boolean; errors: TaskFormErrors } {
  const errors: TaskFormErrors = {};

  const prompt = validatePrompt(values.prompt);
  if (!prompt.valid) errors.prompt = prompt.message;

  const model = validateModel(values.model);
  if (!model.valid) errors.model = model.message;

  const fallback = validateModel(values.fallback_model);
  if (!fallback.valid) errors.fallback_model = fallback.message;

  const maxIter = validateMaxIterations(values.max_iterations);
  if (!maxIter.valid) errors.max_iterations = maxIter.message;

  const maxCost = validateMaxCostUSD(values.max_cost_usd);
  if (!maxCost.valid) errors.max_cost_usd = maxCost.message;

  const cron = validateCronExpression(values.recurrence);
  if (!cron.valid) errors.recurrence = cron.message;

  const scheduled = validateScheduledTime(values.scheduled_for);
  if (!scheduled.valid) errors.scheduled_for = scheduled.message;

  return { valid: Object.keys(errors).length === 0, errors };
}
