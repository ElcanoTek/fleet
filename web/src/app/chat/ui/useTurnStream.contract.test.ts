import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useTurnStream } from "./useTurnStream";
import type { Message } from "./history";
import { closedStream, createTurnStreamHarness } from "./turnStreamTestHarness";

// The chat stream contract, consumer side (docs/TESTING-STRATEGY.md,
// "Contracts").
//
// testdata/contracts/chat-stream/*.sse is what the Go producer REALLY emits
// for a set of scripted turns — recorded from Manager.RunTurn by
// internal/agent TestChatStreamContract, in the exact wire framing the chat
// server writes and /api/chat pipes through untouched. Each recording is
// replayed here as the /api/chat response body into the real hook, and the
// resulting transcript is checked against what the recording itself says
// should be on screen. Nothing below is a hand-written frame: when the
// producer changes the protocol, the recordings change, and this fails until
// the hook keeps up.

// __dirname, not import.meta.url: under the jsdom environment the latter is
// rewritten to an http URL.
const CONTRACT_DIR = path.resolve(__dirname, "../../../../../testdata/contracts/chat-stream");
const CONV = "conv-contract";

type Frame = { id: number; event: string; data: Record<string, unknown> };

// An independent reading of the recording (not the app's own parser, which
// is under test): frames are blank-line separated; ":" lines are comments.
// What the chat server writes ahead of every turn on an attached stream (the
// synthetic fleet.capabilities frame), recorded from the real writer by
// internal/httpapi TestChatStreamWireFraming. A client sees it first, always.
const PREAMBLE = readFileSync(path.join(CONTRACT_DIR, "..", "chat-stream-preamble.sse"), "utf8");

function readRecording(name: string): { raw: string; frames: Frame[] } {
  const raw = PREAMBLE + readFileSync(path.join(CONTRACT_DIR, name), "utf8");
  const frames = raw
    .split("\n\n")
    .map((block) => block.split("\n").filter((l) => l && !l.startsWith(":")))
    .filter((lines) => lines.length > 0)
    .map((lines) => {
      const field = (k: string) =>
        lines.find((l) => l.startsWith(`${k}: `))?.slice(k.length + 2) ?? "";
      return { id: Number(field("id")), event: field("event"), data: JSON.parse(field("data")) };
    });
  return { raw, frames };
}

// What this consumer relies on: every event the hook acts on, and the payload
// fields (with their JSON types) it reads from each — mirroring the
// `payload as {…}` casts in useTurnStream.ts. This is the web side's half of
// the contract, stated independently of the recordings: a field the producer
// renames or retypes fails here even after the recordings are regenerated, and
// an event the producer starts sending fails until it is listed (handled, or
// deliberately ignored with an empty field list and a reason).
type JsonType = "string" | "number" | "boolean";
const CONSUMED: Record<string, Record<string, JsonType>> = {
  // Received first on every stream and deliberately ignored: the hook takes
  // the heartbeat cadence from the X-Fleet-Heartbeat-Interval-Ms header.
  "fleet.capabilities": {},
  "turn.started": {}, // marks the turn live; the hook reads no payload field
  "text.delta": { text: "string" },
  "text.replace": { text: "string" },
  "tool.call": { id: "string", name: "string", input: "string" },
  "tool.result": { id: "string", name: "string", text: "string", is_err: "boolean" },
  "turn.completed": {
    model: "string",
    cost_usd: "number",
    prompt_tokens: "number",
    prompt_tokens_last_step: "number",
    completion_tokens: "number",
    cached_tokens: "number",
    cache_creation_tokens: "number",
    duration_ms: "number",
  },
  "turn.cancelled": {
    model: "string",
    cost_usd: "number",
    prompt_tokens: "number",
    completion_tokens: "number",
    duration_ms: "number",
  },
  "turn.model_required": {
    reason: "string",
    failed_model: "string",
    status_code: "number",
    message: "string",
  },
};

const recordings = readdirSync(CONTRACT_DIR)
  .filter((f) => f.endsWith(".sse"))
  .sort();

// What the transcript must show once the recorded stream has been consumed,
// derived from the recording alone.
function expectedFrom(frames: Frame[]) {
  const deltas = frames.filter((f) => f.event === "text.delta").map((f) => String(f.data.text));
  const replace = frames.filter((f) => f.event === "text.replace").at(-1);
  const results = new Map(
    frames.filter((f) => f.event === "tool.result").map((f) => [String(f.data.id), f.data]),
  );
  const tools = frames
    .filter((f) => f.event === "tool.call")
    .map((f) => {
      const result = results.get(String(f.data.id));
      return {
        id: String(f.data.id),
        name: String(f.data.name),
        input: String(f.data.input),
        state: result ? (result.is_err ? "error" : "done") : "running",
        resultText: result ? String(result.text) : undefined,
      };
    });
  const terminal = frames.findLast((f) => f.event.startsWith("turn.") && f.event !== "turn.started");
  const streamed = replace ? String(replace.data.text) : deltas.join("");
  // A turn that ends before any text (a provider refusal, an error) shows its
  // terminal event's message as the reply instead of an empty bubble.
  const fallback = typeof terminal?.data.message === "string" ? terminal.data.message : "";
  return { text: streamed || fallback, tools, terminal };
}

async function replay(raw: string): Promise<Message[]> {
  const h = createTurnStreamHarness({
    conversationId: CONV,
    // The stream is what is under test: keep a post-turn reconcile from
    // replacing what it rendered with a (here empty) persisted transcript.
    overrides: { loadConversation: async () => {} },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/chat") {
        return new Response(closedStream([raw]), {
          status: 200,
          headers: { "content-type": "text/event-stream", "X-Fleet-Conversation-Id": CONV },
        });
      }
      if (url.includes("/queue")) return Response.json({ items: [] });
      if (url.includes("/inflight")) return Response.json({ inflight: false });
      return Response.json({});
    }),
  );
  const { result } = renderHook(() => useTurnStream(h.deps));
  await act(async () => {
    await result.current.submitPrompt("replay the recorded turn");
  });
  return h.store.get(CONV) ?? [];
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("chat stream contract (recorded from the Go producer)", () => {
  it("has recordings to replay", () => {
    // An empty directory would make every assertion below vacuous.
    expect(recordings.length).toBeGreaterThan(0);
  });

  it.each(recordings)("%s carries only events and fields this consumer understands", (name) => {
    for (const frame of readRecording(name).frames) {
      const fields = CONSUMED[frame.event];
      expect(
        fields,
        `${name}: the producer sends "${frame.event}", which useTurnStream does not declare — handle it, then list it in CONSUMED`,
      ).toBeDefined();
      for (const [field, type] of Object.entries(fields ?? {})) {
        expect(
          typeof frame.data[field],
          `${name}: ${frame.event}.${field} (the hook reads it as a ${type})`,
        ).toBe(type);
      }
    }
  });

  it.each(recordings)("%s renders what the producer streamed", async (name) => {
    const { raw, frames } = readRecording(name);
    const want = expectedFrom(frames);
    const transcript = await replay(raw);

    const assistant = transcript.findLast((m) => m.role === "assistant");
    expect(assistant, "the turn left no assistant message").toBeDefined();
    if (!assistant) return;
    expect(assistant.state).toBe("done");
    expect(assistant.content).toBe(want.text);
    expect(
      (assistant.toolCalls ?? []).map(({ id, name: tool, input, state, resultText }) => ({
        id,
        name: tool,
        input,
        state,
        resultText,
      })),
    ).toEqual(want.tools);

    switch (want.terminal?.event) {
      case "turn.completed":
        expect(assistant.failed ?? false).toBe(false);
        expect(assistant.cancelled ?? false).toBe(false);
        expect(assistant.modelRequired).toBeUndefined();
        expect(assistant.summary?.model).toBe(want.terminal.data.model);
        break;
      case "turn.cancelled":
        expect(assistant.cancelled).toBe(true);
        break;
      case "turn.model_required":
        expect(assistant.modelRequired?.reason).toBe(want.terminal.data.reason);
        break;
      case "turn.error":
        expect(assistant.failed).toBe(true);
        break;
      default:
        throw new Error(
          `${name} ends with ${want.terminal?.event ?? "no terminal event"}; teach this oracle what the hook must show for it`,
        );
    }
  });
});
