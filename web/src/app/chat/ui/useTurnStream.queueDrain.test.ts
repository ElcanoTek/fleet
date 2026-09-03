import { renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  hasPendingQueueWork,
  queueDrainFollowDelaysMs,
  useTurnStream,
  type QueuedInput,
  type TurnStreamDeps,
} from "./useTurnStream";
import type { HistoryEntry, Message } from "./history";

// Following a queued follow-up to the screen (#785 queue, the "my queued
// message never sent" report).
//
// A submission accepted while a turn is running is durable server-side and
// drains as its OWN turn, kicked from the finishing turn's tail call. Nothing
// pushes that to the browser: the finishing turn's event buffer is sealed by
// then, and the drained turn opens a buffer nobody asked to attach to. So the
// client has to go looking when a stream ends — and what it finds decides
// which of three things is honest:
//
//   - the drain started a turn → attach and stream it like any other turn;
//   - the queue emptied without us ever attaching → the turn ran where nobody
//     could see it; Postgres has it, so adopt the canonical transcript;
//   - the row is STILL queued after the backoff → stop. A restart leaves rows
//     queued on purpose (boot recovery never auto-drains), and the honest
//     answer is an accurate chip strip with a send-now button, not a poll that
//     never ends.

const CONV = "conv-1";

type Store = Record<string, Message[]>;
type InflightInfo = { inflight: boolean; turn_id?: string; last_event_id?: number };

const sse = (id: number, event: string, data: unknown) =>
  `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

const closedStream = (frames: string[]) => {
  const encoder = new TextEncoder();
  let i = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (i < frames.length) {
        controller.enqueue(encoder.encode(frames[i++]));
        return;
      }
      controller.close();
    },
  });
};

const queuedRow = (id: string, state = "queued"): QueuedInput => ({
  id,
  client_input_id: `c-${id}`,
  mode: "queued",
  state,
  position: 1,
  message_preview: "keep it clear and concise",
  has_attachments: false,
});

// The transcript as it looks the instant the FIRST turn's stream ended: the
// analysis is on screen, and the queued follow-up is nowhere yet.
const answeredTranscript = (): Message[] => [
  { id: 1, role: "user", content: "run the analysis", state: "done" },
  { id: 2, role: "assistant", content: "Here is the analysis.", state: "done" },
];

const drainedHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the analysis" } },
  { role: "assistant", type: "text", content: { text: "Here is the analysis." } },
  { role: "user", type: "text", content: { text: "keep it clear and concise" } },
  { role: "assistant", type: "text", content: { text: "Rewritten for the client." } },
];

type Harness = {
  deps: TurnStreamDeps;
  store: Store;
  loadConversationCalls: string[];
  queueReads: number;
  inflightProbes: number;
};

const makeHarness = (opts: {
  initial: Message[];
  persisted: HistoryEntry[];
  // Consumed in order; the last entry is reused for any further read.
  queue: QueuedInput[][];
  inflight: InflightInfo[];
  streamBodies?: Array<() => ReadableStream<Uint8Array>>;
}): Harness => {
  const store: Store = { [CONV]: opts.initial };
  const messagesByConvRef = { current: store };
  const loadConversationCalls: string[] = [];
  const streaming = new Set<string>();
  let queueReads = 0;
  let probes = 0;
  let attaches = 0;

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

  const loadConversation = async (convId: string) => {
    loadConversationCalls.push(convId);
    const { historyToMessages } = await import("./history");
    store[convId] = historyToMessages(opts.persisted);
  };

  const nth = <T,>(list: T[], i: number): T => list[Math.min(i, list.length - 1)];

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      const json = (body: unknown) =>
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      if (url.includes("/queue")) {
        const items = nth(opts.queue, queueReads);
        queueReads += 1;
        return json({ items });
      }
      if (url.includes("/inflight")) {
        const info = nth(opts.inflight, probes);
        probes += 1;
        return json(info);
      }
      if (url === "/api/chat") {
        // The stale-busy mirror: we thought the conversation was idle, the
        // server knew a turn was running and queued the submission.
        return new Response(
          JSON.stringify({
            queued: true,
            input: { id: "q1", client_input_id: "c-q1", mode: "queued", state: "queued", position: 1 },
            conversation_id: CONV,
          }),
          { status: 202, headers: { "content-type": "application/json; charset=utf-8" } },
        );
      }
      if (url.includes("/stream")) {
        const body = nth(opts.streamBodies ?? [], attaches);
        attaches += 1;
        return new Response(body(), {
          status: 200,
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (url.includes("/api/conversations/")) {
        return json({ history: opts.persisted });
      }
      return new Response("{}", { status: 404 });
    }),
  );

  const noop = () => {};
  const asyncNoop = async () => {};

  const deps: TurnStreamDeps = {
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
    markConvStreaming: (k: string) => streaming.add(k),
    markConvIdle: (k: string) => streaming.delete(k),
    abortControllersRef: { current: {} },
    attachedConvIdsRef: { current: new Set<string>() },
    lastEventIdByConvRef: { current: {} },
    currentTurnIdByConvRef: { current: {} },
    reattachInFlightRef: { current: new Set<string>() },
    streamPulseRef: { current: {} },
    serverHeartbeatMsRef: { current: 0 },
    supersededStreamsRef: { current: new WeakSet<AbortController>() },
    livenessInFlightRef: { current: new Set<string>() },
    promoteStreamKey: noop,
    streamingConvsRef: { current: new Set<string>() },
    isStreaming: false,
  };

  return {
    deps,
    store,
    loadConversationCalls,
    get queueReads() {
      return queueReads;
    },
    get inflightProbes() {
      return probes;
    },
  };
};

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("hasPendingQueueWork", () => {
  it("queued and running rows are work the client must follow", () => {
    expect(hasPendingQueueWork([queuedRow("a")])).toBe(true);
    expect(hasPendingQueueWork([queuedRow("a", "running")])).toBe(true);
  });

  it("an injected row is not drain work — it rides a turn that is generating", () => {
    expect(hasPendingQueueWork([queuedRow("a", "injected")])).toBe(false);
  });

  it("empty and unknown snapshots are not work", () => {
    expect(hasPendingQueueWork([])).toBe(false);
    expect(hasPendingQueueWork(null)).toBe(false);
    expect(hasPendingQueueWork(undefined)).toBe(false);
  });
});

describe("followQueueDrain", () => {
  it("streams the drained turn instead of leaving it invisible", async () => {
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      // The row is claimed and running when we look; gone once it committed.
      queue: [[queuedRow("q1", "running")], []],
      inflight: [{ inflight: true, turn_id: "t2" }],
      streamBodies: [
        () =>
          closedStream([
            sse(1, "turn.started", { turn_id: "t2", input_id: "q1", queued: true }),
            sse(2, "user.message", { text: "keep it clear and concise" }),
            sse(3, "text.delta", { text: "Rewritten for the client." }),
            sse(4, "turn.completed", { cost_usd: 0.02, duration_ms: 20 }),
          ]),
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.followQueueDrain(CONV);

    const msgs = h.store[CONV];
    // The queued follow-up finally has a user bubble AND an answer.
    expect(msgs.map((m) => m.role)).toEqual(["user", "assistant", "user", "assistant"]);
    expect(msgs[2].content).toBe("keep it clear and concise");
    expect(msgs[3].content).toBe("Rewritten for the client.");
    expect(msgs[3].state).toBe("done");
    // Streamed live — no need to re-read the transcript from Postgres.
    expect(h.loadConversationCalls).toEqual([]);
  });

  it("chains to the next queued row after the first one finishes", async () => {
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      queue: [[queuedRow("q1", "running")], [queuedRow("q2", "running")], []],
      inflight: [{ inflight: true, turn_id: "t2" }, { inflight: true, turn_id: "t3" }],
      streamBodies: [
        () =>
          closedStream([
            sse(1, "turn.started", { turn_id: "t2" }),
            sse(2, "user.message", { text: "first follow-up" }),
            sse(3, "text.delta", { text: "one" }),
            sse(4, "turn.completed", {}),
          ]),
        () =>
          closedStream([
            sse(1, "turn.started", { turn_id: "t3" }),
            sse(2, "user.message", { text: "second follow-up" }),
            sse(3, "text.delta", { text: "two" }),
            sse(4, "turn.completed", {}),
          ]),
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.followQueueDrain(CONV);

    const contents = h.store[CONV].map((m) => m.content);
    expect(contents).toContain("first follow-up");
    expect(contents).toContain("one");
    expect(contents).toContain("second follow-up");
    expect(contents).toContain("two");
    expect(h.loadConversationCalls).toEqual([]);
  });

  it("adopts the transcript when the drained turn finished before we looked", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      // Pending on the first read, gone on the second — and no turn to attach
      // to in between (the drained turn's retain buffer is already evicted).
      queue: [[queuedRow("q1")], []],
      inflight: [{ inflight: false }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const done = result.current.followQueueDrain(CONV);
    await vi.advanceTimersByTimeAsync(queueDrainFollowDelaysMs[0] + 10);
    await done;

    // Postgres held the drained turn all along; the transcript now shows it.
    expect(h.loadConversationCalls).toEqual([CONV]);
    expect(h.store[CONV].map((m) => m.content)).toEqual([
      "run the analysis",
      "Here is the analysis.",
      "keep it clear and concise",
      "Rewritten for the client.",
    ]);
  });

  it("gives up on a row that is not draining, leaving the chip strip accurate", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      // A row the server deliberately will not auto-drain (post-restart).
      queue: [[queuedRow("q1")]],
      inflight: [{ inflight: false }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const done = result.current.followQueueDrain(CONV);
    const total = queueDrainFollowDelaysMs.reduce((a, b) => a + b, 0);
    await vi.advanceTimersByTimeAsync(total + 1000);
    await done;

    // Bounded: one read per attempt, then it stops polling for good.
    expect(h.queueReads).toBe(queueDrainFollowDelaysMs.length + 1);
    // Never invents a transcript for a turn that never ran.
    expect(h.loadConversationCalls).toEqual([]);
    // The chip is still there — and it is TRUE: the input is still queued,
    // and send-now on it forces the drain.
    expect(result.current.queuedInputs[CONV]?.map((i) => i.id)).toEqual(["q1"]);
  });

  it("does nothing for a brand-new chat with no conversation yet", async () => {
    const h = makeHarness({
      initial: [],
      persisted: [],
      queue: [[]],
      inflight: [{ inflight: false }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.followQueueDrain("__pending__:1");

    expect(h.queueReads).toBe(0);
    expect(h.inflightProbes).toBe(0);
  });
});

describe("a direct submission the server queued instead of running", () => {
  it("withdraws the optimistic bubbles so nothing hangs on 'Thinking…'", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      // Accepted and still queued: nothing drains it while we watch.
      queue: [[queuedRow("q1")]],
      inflight: [{ inflight: false }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const submitted = result.current.submitPrompt("keep it clear and concise");
    const total = queueDrainFollowDelaysMs.reduce((a, b) => a + b, 0);
    await vi.advanceTimersByTimeAsync(total + 1000);
    await submitted;

    // The transcript is exactly as it was: the ack said "queued", not "running",
    // so there is no turn to render yet. Before this, the JSON ack was pumped
    // as SSE and left an assistant slot thinking forever.
    expect(h.store[CONV].map((m) => m.content)).toEqual([
      "run the analysis",
      "Here is the analysis.",
    ]);
    expect(h.store[CONV].some((m) => m.state === "thinking" || m.state === "streaming")).toBe(
      false,
    );
    // The message is not lost — it is on the chip strip with a send-now button.
    expect(result.current.queuedInputs[CONV]?.map((i) => i.id)).toEqual(["q1"]);
  });

  it("follows the drain and renders the turn it eventually runs", async () => {
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      // Queued at submit time; running once the drain claims it; then gone.
      queue: [[queuedRow("q1")], [queuedRow("q1", "running")], []],
      inflight: [{ inflight: true, turn_id: "t2" }],
      streamBodies: [
        () =>
          closedStream([
            sse(1, "turn.started", { turn_id: "t2", input_id: "q1", queued: true }),
            sse(2, "user.message", { text: "keep it clear and concise" }),
            sse(3, "text.delta", { text: "Rewritten for the client." }),
            sse(4, "turn.completed", {}),
          ]),
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("keep it clear and concise");
    // followQueueDrain is fire-and-forget from submitPrompt's finally.
    await vi.waitFor(() => expect(h.store[CONV].length).toBe(4));

    const msgs = h.store[CONV];
    // No orphan: the optimistic pair was withdrawn and the drained turn's own
    // replay rendered the exchange.
    expect(msgs.map((m) => m.role)).toEqual(["user", "assistant", "user", "assistant"]);
    expect(msgs[2].content).toBe("keep it clear and concise");
    expect(msgs[3].content).toBe("Rewritten for the client.");
    expect(msgs.some((m) => m.state === "thinking" || m.state === "streaming")).toBe(false);
  });
});
