import { describe, expect, it } from "vitest";
import {
  validateUsername,
  validatePassword,
  validateEmail,
  validateCronExpression,
  validatePrompt,
  validateModel,
  validateMaxIterations,
  validateConcurrencyCap,
  validateFile,
  validateScheduledTime,
  validateTaskForm,
} from "./validation";

// Ported from moc tests/validation.test.js, plus the v2-new concurrency-cap
// validator and the target_node_name removal (the task form no longer carries
// or validates it).

describe("validateUsername", () => {
  it("validates correct username", () => {
    expect(validateUsername("valid_user-1").valid).toBe(true);
  });
  it("rejects empty or non-string", () => {
    expect(validateUsername("").valid).toBe(false);
    expect(validateUsername(null).valid).toBe(false);
  });
  it("rejects short / long / invalid chars", () => {
    expect(validateUsername("ab").valid).toBe(false);
    expect(validateUsername("a".repeat(51)).valid).toBe(false);
    expect(validateUsername("user@name").valid).toBe(false);
    expect(validateUsername("user name").valid).toBe(false);
  });
});

describe("validatePassword", () => {
  it("validates basic password", () => {
    expect(validatePassword("password123").valid).toBe(true);
  });
  it("rejects short password", () => {
    expect(validatePassword("short").valid).toBe(false);
  });
  it("enforces required classes", () => {
    expect(validatePassword("password123", { requireUppercase: true }).valid).toBe(false);
    expect(validatePassword("Password123", { requireUppercase: true }).valid).toBe(true);
    expect(validatePassword("PASSWORD123", { requireLowercase: true }).valid).toBe(false);
    expect(validatePassword("Password", { requireNumber: true }).valid).toBe(false);
    expect(validatePassword("Password123", { requireSpecial: true }).valid).toBe(false);
    expect(validatePassword("Password123!", { requireSpecial: true }).valid).toBe(true);
  });
  it("calculates strength", () => {
    expect(validatePassword("password123").strength).toBe("medium");
    expect(validatePassword("Password123456").strength).toBe("strong");
  });
});

describe("validateEmail", () => {
  it("validates and rejects", () => {
    expect(validateEmail("test@example.com").valid).toBe(true);
    expect(validateEmail("invalid").valid).toBe(false);
    expect(validateEmail("a".repeat(245) + "@example.com").valid).toBe(false);
  });
});

describe("validateCronExpression", () => {
  it("validates 5 and 6 part crons", () => {
    expect(validateCronExpression("0 9 * * 1").valid).toBe(true);
    expect(validateCronExpression("0 9 * * 1 *").valid).toBe(true);
  });
  it("accepts empty", () => {
    expect(validateCronExpression("").valid).toBe(true);
    expect(validateCronExpression(null).valid).toBe(true);
  });
  it("rejects wrong field count / invalid chars", () => {
    expect(validateCronExpression("0 9 * *").valid).toBe(false);
    expect(validateCronExpression("0 9 * * * * *").valid).toBe(false);
    expect(validateCronExpression("0 9 * * A").valid).toBe(false);
  });
});

describe("validatePrompt", () => {
  it("validates and rejects", () => {
    expect(validatePrompt("Run update").valid).toBe(true);
    expect(validatePrompt("").valid).toBe(false);
    expect(validatePrompt("  ").valid).toBe(false);
    expect(validatePrompt("hi").valid).toBe(false);
    expect(validatePrompt("a".repeat(100001)).valid).toBe(false);
  });
});

describe("validateModel", () => {
  it("accepts empty and valid; rejects multiline/oversized", () => {
    expect(validateModel("").valid).toBe(true);
    expect(validateModel("deepseek/deepseek-v3.2").valid).toBe(true);
    expect(validateModel("foo\nbar").valid).toBe(false);
    expect(validateModel("a".repeat(201)).valid).toBe(false);
  });
});

describe("validateMaxIterations", () => {
  it("validates whole numbers in range", () => {
    expect(validateMaxIterations("250").valid).toBe(true);
    expect(validateMaxIterations("0").valid).toBe(false);
    expect(validateMaxIterations("abc").valid).toBe(false);
    expect(validateMaxIterations("").valid).toBe(true);
  });
});

describe("validateConcurrencyCap (v2 — global cap)", () => {
  it("accepts empty (server default)", () => {
    expect(validateConcurrencyCap("").valid).toBe(true);
    expect(validateConcurrencyCap(null).valid).toBe(true);
  });
  it("accepts a sane positive integer", () => {
    expect(validateConcurrencyCap("4").valid).toBe(true);
    expect(validateConcurrencyCap(8).valid).toBe(true);
  });
  it("rejects non-integers and out-of-range", () => {
    expect(validateConcurrencyCap("abc").valid).toBe(false);
    expect(validateConcurrencyCap("0").valid).toBe(false);
    expect(validateConcurrencyCap("65").valid).toBe(false);
  });
});

describe("validateFile", () => {
  it("validates / rejects per size + extension", () => {
    expect(validateFile({ name: "test.txt", size: 1024, type: "text/plain" }).valid).toBe(true);
    expect(validateFile(null).valid).toBe(true);
    expect(validateFile({ name: "test.txt", size: 300 * 1024 * 1024 }).valid).toBe(false);
    expect(validateFile({ name: "test.txt", size: 2 * 1024 * 1024 }, { maxSize: 1024 * 1024 }).valid).toBe(false);
    expect(validateFile({ name: "test.exe" }, { allowedExtensions: ["txt", "md"] }).valid).toBe(false);
    expect(validateFile({ name: "test.txt" }, { allowedExtensions: ["txt", "md"] }).valid).toBe(true);
  });
});

describe("validateScheduledTime", () => {
  it("validates future / rejects past + far future / accepts empty", () => {
    const future = new Date();
    future.setDate(future.getDate() + 1);
    expect(validateScheduledTime(future.toISOString()).valid).toBe(true);
    expect(validateScheduledTime("").valid).toBe(true);
    const past = new Date();
    past.setDate(past.getDate() - 1);
    expect(validateScheduledTime(past.toISOString()).valid).toBe(false);
    expect(validateScheduledTime("not a date").valid).toBe(false);
    const far = new Date();
    far.setFullYear(far.getFullYear() + 6);
    expect(validateScheduledTime(far.toISOString()).valid).toBe(false);
  });
});

describe("validateTaskForm", () => {
  it("validates a minimal valid form", () => {
    const res = validateTaskForm({ prompt: "Do something" });
    expect(res.valid).toBe(true);
    expect(Object.keys(res.errors).length).toBe(0);
  });
  it("detects prompt error", () => {
    const res = validateTaskForm({ prompt: "" });
    expect(res.valid).toBe(false);
    expect(res.errors.prompt).toBeDefined();
  });
  it("validates fallback model", () => {
    const res = validateTaskForm({ prompt: "Run task", fallback_model: "foo\nbar" });
    expect(res.valid).toBe(false);
    expect(res.errors.fallback_model).toBeDefined();
  });
  it("does NOT carry target_node_name (replaced by MCP selection)", () => {
    // The form values type has no target_node_name; a stray key is ignored and
    // never produces an error key.
    const res = validateTaskForm({ prompt: "Run task" } as never);
    expect("target_node_name" in res.errors).toBe(false);
  });
});
