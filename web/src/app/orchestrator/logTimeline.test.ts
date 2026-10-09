import { describe, expect, it } from "vitest";
import type { LogMessage } from "@/app/shared/lib/orchestratorApi";
import {
  buildTimeline,
  formatDuration,
  languageForPath,
  parseRuntimeEvent,
  parseToolResult,
  splitFleetNotes,
  stepFailed,
  toolCallSummary,
  toolMeta,
  unwrapDeferredCall,
} from "./logTimeline";

describe("toolMeta", () => {
  it("files built-ins and MCP tools under their family", () => {
    expect(toolMeta("run_python")).toMatchObject({ category: "python", action: "run_python" });
    expect(toolMeta("bash").category).toBe("shell");
    expect(toolMeta("publish_artifact").category).toBe("files");
    expect(toolMeta("confirm_audit").category).toBe("audit");
    expect(toolMeta("mcp_email_search_emails")).toMatchObject({ category: "email", server: "email", action: "search_emails" });
    // A multi-word server splits at its own boundary, not the first underscore.
    expect(toolMeta("mcp_ses_outbound_send_email")).toMatchObject({ category: "send", server: "ses_outbound", action: "send_email" });
    expect(toolMeta("mcp_pages_update_page_data").category).toBe("pages");
    expect(toolMeta("mcp_magnite_magnite_list_deals")).toMatchObject({ category: "mcp", server: "magnite", categoryLabel: "magnite" });
    expect(toolMeta("something_new").category).toBe("other");
  });
});

describe("unwrapDeferredCall", () => {
  it("resolves the tool_call envelope, including the old singleton-array arguments", () => {
    const input = JSON.stringify({ name: "mcp_fast_io_download", arguments: [{ node_id: "n1" }] });
    expect(unwrapDeferredCall("tool_call", input)).toEqual({ name: "mcp_fast_io_download", input: '{"node_id":"n1"}' });
    expect(unwrapDeferredCall("bash", '{"command":"ls"}')).toEqual({ name: "bash", input: '{"command":"ls"}' });
  });
});

describe("toolCallSummary", () => {
  it("leads a python step with its first comment, else its first line", () => {
    expect(toolCallSummary("run_python", { code: "# Re-run reconciliation\nx = 1\nprint(x)" })).toBe("Re-run reconciliation  ·  3 lines");
    expect(toolCallSummary("run_python", { code: "print(1)" })).toBe("print(1)");
  });

  it("summarizes shell, plan, audit and email searches by what matters", () => {
    expect(toolCallSummary("bash", { command: "ls -la\npwd" })).toBe("$ ls -la");
    expect(toolCallSummary("task_tracker", { command: "plan", task_list: [{ status: "done" }, { status: "todo" }] })).toBe("plan · 2 items · 1 done");
    expect(toolCallSummary("confirm_audit", { success: false, user_visible_summary: "No email was sent." })).toBe("abort — No email was sent.");
    expect(
      toolCallSummary("mcp_email_search_emails", {
        subject_contains: "TWCDaily",
        sender_contains: "magnite",
        date_from: "2026-10-08",
        date_to: "2026-10-08",
      }),
    ).toBe("“TWCDaily” · from magnite · 2026-10-08");
    expect(toolCallSummary("view_file", { path: "protocols/self-audit.md" })).toBe("protocols/self-audit.md");
    expect(toolCallSummary("mcp_x_y", { a: 1, b: "two", c: { nested: true } })).toBe("a=1 · b=two");
  });
});

describe("parseToolResult", () => {
  it("unwraps run_python's envelope into stdout, without the duplicated output field", () => {
    const raw = JSON.stringify({ status: "success", output: "42", stdout: "42\n", stderr: "", error: "", execution_time_ms: 11 });
    expect(parseToolResult("run_python", raw, false)).toMatchObject({ kind: "python", ok: true, stdout: "42\n", ms: 11 });
    const failed = JSON.stringify({ status: "error", output: "", stdout: "", stderr: "", error: "KeyError: 'spend'" });
    expect(parseToolResult("run_python", failed, false)).toMatchObject({ kind: "python", ok: false, error: "KeyError: 'spend'" });
  });

  it("reads bash exit codes", () => {
    const raw = JSON.stringify({ command: "false", exit_code: 1, stdout: "", stderr: "boom" });
    expect(parseToolResult("bash", raw, false)).toMatchObject({ kind: "shell", ok: false, exitCode: 1, stderr: "boom" });
  });

  it("parses JSON with a fleet note appended, and flags failed validations", () => {
    const raw = `{"status":"success","match_count":3}\n\n[fleet date-window] runtime_today=2026-10-08`;
    const parsed = parseToolResult("mcp_email_find_latest_report", raw, false);
    expect(parsed).toMatchObject({ kind: "json", ok: true, headline: "3 matches" });
    expect(parsed.notes).toBe("[fleet date-window] runtime_today=2026-10-08");
    expect(parseToolResult("mcp_ses_outbound_validate_email_content", '{"valid":false,"errors":["EL101"]}', false)).toMatchObject({
      ok: false,
      headline: "invalid · 1 error",
    });
  });

  it("treats text as text, and a failed audit or an error bit as a failure", () => {
    expect(parseToolResult("view_file", "---\nname: x\n", false)).toMatchObject({ kind: "text", ok: true });
    expect(parseToolResult("confirm_audit", "Audit Failed Terminally.\nAudit Evidence:", false)).toMatchObject({
      ok: false,
      headline: "Audit Failed Terminally.",
    });
    expect(parseToolResult("x", "json: cannot unmarshal array into Go value of type map[string]interface {}", true)).toMatchObject({
      ok: false,
      text: "Invalid tool arguments: Fleet expected a JSON object but received an array.",
    });
  });
});

describe("splitFleetNotes", () => {
  it("leaves output without a note alone", () => {
    expect(splitFleetNotes('{"a":1}')).toEqual({ body: '{"a":1}', notes: "" });
  });
});

describe("parseRuntimeEvent", () => {
  it("reads the compaction breadcrumb's fields and sentence", () => {
    const ev = parseRuntimeEvent(
      "[context_compacted] trigger=resend_budget used=187766 budget=80000 removed_turns=30 — the oldest half of the history was summarized",
    );
    expect(ev).toMatchObject({ tag: "context_compacted", title: "History compacted", tone: "info" });
    expect(ev?.fields).toContainEqual(["removed_turns", "30"]);
    expect(ev?.detail).toBe("the oldest half of the history was summarized");
  });

  it("numbers checkpoints and marks a fatal abort", () => {
    expect(parseRuntimeEvent("[context_checkpoint] resent prompt 1 tokens reached X (checkpoint 2)")?.title).toBe("Context checkpoint 2");
    expect(parseRuntimeEvent("[fatal] run aborted by its own self-audit: no send")).toMatchObject({
      tone: "danger",
      title: "Run aborted",
      detail: "run aborted by its own self-audit: no send",
    });
  });

  it("does not mistake ordinary text for an event", () => {
    expect(parseRuntimeEvent("Please send the report")).toBeNull();
    expect(parseRuntimeEvent("[link](http://x)")).toBeNull();
  });
});

describe("buildTimeline", () => {
  const messages: LogMessage[] = [
    { id: "u", role: "user", content: "## Task\nDo it", created_at: 100 },
    { id: "a1", role: "assistant", content: "Starting.", created_at: 101, tool_calls: [
      { id: "c1", name: "run_python", arguments: '{"code":"print(1)"}' },
      { id: "c2", name: "bash", arguments: '{"command":"false"}' },
    ] },
    { id: "t1", role: "tool", tool_call_id: "c1", tool_name: "run_python", content: '{"status":"success","stdout":"1\\n"}', created_at: 103 },
    { id: "t2", role: "tool", tool_call_id: "c2", tool_name: "bash", content: '{"exit_code":1,"stdout":"","stderr":"no"}', created_at: 104 },
    { id: "e", role: "user", content: "[context_compacted] trigger=resend_budget removed_turns=4 — summarized", created_at: 105 },
    { id: "s", role: "user", message_type: "compaction_summary", content: "[context compaction] ## Progress", created_at: 105 },
    { id: "orphan", role: "tool", tool_name: "view_file", content: "text", created_at: 106 },
    { id: "a2", role: "assistant", content: "Done.", created_at: 107 },
  ];

  it("pairs each call with its result and recognizes prompt, events and summaries", () => {
    const items = buildTimeline(messages);
    expect(items.map((it) => it.kind)).toEqual(["prompt", "assistant", "tool", "tool", "event", "summary", "tool", "assistant"]);
    const [, first, py, sh] = items;
    expect(first.kind === "assistant" && first.final).toBe(false);
    expect(py.kind === "tool" && py.tool).toMatchObject({ step: 1, name: "run_python", startedAt: 101, endedAt: 103 });
    expect(py.members).toEqual([1, 2]);
    expect(sh.kind === "tool" && stepFailed(sh.tool)).toBe(true);
    const orphan = items[6];
    expect(orphan.kind === "tool" && orphan.tool).toMatchObject({ step: 3, name: "view_file", argsRaw: "" });
    const last = items[7];
    expect(last.kind === "assistant" && last.final).toBe(true);
  });
});

describe("formatting helpers", () => {
  it("formats durations and maps file extensions to grammars", () => {
    expect(formatDuration(0.4)).toBe("<1s");
    expect(formatDuration(7)).toBe("7s");
    expect(formatDuration(2074)).toBe("34m 34s");
    expect(formatDuration(3720)).toBe("1h 2m");
    expect(languageForPath("protocols/margin-calculation.yaml")).toBe("yaml");
    expect(languageForPath("report/twc.html")).toBe("markup");
    expect(languageForPath("notes.md")).toBeUndefined();
  });
});
