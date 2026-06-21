// Validation helpers for the orchestrator task form. Ported from moc's
// assets/js/validation.js to TypeScript. Pure functions — no DOM, no DOMPurify
// (React escapes by default, so the moc `sanitizeHtml` shim is dropped).

import { isValidEmail } from "./format";

export type ValidationResult = { valid: boolean; message: string };

export function validateUsername(username: unknown): ValidationResult {
  if (!username || typeof username !== "string") {
    return { valid: false, message: "Username is required" };
  }
  const trimmed = username.trim();
  if (trimmed.length < 3) {
    return { valid: false, message: "Username must be at least 3 characters" };
  }
  if (trimmed.length > 50) {
    return { valid: false, message: "Username must be 50 characters or less" };
  }
  if (!/^[a-zA-Z0-9_-]+$/.test(trimmed)) {
    return {
      valid: false,
      message: "Username can only contain letters, numbers, underscore, and hyphen",
    };
  }
  return { valid: true, message: "" };
}

export type PasswordOptions = {
  minLength?: number;
  requireUppercase?: boolean;
  requireLowercase?: boolean;
  requireNumber?: boolean;
  requireSpecial?: boolean;
};

export type PasswordResult = ValidationResult & { strength: string };

export function validatePassword(password: unknown, options: PasswordOptions = {}): PasswordResult {
  const opts = {
    minLength: 8,
    requireUppercase: false,
    requireLowercase: false,
    requireNumber: false,
    requireSpecial: false,
    ...options,
  };

  if (!password || typeof password !== "string") {
    return { valid: false, message: "Password is required", strength: "none" };
  }
  if (password.length < opts.minLength) {
    return {
      valid: false,
      message: `Password must be at least ${opts.minLength} characters`,
      strength: "weak",
    };
  }

  let strength = "weak";
  let checks = 0;

  if (opts.requireUppercase && !/[A-Z]/.test(password)) {
    return { valid: false, message: "Password must contain at least one uppercase letter", strength: "weak" };
  }
  if (/[A-Z]/.test(password)) checks++;

  if (opts.requireLowercase && !/[a-z]/.test(password)) {
    return { valid: false, message: "Password must contain at least one lowercase letter", strength: "weak" };
  }
  if (/[a-z]/.test(password)) checks++;

  if (opts.requireNumber && !/[0-9]/.test(password)) {
    return { valid: false, message: "Password must contain at least one number", strength: "weak" };
  }
  if (/[0-9]/.test(password)) checks++;

  if (opts.requireSpecial && !/[!@#$%^&*(),.?":{}|<>]/.test(password)) {
    return { valid: false, message: "Password must contain at least one special character", strength: "weak" };
  }
  if (/[!@#$%^&*(),.?":{}|<>]/.test(password)) checks++;

  if (password.length >= 12 && checks >= 3) {
    strength = "strong";
  } else if (password.length >= 8 && checks >= 2) {
    strength = "medium";
  }

  return { valid: true, message: "", strength };
}

export function validateEmail(email: unknown): ValidationResult {
  if (!email || typeof email !== "string") {
    return { valid: false, message: "Email is required" };
  }
  const trimmed = email.trim().toLowerCase();
  if (!isValidEmail(trimmed)) {
    return { valid: false, message: "Invalid email format" };
  }
  if (trimmed.length > 254) {
    return { valid: false, message: "Email is too long" };
  }
  return { valid: true, message: "" };
}

export function validateCronExpression(cron: unknown): ValidationResult {
  if (!cron || typeof cron !== "string") {
    return { valid: true, message: "" }; // Optional field
  }
  const trimmed = cron.trim();
  if (trimmed === "") return { valid: true, message: "" };

  const parts = trimmed.split(/\s+/);
  if (parts.length < 5 || parts.length > 6) {
    return {
      valid: false,
      message: "Cron expression must have 5 or 6 fields (minute hour day month weekday [year])",
    };
  }
  const cronRegex = /^[0-9*\-,/]+$/;
  for (const part of parts) {
    if (!cronRegex.test(part)) {
      return {
        valid: false,
        message: "Cron expression contains invalid characters. Use numbers, *, -, /, and ,",
      };
    }
  }
  return { valid: true, message: "" };
}

export function validatePrompt(prompt: unknown): ValidationResult {
  if (!prompt || typeof prompt !== "string") {
    return { valid: false, message: "Prompt is required" };
  }
  const trimmed = prompt.trim();
  if (trimmed.length === 0) return { valid: false, message: "Prompt cannot be empty" };
  if (trimmed.length < 3) return { valid: false, message: "Prompt must be at least 3 characters" };
  if (trimmed.length > 100000) {
    return { valid: false, message: "Prompt is too long (max 100,000 characters)" };
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

// Global concurrency cap (FLEET_MAX_CONCURRENT_AGENTS). Mirrors the iteration
// bound: a positive integer with a sane upper limit. Empty falls back to the
// server default (4), so empty is valid.
export function validateConcurrencyCap(value: unknown): ValidationResult {
  if (value === null || value === undefined) return { valid: true, message: "" };
  const trimmed = String(value).trim();
  if (trimmed === "") return { valid: true, message: "" };
  if (!/^\d+$/.test(trimmed)) {
    return { valid: false, message: "Concurrency cap must be a whole number" };
  }
  const parsed = Number.parseInt(trimmed, 10);
  if (parsed < 1 || parsed > 64) {
    return { valid: false, message: "Concurrency cap must be between 1 and 64" };
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
    maxSize: 250 * 1024 * 1024,
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

export type FilesResult = {
  valid: boolean;
  message: string;
  results: Array<{ file: FileLike } & ValidationResult>;
};

export function validateFiles(
  files: FileLike[],
  options: FileOptions & { maxFiles?: number } = {},
): FilesResult {
  const opts = {
    maxSize: 250 * 1024 * 1024,
    maxFiles: 10,
    allowedTypes: null as string[] | null,
    allowedExtensions: null as string[] | null,
    ...options,
  };
  if (files.length > opts.maxFiles) {
    return { valid: false, message: `Too many files. Maximum ${opts.maxFiles} files allowed.`, results: [] };
  }
  const results = files.map((file) => ({ file, ...validateFile(file, opts) }));
  return { valid: results.every((r) => r.valid), message: "", results };
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

  const cron = validateCronExpression(values.recurrence);
  if (!cron.valid) errors.recurrence = cron.message;

  const scheduled = validateScheduledTime(values.scheduled_for);
  if (!scheduled.valid) errors.scheduled_for = scheduled.message;

  return { valid: Object.keys(errors).length === 0, errors };
}
