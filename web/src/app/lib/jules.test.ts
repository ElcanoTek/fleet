import { describe, expect, it } from "vitest";
import {
  buildJulesPrompt,
  clipTranscript,
  formatHistory,
  getJulesConfig,
  type ExportEnvelope,
} from "./jules";

const sampleEnvelope: ExportEnvelope = {
  conversation: {
    id: "11111111-2222-3333-4444-555555555555",
    title: "Persona keeps ignoring my CSV",
    persona: "victoria",
    model: "openai/gpt-5",
  },
  history: [
    { role: "user", type: "text", content: { text: "Hi, can you summarize this CSV?" } },
    { role: "assistant", type: "reasoning", content: { text: "Let me think about that." } },
    { role: "assistant", type: "text", content: { text: "Sure — please attach the file." } },
    { role: "user", type: "text", content: { text: "I already attached it. Are you blind?" } },
    {
      role: "assistant",
      type: "tool_call",
      content: { id: "1", name: "view_file", input: "{}" },
    },
    {
      role: "tool",
      type: "tool_result",
      content: { text: "rows=5\ncolumns=name,age" },
    },
    { role: "assistant", type: "text", content: { text: "Got it — the CSV has 5 rows." } },
  ],
};

describe("formatHistory", () => {
  it("renders user, assistant text, and tool calls in order", () => {
    const out = formatHistory(sampleEnvelope.history!);
    expect(out).toContain("[USER]\nHi, can you summarize this CSV?");
    expect(out).toContain("[ASSISTANT (reasoning)]");
    expect(out).toContain("[TOOL CALL] view_file");
    expect(out).toContain("[TOOL RESULT]");
    expect(out).toContain("rows=5\ncolumns=name,age");
    expect(out.indexOf("[USER]")).toBeLessThan(out.indexOf("[ASSISTANT (reasoning)]"));
  });

  it("skips empty / unrecognized entries", () => {
    const out = formatHistory([
      { role: "user", type: "text", content: { text: "" } },
      { role: "system", type: "garbage", content: {} },
      { role: "user", type: "text", content: { text: "real" } },
    ]);
    expect(out).toBe("[USER]\nreal");
  });

  it("preserves long tool results in full so the bug context isn't lost", () => {
    const big = "row " + "x".repeat(5000);
    const out = formatHistory([
      { role: "tool", type: "tool_result", content: { id: "call-7", text: big } },
    ]);
    expect(out).toContain("[TOOL RESULT] (id=call-7)");
    expect(out).toContain(big);
    expect(out.length).toBeGreaterThan(big.length);
  });

  it("renders tool-call arguments verbatim when provided", () => {
    const out = formatHistory([
      {
        role: "assistant",
        type: "tool_call",
        content: { id: "abc", name: "run_python", input: { code: "print(1)" } },
      },
    ]);
    expect(out).toContain("[TOOL CALL] run_python (id=abc)");
    expect(out).toContain('"code": "print(1)"');
  });

  it("notes attached images on user messages", () => {
    const out = formatHistory([
      {
        role: "user",
        type: "text",
        content: {
          text: "see attached",
          images: [{ name: "screenshot.png", path: "/uploads/x.png" }],
        },
      },
    ]);
    expect(out).toContain("[USER] (1 attached image: screenshot.png)");
  });
});

describe("clipTranscript", () => {
  it("returns input unchanged when under budget", () => {
    expect(clipTranscript("hello", 100)).toEqual({ body: "hello", truncated: false });
  });

  it("keeps both head and tail and elides the middle when above budget", () => {
    const turns: string[] = [];
    turns.push("[USER]\noriginal goal: export the deals to CSV");
    for (let i = 0; i < 80; i += 1) {
      turns.push(`[ASSISTANT]\n${"intermediate body line ".repeat(20)} #${i}`);
      turns.push(`[USER]\n${"clarifying followup ".repeat(15)} #${i}`);
    }
    turns.push("[USER]\nrecent turn — and now the model is hallucinating column names");
    const text = turns.join("\n\n");

    const { body, truncated } = clipTranscript(text, 2_000);
    expect(truncated).toBe(true);
    // Opening goal preserved.
    expect(body).toContain("[USER]\noriginal goal: export the deals to CSV");
    // Recent (frustrated) turn preserved.
    expect(body).toContain("[USER]\nrecent turn — and now the model is hallucinating column names");
    // Explicit elision marker between the two halves.
    expect(body).toMatch(/characters of the middle of the transcript elided/);
    expect(body.length).toBeLessThanOrEqual(2_500);
  });

  it("falls back to a tail-only clip when the budget is too small to keep both halves", () => {
    const turns: string[] = [];
    for (let i = 0; i < 5; i += 1) turns.push(`[USER]\n${"x".repeat(200)} #${i}`);
    const text = turns.join("\n\n");
    const { body, truncated } = clipTranscript(text, 250);
    expect(truncated).toBe(true);
    expect(body).toContain("(earlier");
    // The tail should contain the most recent turn.
    expect(body).toContain("#4");
    // The first turn ("#0") should NOT be in the body — budget was too
    // small to fit both halves.
    expect(body.includes("#0")).toBe(false);
  });

  it("snaps the elision to turn boundaries so we never split a message", () => {
    const turns: string[] = [];
    turns.push("[USER]\nopening message");
    turns.push("[ASSISTANT]\nlong middle body " + "y".repeat(2_000));
    turns.push("[USER]\nclosing message");
    const text = turns.join("\n\n");

    const { body, truncated } = clipTranscript(text, 200);
    expect(truncated).toBe(true);
    // The kept slices must each begin with a "[USER]" or "[ASSISTANT]"
    // header — never with mid-message content.
    const elisionIdx = body.indexOf("[…");
    if (elisionIdx > 0) {
      const head = body.slice(0, elisionIdx);
      const tail = body.slice(elisionIdx).split("]\n\n")[1] ?? "";
      // Head should end with a complete message — not partial body.
      expect(head.endsWith("opening message\n\n") || head === "" || head.startsWith("[")).toBe(true);
      // Tail should begin with a turn header.
      expect(tail.startsWith("[")).toBe(true);
    }
  });
});

describe("buildJulesPrompt", () => {
  it("packs the metadata, transcript, and ask into a single prompt", () => {
    const out = buildJulesPrompt({
      userEmail: "alice@example.com",
      exportEnvelope: sampleEnvelope,
      appUrl: "https://chat.example.com",
    });
    expect(out.title).toBe("Bug report: Persona keeps ignoring my CSV");
    expect(out.messageCount).toBe(7);
    expect(out.truncated).toBe(false);
    expect(out.prompt).toContain("Reporter: alice@example.com");
    expect(out.prompt).toContain("App URL: https://chat.example.com");
    expect(out.prompt).toContain("Conversation id: 11111111-2222-3333-4444-555555555555");
    expect(out.prompt).toContain("Persona: victoria");
    expect(out.prompt).toContain("dev");
    expect(out.prompt).toContain("── Conversation transcript");
    expect(out.prompt).toContain("── End transcript ──");
    expect(out.prompt).toContain("[USER]\nHi, can you summarize this CSV?");
  });

  it("includes the optional reporter note", () => {
    const out = buildJulesPrompt({
      userEmail: "alice@example.com",
      note: "  this is the third time this happened today  ",
      exportEnvelope: sampleEnvelope,
    });
    expect(out.prompt).toContain("Reporter note:");
    expect(out.prompt).toContain("this is the third time this happened today");
  });

  it("surfaces lockdown, optional MCP servers, and build id in the metadata block", () => {
    const out = buildJulesPrompt({
      userEmail: "alice@example.com",
      buildId: "abc1234",
      exportEnvelope: {
        conversation: {
          ...sampleEnvelope.conversation,
          lockdown: true,
          optional_mcp_servers_enabled: ["fast_io", "tavily"],
          created_at: 1_700_000_000,
        },
        history: sampleEnvelope.history,
      },
    });
    expect(out.prompt).toContain("Lockdown chat: true");
    expect(out.prompt).toContain("Optional MCP servers enabled: fast_io, tavily");
    expect(out.prompt).toContain("App build id: abc1234");
    expect(out.prompt).toContain("Created at: 2023-11-14T22:13:20.000Z");
    expect(out.prompt).toContain("Messages in transcript: 7");
  });

  it("marks truncated=true and keeps both opening + recent turns when the chat is huge", () => {
    const longHistory: ExportEnvelope["history"] = [];
    for (let i = 0; i < 500; i += 1) {
      longHistory!.push({
        role: i % 2 === 0 ? "user" : "assistant",
        type: "text",
        content: { text: `turn-${i} ${"lorem ipsum ".repeat(50)}` },
      });
    }
    const out = buildJulesPrompt({
      userEmail: "alice@example.com",
      exportEnvelope: { ...sampleEnvelope, history: longHistory },
      maxPromptBytes: 8_000,
    });
    expect(out.truncated).toBe(true);
    expect(out.prompt.length).toBeLessThanOrEqual(8_500);
    expect(out.prompt).toContain("── Conversation transcript");
    expect(out.prompt).toContain("── End transcript ──");
    // Opening turn (the goal) survives.
    expect(out.prompt).toContain("turn-0 ");
    // Most recent turn (the frustration) survives.
    expect(out.prompt).toContain("turn-499 ");
    // And the middle is explicitly elided so Jules knows there's a gap.
    expect(out.prompt).toMatch(/characters of the middle of the transcript elided/);
  });

  it("keeps the full transcript intact when it fits inside the default 1MB cap", () => {
    const history: ExportEnvelope["history"] = [];
    for (let i = 0; i < 200; i += 1) {
      history!.push({
        role: i % 2 === 0 ? "user" : "assistant",
        type: "text",
        content: { text: `turn-${i} payload` },
      });
    }
    const out = buildJulesPrompt({
      userEmail: "alice@example.com",
      exportEnvelope: { conversation: { id: "c" }, history },
    });
    expect(out.truncated).toBe(false);
    expect(out.prompt).toContain("turn-0 payload");
    expect(out.prompt).toContain("turn-199 payload");
  });

  it("falls back gracefully when conversation metadata is missing", () => {
    const out = buildJulesPrompt({
      userEmail: "alice@example.com",
      exportEnvelope: { history: sampleEnvelope.history },
    });
    expect(out.title).toBe("Bug report: untitled chat");
    expect(out.prompt).toContain("Conversation id: (unknown)");
  });
});

describe("getJulesConfig", () => {
  const original = process.env;
  it("uses production defaults when env vars are unset", () => {
    process.env = { ...original };
    delete process.env.JULES_API_KEY;
    delete process.env.JULES_API_BASE;
    delete process.env.JULES_SOURCE;
    delete process.env.JULES_BRANCH;
    const config = getJulesConfig();
    expect(config.apiKey).toBeUndefined();
    expect(config.baseUrl).toBe("https://jules.googleapis.com/v1alpha");
    expect(config.source).toBe("sources/github/ElcanoTek/chat");
    expect(config.branch).toBe("dev");
    process.env = original;
  });

  it("respects overrides and strips trailing slashes from the base URL", () => {
    process.env = { ...original };
    process.env.JULES_API_KEY = "k";
    process.env.JULES_API_BASE = "http://127.0.0.1:9999///";
    process.env.JULES_SOURCE = "sources/github/foo/bar";
    process.env.JULES_BRANCH = "main";
    const config = getJulesConfig();
    expect(config.apiKey).toBe("k");
    expect(config.baseUrl).toBe("http://127.0.0.1:9999");
    expect(config.source).toBe("sources/github/foo/bar");
    expect(config.branch).toBe("main");
    process.env = original;
  });
});
