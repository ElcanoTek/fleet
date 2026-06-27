import { describe, expect, it, vi } from "vitest";
import {
  applyContextCompacted,
  applyContextPressure,
  applyModelRequired,
  applyRetryNotice,
  cachedPercent,
  clearRetryNotice,
  conversationTotals,
  historyToMessages,
  humanToolLabel,
  parsePythonStream,
  prettyToolName,
  safePretty,
  shortModelName,
  toolIcon,
  type HistoryEntry,
  type Message,
  type ModelRequiredEventPayload,
  type TurnSummary,
} from "./history";

// Small helper so test fixtures read like a transcript.
const user = (text: string): HistoryEntry => ({
  role: "user",
  type: "text",
  content: { text },
});
const asText = (text: string): HistoryEntry => ({
  role: "assistant",
  type: "text",
  content: { text },
});
const reasoning = (text: string): HistoryEntry => ({
  role: "assistant",
  type: "reasoning",
  content: { text },
});
const toolCall = (id: string, name: string, input = "{}"): HistoryEntry => ({
  role: "assistant",
  type: "tool_call",
  content: { id, name, input },
});
const toolResult = (id: string, name: string, text: string, is_err = false): HistoryEntry => ({
  role: "tool",
  type: "tool_result",
  content: { id, name, text, is_err },
});

describe("historyToMessages", () => {
  it("returns empty for empty input", () => {
    expect(historyToMessages([])).toEqual([]);
  });

  it("opens a fresh user Message per user text row", () => {
    const msgs = historyToMessages([user("first"), user("second")]);
    expect(msgs).toHaveLength(2);
    expect(msgs[0]).toMatchObject({ role: "user", content: "first" });
    expect(msgs[1]).toMatchObject({ role: "user", content: "second" });
  });

  it("coalesces assistant text + reasoning + tool calls into one Message", () => {
    const msgs = historyToMessages([
      user("go"),
      reasoning("thinking"),
      toolCall("c1", "run_python", '{"code":"1+1"}'),
      toolResult("c1", "run_python", `{"stdout":"2"}`),
      asText("two"),
    ]);
    expect(msgs).toHaveLength(2);
    const assistant = msgs[1];
    expect(assistant.role).toBe("assistant");
    expect(assistant.reasoning).toBe("thinking");
    expect(assistant.content).toBe("two");
    expect(assistant.toolCalls).toHaveLength(1);
    expect(assistant.toolCalls?.[0]).toMatchObject({
      id: "c1",
      name: "run_python",
      state: "done",
    });
    expect(assistant.toolCalls?.[0].resultText).toContain("stdout");
  });

  it("parses run_python tool result into stdout/stderr (not the raw JSON envelope)", () => {
    const msgs = historyToMessages([
      user("go"),
      toolCall("c1", "run_python"),
      toolResult(
        "c1",
        "run_python",
        `{"status":"success","stdout":"hi\\n","stderr":"","execution_time_ms":42}`,
      ),
      asText("ok"),
    ]);
    const stream = msgs[1].pythonStreams?.[0];
    expect(stream).toBeDefined();
    expect(stream?.stdout).toBe("hi\n");
    expect(stream?.executionMs).toBe(42);
  });

  it("falls back to raw text when the run_python envelope is unparsable", () => {
    const msgs = historyToMessages([
      user("go"),
      toolCall("c1", "run_python"),
      toolResult("c1", "run_python", "not json"),
    ]);
    expect(msgs[1].pythonStreams?.[0].stdout).toBe("not json");
  });

  it("joins multiple reasoning deltas with a newline", () => {
    const msgs = historyToMessages([
      user("go"),
      reasoning("first"),
      reasoning("second"),
      asText("ok"),
    ]);
    expect(msgs[1].reasoning).toBe("first\nsecond");
  });

  it("marks a tool_result as errored when is_err=true", () => {
    const msgs = historyToMessages([
      user("go"),
      toolCall("c1", "bash"),
      toolResult("c1", "bash", "exit 1", true),
      asText("failed"),
    ]);
    expect(msgs[1].toolCalls?.[0].state).toBe("error");
  });

  it("ignores a tool_result whose id has no matching call (no retroactive creation)", () => {
    const msgs = historyToMessages([
      user("go"),
      toolResult("missing", "bash", "orphan"),
      asText("what"),
    ]);
    expect(msgs[1].toolCalls ?? []).toHaveLength(0);
  });

  it("applies an approval resolution that lands before its own tool_call (out-of-order)", () => {
    // Repro of the send_email "looks like nothing happened" bug: the user
    // clicks Send before the turn that issued the call is persisted, so the
    // appended 202 result row sorts BEFORE the call + APPROVAL_REQUIRED rows.
    const sent = `{"status_code":202,"status":"queued"}`;
    const msgs = historyToMessages([
      user("put it in an email"),
      toolCall("preview", "preview_email"),
      toolResult("preview", "preview_email", "PREVIEW_DISPLAYED", true),
      asText("Staged a preview for you."),
      // Resolution appended out of order (lower id than the call below):
      toolResult("send1", "mcp_sendgrid_send_email", sent),
      user("send to jeanne"),
      toolCall("send1", "mcp_sendgrid_send_email"),
      toolResult("send1", "mcp_sendgrid_send_email", "APPROVAL_REQUIRED: staged", true),
      asText("I have staged the email."),
    ]);
    const sendCall = msgs
      .flatMap((m) => m.toolCalls ?? [])
      .find((t) => t.id === "send1");
    expect(sendCall).toBeDefined();
    // The real 202 outcome wins over the APPROVAL_REQUIRED placeholder.
    expect(sendCall?.state).toBe("done");
    expect(sendCall?.resultText).toContain("202");
  });

  it("applies an in-order approval resolution appended after its call+placeholder", () => {
    // Non-race ordering: resolution row lands after the call in the same
    // (still-open) assistant turn. Must also win over the placeholder.
    const msgs = historyToMessages([
      user("send it"),
      toolCall("send1", "mcp_sendgrid_send_email"),
      toolResult("send1", "mcp_sendgrid_send_email", "APPROVAL_REQUIRED: staged", true),
      asText("Staged."),
      toolResult("send1", "mcp_sendgrid_send_email", `{"status_code":202}`),
    ]);
    const sendCall = msgs.flatMap((m) => m.toolCalls ?? []).find((t) => t.id === "send1");
    expect(sendCall?.state).toBe("done");
    expect(sendCall?.resultText).toContain("202");
  });

  it("starts an assistant Message even if history opens with a tool call (orphan-safe)", () => {
    const msgs = historyToMessages([toolCall("c1", "bash")]);
    expect(msgs).toHaveLength(1);
    expect(msgs[0].role).toBe("assistant");
    expect(msgs[0].toolCalls?.[0].id).toBe("c1");
  });

  it("assigns stable monotonically increasing ids", () => {
    const msgs = historyToMessages([user("a"), asText("b"), user("c"), asText("d")]);
    expect(msgs.map((m) => m.id)).toEqual([1, 2, 3, 4]);
  });

  it("emits a banner-style summary message with kind=summary + meta", () => {
    const summary: HistoryEntry = {
      role: "assistant",
      type: "summary",
      content: {
        text: "Brief: user asked X, we did Y.",
        model: "anthropic/claude-sonnet-4.6",
        prompt_tokens: 12000,
        completion_tokens: 250,
        cost_usd: 0.04,
      },
    };
    const msgs = historyToMessages([
      user("first"),
      asText("first reply"),
      summary,
      user("after summary"),
    ]);
    // user, assistant, summary banner, user — distinct messages
    expect(msgs).toHaveLength(4);
    const banner = msgs[2];
    expect(banner.kind).toBe("summary");
    expect(banner.role).toBe("assistant");
    expect(banner.content).toBe("Brief: user asked X, we did Y.");
    expect(banner.summaryMeta).toEqual({
      model: "anthropic/claude-sonnet-4.6",
      promptTokens: 12000,
      completionTokens: 250,
      costUsd: 0.04,
    });
  });

  it("flushes any in-flight assistant Message when a summary entry arrives", () => {
    const msgs = historyToMessages([
      user("go"),
      asText("partial"),
      reasoning("midway"),
      // summary should NOT collapse into the assistant turn
      {
        role: "assistant",
        type: "summary",
        content: { text: "summary text" },
      },
    ]);
    // user, assistant (text+reasoning coalesced), summary banner
    expect(msgs).toHaveLength(3);
    expect(msgs[1].kind).not.toBe("summary");
    expect(msgs[1].content).toBe("partial");
    expect(msgs[2].kind).toBe("summary");
  });
});

describe("prettyToolName", () => {
  it.each([
    ["mcp_email_search_emails", "email: search_emails"],
    ["mcp_sendgrid_send_email", "sendgrid: send_email"],
    ["mcp_fastio_download", "fastio_download"],
    ["bash", "bash"],
    ["run_python", "run_python"],
  ])("%s → %s", (input, expected) => {
    expect(prettyToolName(input)).toBe(expected);
  });
});

describe("humanToolLabel", () => {
  it.each([
    ["view_file", "Reading file"],
    ["edit_file", "Editing file"],
    ["write_file", "Writing file"],
    ["run_python", "Running Python"],
    ["bash", "Running shell"],
    ["task_tracker", "Updating plan"],
    ["web_fetch", "Fetching page"],
    ["smart_search", "Searching the web"],
    ["mcp_email_send_email", "Sending email"],
    ["mcp_email_search_emails", "Searching inbox"],
    ["mcp_email_archive_thread", "Email archive thread"],
    ["mcp_sendgrid_send", "SendGrid send"],
    // SSP MCPs strip both `mcp_<server>_` and the redundant tool
    // prefix (`pm_`, `ix_`, etc.) so we don't get
    // "Pubmatic pm run standard report" which is triple-redundant.
    ["mcp_pubmatic_pm_run_standard_report", "PubMatic run standard report"],
    ["mcp_indexexchange_ix_list_report_files", "Index Exchange list report files"],
    ["mcp_indexexchange_ix_run_marketplace_draft_report", "Index Exchange run marketplace draft report"],
    ["mcp_magnite_magnite_download_report", "Magnite download report"],
    ["mcp_xandr_xandr_run_curator_report", "Xandr run curator report"],
    ["mcp_medianet_mn_queue_report_data", "Media.net queue report data"],
    ["mcp_fastio_download", "Fastio download"],
    ["unknown_tool", "Unknown tool"],
  ])("%s → %s", (input, expected) => {
    expect(humanToolLabel(input)).toBe(expected);
  });
});

describe("toolIcon", () => {
  it("picks emoji by tool class", () => {
    expect(toolIcon("run_python")).toBe("🐍");
    expect(toolIcon("bash")).toBe("❯");
    expect(toolIcon("task_tracker")).toBe("✓");
    expect(toolIcon("view_file")).toBe("📄");
    expect(toolIcon("write_file")).toBe("📄");
    expect(toolIcon("edit_file")).toBe("📄");
    expect(toolIcon("web_fetch")).toBe("🔎");
    expect(toolIcon("smart_search")).toBe("🔎");
    expect(toolIcon("mcp_email_search_emails")).toBe("📧");
    expect(toolIcon("mcp_sendgrid_send_email")).toBe("📤");
    expect(toolIcon("something_unknown")).toBe("🛠");
  });
});

describe("safePretty", () => {
  it("pretty-prints valid JSON", () => {
    const out = safePretty(`{"a":1,"b":[2,3]}`);
    expect(out).toContain('"a": 1');
    expect(out.split("\n").length).toBeGreaterThan(1);
  });

  it("returns the raw string when input is not valid JSON", () => {
    expect(safePretty("not json")).toBe("not json");
  });

  it("handles empty string", () => {
    expect(safePretty("")).toBe("");
  });

  it("handles parsing failures by returning raw string", () => {
    expect(safePretty("{malformed")).toBe("{malformed");
  });

  it("handles thrown errors during parsing", () => {
    const spy = vi.spyOn(JSON, "parse").mockImplementation(() => {
      throw new Error("mock error");
    });
    expect(safePretty("{}")).toBe("{}");
    spy.mockRestore();
  });
});

describe("cachedPercent", () => {
  // OpenRouter's `prompt_tokens` already contains cached+fresh; the formula
  // must use promptTokens as the denominator alone. These fixtures come from
  // real OpenRouter responses captured while debugging.

  it("returns null when no prompt tokens yet", () => {
    expect(cachedPercent({ promptTokens: 0, cachedTokens: 0 })).toBeNull();
  });

  it("returns 0 when nothing was cached", () => {
    expect(cachedPercent({ promptTokens: 4028, cachedTokens: 0 })).toBe(0);
  });

  it("matches OpenRouter's reported hit rate on a warm cache", () => {
    // gpt-4o-mini, second call with 4028 prompt tokens, 3968 served from cache.
    expect(cachedPercent({ promptTokens: 4028, cachedTokens: 3968 })).toBe(98);
  });

  it("floors a near-total hit to 99 rather than rounding up to 100", () => {
    // claude-sonnet-4.5, second call with 6424 prompt tokens, 6410 from cache.
    // Real ratio is 99.78%; the latest user message can never be cached, so
    // displaying "100% cached" on a warm turn would look like a broken chip.
    expect(cachedPercent({ promptTokens: 6424, cachedTokens: 6410 })).toBe(99);
  });

  it("caps at 100 when cachedTokens exceeds promptTokens (defensive)", () => {
    // Shouldn't happen in practice but we guard against upstream accounting drift.
    expect(cachedPercent({ promptTokens: 100, cachedTokens: 200 })).toBe(100);
  });
});

describe("shortModelName", () => {
  it("strips the OpenRouter provider prefix", () => {
    expect(shortModelName("anthropic/claude-sonnet-4.6")).toBe("claude-sonnet-4.6");
    expect(shortModelName("openai/gpt-5.4")).toBe("gpt-5.4");
  });

  it("passes through slugs without a provider segment", () => {
    expect(shortModelName("claude")).toBe("claude");
  });

  it("returns empty string for missing input", () => {
    expect(shortModelName(undefined)).toBe("");
    expect(shortModelName("")).toBe("");
  });
});

describe("conversationTotals", () => {
  const summary = (partial: Partial<TurnSummary>): TurnSummary => ({
    costUsd: 0,
    promptTokens: 0,
    completionTokens: 0,
    cachedTokens: 0,
    cacheCreationTokens: 0,
    durationMs: 0,
    ...partial,
  });

  it("returns zeroed totals with null cache% when there are no summaries", () => {
    const t = conversationTotals([]);
    expect(t.costUsd).toBe(0);
    expect(t.turns).toBe(0);
    expect(t.cachedPercent).toBeNull();
  });

  it("sums cost and token-weights the cache-hit %", () => {
    // Two turns: one cold (17k fresh), one warm (17k with 15k cached). A
    // naive mean-of-percents would say (0 + 88) / 2 = 44%; token-weighted
    // says 15k / 34k = 44%. These happen to match here — the point is to
    // guard against silently switching back to a mean-of-percents later.
    const t = conversationTotals([
      summary({ costUsd: 0.003, promptTokens: 17000, cachedTokens: 0 }),
      summary({ costUsd: 0.0005, promptTokens: 17000, cachedTokens: 15000 }),
    ]);
    expect(t.turns).toBe(2);
    expect(t.costUsd).toBeCloseTo(0.0035, 6);
    expect(t.cachedPercent).toBe(44);
  });

  it("caps cached at prompt per-turn before summing", () => {
    // One turn with cached > prompt (defensive) — the cap happens per-turn,
    // not on the sum, so the conversation total can't exceed 100.
    const t = conversationTotals([
      summary({ promptTokens: 100, cachedTokens: 500 }),
      summary({ promptTokens: 100, cachedTokens: 0 }),
    ]);
    expect(t.cachedPercent).toBe(50);
  });

  it("returns null cache% when no turn has prompt tokens yet", () => {
    const t = conversationTotals([summary({ costUsd: 0.01, promptTokens: 0 })]);
    expect(t.cachedPercent).toBeNull();
  });
});

// Small helper so each test starts from a known-shape message.
const assistantMessage = (overrides: Partial<Message> = {}): Message => ({
  id: 1,
  role: "assistant",
  content: "",
  state: "thinking",
  ...overrides,
});

describe("applyRetryNotice", () => {
  it("stamps a retry notice with all four fields", () => {
    const out = applyRetryNotice(assistantMessage(), {
      status_code: 429,
      title: "too many requests",
      message: "rate limited",
      delay_ms: 500,
    });
    expect(out.retrying).toEqual({
      statusCode: 429,
      title: "too many requests",
      message: "rate limited",
      delayMs: 500,
    });
  });

  it("defaults missing fields rather than blowing up", () => {
    const out = applyRetryNotice(assistantMessage(), {});
    expect(out.retrying).toEqual({ statusCode: 0, title: "", message: "", delayMs: 0 });
  });

  it("does not change state — a retry is non-terminal", () => {
    const out = applyRetryNotice(assistantMessage({ state: "streaming" }), { status_code: 503 });
    expect(out.state).toBe("streaming");
    expect(out.failed).toBeUndefined();
    expect(out.modelRequired).toBeUndefined();
  });

  it("does not touch content or reasoning buffers", () => {
    const out = applyRetryNotice(
      assistantMessage({ content: "half a reply", reasoning: "partial thought" }),
      { status_code: 429 },
    );
    expect(out.content).toBe("half a reply");
    expect(out.reasoning).toBe("partial thought");
  });
});

describe("clearRetryNotice", () => {
  it("is a no-op on messages without a retry flag", () => {
    const m = assistantMessage();
    expect(clearRetryNotice(m)).toBe(m); // same reference so React can skip re-render
  });

  it("removes the retry flag when present", () => {
    const withRetry = applyRetryNotice(assistantMessage(), { status_code: 429 });
    const cleared = clearRetryNotice(withRetry);
    expect(cleared.retrying).toBeUndefined();
  });
});

describe("applyContextPressure", () => {
  it("stamps the pressure fields from a snake_case payload", () => {
    const out = applyContextPressure(assistantMessage(), {
      used_tokens: 150000,
      window_size: 200000,
      pct: 0.75,
    });
    expect(out.contextPressure).toEqual({ usedTokens: 150000, windowSize: 200000, pct: 0.75 });
  });

  it("defaults missing fields to zero rather than throwing", () => {
    const out = applyContextPressure(assistantMessage(), {});
    expect(out.contextPressure).toEqual({ usedTokens: 0, windowSize: 0, pct: 0 });
  });

  it("is non-terminal — state, content, and reasoning are untouched", () => {
    const out = applyContextPressure(
      assistantMessage({ state: "streaming", content: "hi", reasoning: "thinking" }),
      { pct: 0.8 },
    );
    expect(out.state).toBe("streaming");
    expect(out.content).toBe("hi");
    expect(out.reasoning).toBe("thinking");
    expect(out.failed).toBeUndefined();
  });
});

describe("applyContextCompacted", () => {
  it("stamps the compaction fields and clears any prior pressure warning", () => {
    const warned = applyContextPressure(assistantMessage(), { pct: 0.95 });
    const out = applyContextCompacted(warned, { removed_turns: 12, summary_tokens: 4200 });
    expect(out.contextCompacted).toEqual({ removedTurns: 12, summaryTokens: 4200 });
    expect(out.contextPressure).toBeUndefined();
  });

  it("defaults missing fields to zero", () => {
    const out = applyContextCompacted(assistantMessage(), {});
    expect(out.contextCompacted).toEqual({ removedTurns: 0, summaryTokens: 0 });
  });
});

describe("applyModelRequired", () => {
  it("marks the turn done+failed and stashes the server reason", () => {
    const out = applyModelRequired(assistantMessage({ content: "partial output" }), {
      reason: "retry_exhausted",
      failed_model: "anthropic/claude-sonnet-4.6",
      status_code: 429,
      message: "The selected model is rate-limiting this request. Retrying did not help.",
    });
    expect(out.state).toBe("done");
    expect(out.failed).toBe(true);
    expect(out.content).toBe("partial output");
    expect(out.modelRequired).toEqual({
      reason: "retry_exhausted",
      failedModel: "anthropic/claude-sonnet-4.6",
      statusCode: 429,
      message: "The selected model is rate-limiting this request. Retrying did not help.",
    });
  });

  it("uses the server message as fallback content when nothing streamed", () => {
    const out = applyModelRequired(assistantMessage(), {
      reason: "context_too_large",
      failed_model: "anthropic/claude-haiku-4.5",
      message: "Conversation exceeds the selected model's window.",
    });
    expect(out.content).toBe("Conversation exceeds the selected model's window.");
  });

  it("defaults reason to 'fatal' when the server sends an unknown value", () => {
    const out = applyModelRequired(assistantMessage(), {
      reason: "whatever-new-thing",
      message: "x",
    });
    expect(out.modelRequired?.reason).toBe("fatal");
  });

  it("clears any in-flight retry notice — model_required supersedes retry", () => {
    const withRetry = applyRetryNotice(assistantMessage(), { status_code: 429 });
    const out = applyModelRequired(withRetry, { reason: "retry_exhausted", message: "x" });
    expect(out.retrying).toBeUndefined();
  });

  it("falls back to generic copy when the server omits the message", () => {
    const out = applyModelRequired(assistantMessage(), { reason: "fatal" });
    expect(out.modelRequired?.message).toContain("different model");
    expect(out.content).toContain("different model");
  });

  it("falls back to generic copy when payload.message is an empty string", () => {
    const out = applyModelRequired(assistantMessage(), { reason: "fatal", message: "" });
    expect(out.modelRequired?.message).toContain("different model");
    expect(out.content).toContain("different model");
  });

  it("safely handles wrong types for failed_model and status_code in payload", () => {
    const out = applyModelRequired(assistantMessage(), {
      reason: "retry_exhausted",
      failed_model: 123, // Should be string
      status_code: "429", // Should be number
    } as unknown as ModelRequiredEventPayload);

    expect(out.modelRequired?.failedModel).toBe(""); // Default for wrong type
    expect(out.modelRequired?.statusCode).toBe(0); // Default for wrong type
  });
});

describe("parsePythonStream", () => {
  it("parses valid JSON with all fields", () => {
    const raw = JSON.stringify({
      stdout: "hello",
      stderr: "world",
      error: "err",
      execution_time_ms: 100,
    });
    expect(parsePythonStream(raw)).toEqual({
      stdout: "hello",
      stderr: "world",
      error: "err",
      executionMs: 100,
    });
  });

  it("falls back to output if stdout is missing", () => {
    const raw = JSON.stringify({ output: "fallback" });
    expect(parsePythonStream(raw)).toEqual({
      stdout: "fallback",
    });
  });

  it("falls back to raw text for malformed JSON", () => {
    const raw = "{ invalid json";
    expect(parsePythonStream(raw)).toEqual({ stdout: raw });
  });

  it("handles missing optional fields", () => {
    const raw = JSON.stringify({ stdout: "hi" });
    expect(parsePythonStream(raw)).toEqual({
      stdout: "hi",
    });
  });
});
