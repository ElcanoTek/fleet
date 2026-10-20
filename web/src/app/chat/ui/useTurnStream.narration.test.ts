import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useTurnStream } from "./useTurnStream";
import type { Message } from "./history";
import { closedStream, createTurnStreamHarness } from "./turnStreamTestHarness";

// The stream reducer's half of the default-view cleanup (turnNarration.ts):
// what a turn's events leave in `content` (the reply) and `narration` (the
// working notes behind "Show details"). The recorded producer streams cover
// the happy path (useTurnStream.contract.test.ts); these cover the frames a
// recording does not: an older server that does not separate steps, a
// failed turn, and reasoning blocks.

const CONV = "conv-narration";

function sse(frames: Array<{ event: string; data: unknown }>): string {
  return frames
    .map((f, i) => `id: ${i + 1}\nevent: ${f.event}\ndata: ${JSON.stringify(f.data)}\n\n`)
    .join("");
}

async function replay(frames: Array<{ event: string; data: unknown }>): Promise<Message> {
  const h = createTurnStreamHarness({
    conversationId: CONV,
    overrides: { loadConversation: async () => {} },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/chat") {
        return new Response(closedStream([sse(frames)]), {
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
    await result.current.submitPrompt("go");
  });
  const assistant = (h.store.get(CONV) ?? []).findLast((m) => m.role === "assistant");
  if (!assistant) throw new Error("no assistant message");
  return assistant;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const call = (id: string) => ({ event: "tool.call", data: { id, name: "bash", input: "{}" } });
const result = (id: string) => ({
  event: "tool.result",
  data: { id, name: "bash", text: "ok", is_err: false },
});
const completed = { event: "turn.completed", data: { model: "vendor/one" } };

describe("useTurnStream narration", () => {
  it("separates steps from a server that streams them glued together", async () => {
    const m = await replay([
      { event: "text.delta", data: { text: "Let me check the deal." } },
      call("c1"),
      result("c1"),
      { event: "text.delta", data: { text: "Found it." } },
      call("c2"),
      result("c2"),
      { event: "text.delta", data: { text: "Both checks passed." } },
      { event: "text.replace", data: { text: "Both checks passed." } },
      completed,
    ]);
    expect(m.content).toBe("Both checks passed.");
    expect(m.narration).toBe("Let me check the deal.\n\nFound it.");
    expect(m.finalAnswer).toBe(true);
  });

  it("keeps a failed turn's whole partial transcript as its reply", async () => {
    const m = await replay([
      { event: "text.delta", data: { text: "Let me check the deal." } },
      call("c1"),
      result("c1"),
      { event: "text.delta", data: { text: "Found it, updat" } },
      { event: "turn.error", data: { message: "provider went away" } },
    ]);
    expect(m.failed).toBe(true);
    expect(m.content).toBe("Let me check the deal.\n\nFound it, updat");
    expect(m.narration).toBeUndefined();
  });

  // Stop before the turn.cancelled frame arrives: the client marks the slot
  // done itself, on a path no terminal frame reaches (Codex review, #1817).
  it("keeps the narration when the user stops before any terminal frame", async () => {
    const h = createTurnStreamHarness({
      conversationId: CONV,
      overrides: { loadConversation: async () => {} },
    });
    const body = sse([
      { event: "text.delta", data: { text: "Let me check the deal." } },
      call("c1"),
    ]);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/chat") {
          const stream = new ReadableStream<Uint8Array>({
            start(c) {
              c.enqueue(new TextEncoder().encode(body));
              init?.signal?.addEventListener("abort", () =>
                c.error(new DOMException("Aborted", "AbortError")),
              );
            },
          });
          return new Response(stream, {
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
      const turn = result.current.submitPrompt("go");
      await new Promise((r) => setTimeout(r, 400));
      const slot = (h.store.get(CONV) ?? []).findLast((m) => m.role === "assistant");
      expect(slot?.narration).toBe("Let me check the deal.");
      h.deps.abortControllersRef.current.get(CONV)?.abort();
      await turn;
    });
    const m = (h.store.get(CONV) ?? []).findLast((x) => x.role === "assistant");
    expect(m?.cancelled).toBe(true);
    expect(m?.content).toBe("Let me check the deal.");
    expect(m?.narration).toBeUndefined();
  });

  it("starts each reasoning block on its own paragraph", async () => {
    const m = await replay([
      { event: "reasoning.start", data: { id: "r1", text: "" } },
      { event: "reasoning.delta", data: { id: "r1", text: "**Planning** I need the deal." } },
      { event: "reasoning.end", data: { id: "r1", text: "" } },
      { event: "reasoning.start", data: { id: "r2", text: "" } },
      { event: "reasoning.delta", data: { id: "r2", text: "**Checking** Now its targeting." } },
      { event: "reasoning.start", data: { id: "r3", text: "**Done** All set." } },
      { event: "text.delta", data: { text: "Done." } },
      completed,
    ]);
    expect(m.reasoning).toBe(
      "**Planning** I need the deal.\n\n**Checking** Now its targeting.\n\n**Done** All set.",
    );
  });
});
