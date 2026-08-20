import { renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useTurnStream, type TurnStreamDeps } from "./useTurnStream";
import type { HistoryEntry, Message } from "./history";

// Regression: "I walked away and came back to 'the assistant did not reply'".
//
// A phone locking mid-turn severs the SSE socket while the server keeps
// generating. The reattach pump then ends WITHOUT a terminal turn event.
// Its finally block used to stamp `state: "done"` on the assistant slot in
// place, leaving an empty bubble that renders as "The assistant finished
// without a written reply." — while Postgres held the complete answer the
// whole time, which is why a manual refresh always fixed it.
//
// These tests drive reattachToConv against a stream that dies mid-flight and
// assert the loop reconciles with the persisted transcript instead of
// inventing a terminal state.

const CONV = "conv-1";

type Store = Record<string, Message[]>;

const sse = (id: number, event: string, data: unknown) =>
  `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

// A stream that emits a few frames and then FAILS, the way a severed socket
// surfaces to fetch's reader.
// Pull-based: ReadableStreamDefaultController.error() DISCARDS anything still
// queued, so the frames have to be handed over one pull at a time and the
// error raised only once the reader has drained them.
const severedStream = (frames: string[]) => {
  const encoder = new TextEncoder();
  let i = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (i < frames.length) {
        controller.enqueue(encoder.encode(frames[i++]));
        return;
      }
      controller.error(new TypeError("Load failed"));
    },
  });
};

// A stream that emits frames and then ends cleanly with no terminal event —
// the other severed-socket signature (graceful EOF, turn still alive).
const truncatedStream = (frames: string[]) => {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const f of frames) controller.enqueue(encoder.encode(f));
      controller.close();
    },
  });
};

type Harness = {
  deps: TurnStreamDeps;
  store: Store;
  loadConversationCalls: string[];
};

const makeHarness = (opts: {
  initial: Message[];
  persisted: HistoryEntry[];
  streamBody: () => ReadableStream<Uint8Array>;
  inflight: { inflight: boolean; turn_id?: string };
}): Harness => {
  const store: Store = { [CONV]: opts.initial };
  const messagesByConvRef = { current: store };
  const loadConversationCalls: string[] = [];

  const setConvMessages = (
    convId: string,
    updater: Message[] | ((prev: Message[]) => Message[]),
  ) => {
    const prev = store[convId] ?? [];
    store[convId] = typeof updater === "function" ? updater(prev) : updater;
  };

  const patchAssistantMessage = (
    convId: string,
    assistantId: number,
    updater: (m: Message) => Message,
  ) => {
    store[convId] = (store[convId] ?? []).map((m) =>
      m.id === assistantId ? updater(m) : m,
    );
  };

  // Stands in for the component's loadConversation: swaps the in-memory
  // transcript for the canonical one, exactly as a refresh would.
  const loadConversation = async (convId: string) => {
    loadConversationCalls.push(convId);
    const { historyToMessages } = await import("./history");
    store[convId] = historyToMessages(opts.persisted);
  };

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/inflight")) {
        return new Response(JSON.stringify(opts.inflight), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
      if (url.includes("/stream")) {
        return new Response(opts.streamBody(), {
          status: 200,
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (url.includes(`/api/conversations/${CONV}`)) {
        return new Response(JSON.stringify({ history: opts.persisted }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response("{}", { status: 404 });
    }),
  );

  const noop = () => {};
  const asyncNoop = async () => {};

  const deps = {
    setConvMessages,
    getConvMessages: (convId: string) => store[convId] ?? [],
    renameConvKey: noop,
    patchAssistantMessage,
    startThinkingCrossfade: noop,
    refreshConversations: asyncNoop,
    loadConversation,
    loadMemories: asyncNoop,
    loadRankedModels: asyncNoop,
    loadCatalogModels: asyncNoop,
    nextPendingKey: () => "__pending__:1",
    isPendingKey: (key: string | null) => !!key && key.startsWith("__pending__"),
    setPromptForKey: noop,
    setPendingAttachmentsForKey: noop,
    setAttachmentErrorForKey: noop,
    markConvUploading: noop,
    markConvUploadDone: noop,
    getPendingAttachmentsForKey: () => [],
    promoteComposerKey: noop,
    setMessagesByConv: noop,
    setConversations: noop,
    setActiveConversationId: noop,
    setSelectedPersona: noop,
    setSelectedModel: noop,
    setModelPickerOpen: noop,
    setModelSearchQuery: noop,
    setPendingLockdown: noop,
    setSidebarOpen: noop,
    setSpreadsheetNudgeDismissed: noop,
    activeConversationIdRef: { current: CONV },
    messagesByConvRef,
    pendingApprovalScrollRef: { current: null },
    selectedModel: "test-model",
    selectedPersona: "default",
    mcpServers: [],
    pendingLockdown: false,
    userEmail: "tester@example.com",
    modelError: null,
    markConvStreaming: noop,
    markConvIdle: noop,
    abortControllersRef: { current: {} },
    attachedConvIdsRef: { current: new Set<string>() },
    lastEventIdByConvRef: { current: {} },
    currentTurnIdByConvRef: { current: {} },
    reattachInFlightRef: { current: new Set<string>() },
    promoteStreamKey: noop,
    streamingConvsRef: { current: new Set<string>() },
    isStreaming: false,
  } as unknown as TurnStreamDeps;

  return { deps, store, loadConversationCalls };
};

const midTurnTranscript = (): Message[] => [
  { id: 1, role: "user", content: "run the long job", state: "done" },
  { id: 2, role: "assistant", content: "", state: "streaming" },
];

const answeredHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
  { role: "assistant", type: "text", content: { text: "Done — here are the results." } },
];

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("reattachToConv recovery when the socket dies mid-turn", () => {
  it("adopts the persisted answer instead of an empty 'done' bubble (severed socket)", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBody: () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      inflight: { inflight: true, turn_id: "t1" },
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    expect(h.loadConversationCalls).toEqual([CONV]);
    const last = h.store[CONV][h.store[CONV].length - 1];
    expect(last.role).toBe("assistant");
    expect(last.content).toBe("Done — here are the results.");
    // The empty-reply notice in ChatTranscript keys off exactly this shape.
    expect(last.state === "done" && !last.content.trim()).toBe(false);
  });

  it("adopts the persisted answer on a truncated stream (clean EOF, no terminal event)", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBody: () => truncatedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      inflight: { inflight: false, turn_id: "t1" },
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    expect(h.loadConversationCalls).toEqual([CONV]);
    expect(h.store[CONV][h.store[CONV].length - 1].content).toBe(
      "Done — here are the results.",
    );
  });

  it("marks a genuinely lost turn as failed and retryable, never as an empty reply", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      // Postgres has nothing beyond the prompt: the turn really is gone.
      persisted: [{ role: "user", type: "text", content: { text: "run the long job" } }],
      streamBody: () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      inflight: { inflight: true, turn_id: "t1" },
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    expect(h.loadConversationCalls).toEqual([]);
    const last = h.store[CONV][h.store[CONV].length - 1];
    expect(last.state).toBe("done");
    expect(last.failed).toBe(true);
    expect(last.content).toBe("The connection dropped before the response finished.");
  });

  it("leaves a normally-completed turn alone", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBody: () =>
        truncatedStream([
          sse(1, "turn.started", { turn_id: "t1" }),
          sse(2, "text.delta", { text: "Done — here are the results." }),
          sse(3, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
        ]),
      inflight: { inflight: true, turn_id: "t1" },
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    // Terminal event observed — no DB round trip, no re-render of history.
    expect(h.loadConversationCalls).toEqual([]);
    const last = h.store[CONV][h.store[CONV].length - 1];
    expect(last.state).toBe("done");
    expect(last.failed).toBeUndefined();
    expect(last.content).toBe("Done — here are the results.");
  });
});

describe("reconcileStaleConv (tab return over a zombie socket)", () => {
  it("swaps in the persisted answer once the turn is provably over", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBody: () => truncatedStream([]),
      inflight: { inflight: false },
    });
    h.deps.attachedConvIdsRef.current.add(CONV);

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.reconcileStaleConv(CONV)).resolves.toBe(true);
    expect(h.store[CONV][h.store[CONV].length - 1].content).toBe(
      "Done — here are the results.",
    );
    expect(h.deps.attachedConvIdsRef.current.has(CONV)).toBe(false);
  });

  it("does nothing while the turn is still generating", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBody: () => truncatedStream([]),
      inflight: { inflight: true, turn_id: "t1" },
    });
    h.deps.attachedConvIdsRef.current.add(CONV);

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.reconcileStaleConv(CONV)).resolves.toBe(false);
    expect(h.loadConversationCalls).toEqual([]);
    expect(h.deps.attachedConvIdsRef.current.has(CONV)).toBe(true);
  });

  it("does nothing when the local transcript is not mid-turn", async () => {
    const h = makeHarness({
      initial: [
        { id: 1, role: "user", content: "hi", state: "done" },
        { id: 2, role: "assistant", content: "hello", state: "done" },
      ],
      persisted: answeredHistory(),
      streamBody: () => truncatedStream([]),
      inflight: { inflight: false },
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.reconcileStaleConv(CONV)).resolves.toBe(false);
    expect(h.loadConversationCalls).toEqual([]);
  });
});

describe("settling a slot that is waiting on the user", () => {
  it("does not stamp 'Turn failed' over a pending approval card", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: [{ role: "user", type: "text", content: { text: "run the long job" } }],
      streamBody: () =>
        severedStream([
          sse(1, "turn.started", { turn_id: "t1" }),
          sse(2, "tool.approval_required", {
            approval_id: "a1",
            tool: "send_email",
            summary: {},
          }),
        ]),
      inflight: { inflight: true, turn_id: "t1" },
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    const last = h.store[CONV][h.store[CONV].length - 1];
    expect(last.state).toBe("done");
    expect(last.failed).toBeUndefined();
    expect(last.content).toBe("");
    expect(last.approvals?.[0]?.status).toBe("pending");
  });
});
