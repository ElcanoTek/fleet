import { renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useTurnStream, type TurnStreamDeps } from "./useTurnStream";
import type { HistoryEntry, Message } from "./history";

// Recovery from a socket that dies while the device sleeps.
//
// A phone locking mid-turn severs the SSE socket while the server keeps
// generating. Two different things can be true behind the resulting silence,
// and they need opposite responses:
//
//   - the turn FINISHED while we were away. Postgres has the answer; adopt it.
//     Stamping `state: "done"` on the orphaned slot instead — what every
//     finalizer used to do — renders as "The assistant finished without a
//     written reply.", a claim the database flatly contradicts.
//   - the turn is STILL GENERATING. There is nothing to adopt yet; replace the
//     dead socket and resume the live stream from the last applied event id,
//     so tokens land again instead of the user watching an indicator that will
//     never move.
//
// These tests drive both, plus the cases where doing nothing is correct.

const CONV = "conv-1";

type Store = Record<string, Message[]>;
type InflightInfo = { inflight: boolean; turn_id?: string; last_event_id?: number };

const sse = (id: number, event: string, data: unknown) =>
  `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

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

// Frames, then a clean EOF with no terminal event — the other severed-socket
// signature (the turn is still alive; our end just stopped hearing about it).
const truncatedStream = (frames: string[]) => {
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

// The zombie: a socket that delivers nothing and never errors — what an OS
// leaves behind when it suspends the page. It only ends if we abort it, which
// is exactly what the liveness check is expected to do.
const zombieStream = (signal?: AbortSignal, emitAfterMs?: number, frames: string[] = []) => {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      let closed = false;
      if (emitAfterMs !== undefined) {
        // A socket that was merely frozen: it flushes once the page thaws.
        window.setTimeout(() => {
          if (closed) return;
          for (const f of frames) controller.enqueue(encoder.encode(f));
        }, emitAfterMs);
      }
      signal?.addEventListener("abort", () => {
        if (closed) return;
        closed = true;
        controller.error(new DOMException("aborted", "AbortError"));
      });
    },
  });
};

type Harness = {
  deps: TurnStreamDeps;
  store: Store;
  loadConversationCalls: string[];
  streamRequests: Array<{ url: string; lastEventId: string | null }>;
  streaming: Set<string>;
  inflightProbes: number;
  attachCount: () => number;
};

const makeHarness = (opts: {
  initial: Message[];
  persisted: HistoryEntry[];
  // Consumed in order; the last entry is reused for any further attach.
  streamBodies: Array<(signal?: AbortSignal) => ReadableStream<Uint8Array>>;
  // Consumed in order; the last entry is reused for any further probe.
  inflight: InflightInfo[];
  onLoaded?: () => void;
  // Advertised keepalive cadence, in ms. Omit to send no header at all.
  heartbeatMs?: number;
  // Extra conversations the client already has sockets attached to, for the
  // sweep. Keyed by conv id; each gets its own mid-flight transcript.
  extraConvs?: string[];
}): Harness => {
  const store: Store = { [CONV]: opts.initial };
  for (const extra of opts.extraConvs ?? []) {
    store[extra] = midTurnTranscript();
  }
  const messagesByConvRef = { current: store };
  const loadConversationCalls: string[] = [];
  const streamRequests: Array<{ url: string; lastEventId: string | null }> = [];
  const streaming = new Set<string>();
  let attaches = 0;
  let probes = 0;

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
    // loadConversation ends by re-probing for an in-flight turn; onLoaded lets
    // a test stand in for that trailing reattach claiming the conversation.
    opts.onLoaded?.();
  };

  const nth = <T,>(list: T[], i: number): T => list[Math.min(i, list.length - 1)];

  // The server advertises its keepalive cadence on every attached stream
  // (X-Fleet-Heartbeat-Interval-Ms). undefined = the header is absent, which
  // must read as "no promised cadence" rather than as a default.
  const streamHeaders = (): Record<string, string> => {
    const h: Record<string, string> = { "content-type": "text/event-stream" };
    if (opts.heartbeatMs !== undefined) {
      h["X-Fleet-Heartbeat-Interval-Ms"] = String(opts.heartbeatMs);
    }
    return h;
  };

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("/inflight")) {
        const info = nth(opts.inflight, probes);
        probes += 1;
        return new Response(JSON.stringify(info), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
      if (url === "/api/chat") {
        const body = nth(opts.streamBodies, attaches);
        attaches += 1;
        streamRequests.push({ url, lastEventId: null });
        return new Response(body(init?.signal ?? undefined), {
          status: 200,
          headers: streamHeaders(),
        });
      }
      if (url.includes("/stream")) {
        const body = nth(opts.streamBodies, attaches);
        attaches += 1;
        streamRequests.push({
          url,
          lastEventId: new Headers(init?.headers ?? {}).get("Last-Event-ID"),
        });
        return new Response(body(init?.signal ?? undefined), {
          status: 200,
          headers: streamHeaders(),
        });
      }
      if (url.includes("/api/conversations/")) {
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

  // Typed as TurnStreamDeps, NOT cast through `unknown`: an `as unknown as`
  // here silently handed the hook `undefined` for a newly-added ref, and every
  // test in the file failed on `.current`. The annotation makes adding a dep a
  // compile error in this harness instead.
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
    streamRequests,
    streaming,
    get inflightProbes() {
      return probes;
    },
    attachCount: () => attaches,
  };
};

const midTurnTranscript = (): Message[] => [
  { id: 1, role: "user", content: "run the long job", state: "done" },
  { id: 2, role: "assistant", content: "", state: "streaming" },
];

const answeredHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
  { role: "assistant", type: "text", content: { text: "Done — here are the results." } },
];

const unansweredHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
];

const lastOf = (h: Harness) => h.store[CONV][h.store[CONV].length - 1];

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("reattachToConv recovery when the socket dies mid-turn", () => {
  it("adopts the persisted answer instead of an empty 'done' bubble (severed socket)", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => severedStream([sse(1, "turn.started", { turn_id: "t1" })])],
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    expect(h.loadConversationCalls).toEqual([CONV]);
    const last = lastOf(h);
    expect(last.role).toBe("assistant");
    expect(last.content).toBe("Done — here are the results.");
    // The empty-reply notice in ChatTranscript keys off exactly this shape.
    expect(last.state === "done" && !last.content.trim()).toBe(false);
  });

  it("adopts the persisted answer on a truncated stream (clean EOF, no terminal event)", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([sse(1, "turn.started", { turn_id: "t1" })])],
      inflight: [{ inflight: false, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    expect(h.loadConversationCalls).toEqual([CONV]);
    expect(lastOf(h).content).toBe("Done — here are the results.");
  });

  it("marks a genuinely lost turn as failed and retryable, never as an empty reply", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [() => severedStream([sse(1, "turn.started", { turn_id: "t1" })])],
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    expect(h.loadConversationCalls).toEqual([]);
    const last = lastOf(h);
    expect(last.state).toBe("done");
    expect(last.failed).toBe(true);
    expect(last.content).toBe("The connection dropped before the response finished.");
  });

  it("leaves a normally-completed turn alone", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [
        () =>
          truncatedStream([
            sse(1, "turn.started", { turn_id: "t1" }),
            sse(2, "text.delta", { text: "Done — here are the results." }),
            sse(3, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    // Terminal event observed — no DB round trip, no re-render of history.
    expect(h.loadConversationCalls).toEqual([]);
    const last = lastOf(h);
    expect(last.state).toBe("done");
    expect(last.failed).toBeUndefined();
    expect(last.content).toBe("Done — here are the results.");
  });
});

describe("settling a slot that is waiting on the user", () => {
  it("does not stamp 'Turn failed' over a pending approval card", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () =>
          severedStream([
            sse(1, "turn.started", { turn_id: "t1" }),
            sse(2, "tool.approval_required", {
              approval_id: "a1",
              tool: "send_email",
              summary: {},
            }),
          ]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    const last = lastOf(h);
    expect(last.state).toBe("done");
    expect(last.failed).toBeUndefined();
    expect(last.content).toBe("");
    expect(last.approvals?.[0]?.status).toBe("pending");
  });
});

describe("checkStreamLiveness — the turn already finished", () => {
  it("swaps in the persisted answer and releases the conversation", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([])],
      inflight: [{ inflight: false }],
    });
    h.deps.attachedConvIdsRef.current.add(CONV);
    h.streaming.add(CONV);

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.checkStreamLiveness(CONV, { force: true })).resolves.toBe(
      "recovered",
    );
    expect(lastOf(h).content).toBe("Done — here are the results.");
    expect(h.deps.attachedConvIdsRef.current.has(CONV)).toBe(false);
    expect(h.streaming.has(CONV)).toBe(false);
  });

  it("does not abort a stream that claimed the conversation while we reconciled", async () => {
    // The race the identity check exists for: loadConversation ends by
    // re-probing for an in-flight turn, so a NEW stream can own the
    // conversation by the time we get around to retiring the old socket.
    // Aborting that one — while flagging it "superseded", which tells its
    // teardown to keep its hands off — would strand the turn it is reading.
    const replacement = new AbortController();
    let replacementAborted = false;
    replacement.signal.addEventListener("abort", () => {
      replacementAborted = true;
    });

    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([])],
      inflight: [{ inflight: false }],
      onLoaded: () => {
        // A newer stream takes the conversation mid-reconcile.
        h.deps.abortControllersRef.current[CONV] = replacement;
        h.deps.attachedConvIdsRef.current.add(CONV);
        h.streaming.add(CONV);
      },
    });
    const doomed = new AbortController();
    h.deps.abortControllersRef.current[CONV] = doomed;
    h.deps.attachedConvIdsRef.current.add(CONV);
    h.streaming.add(CONV);

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.checkStreamLiveness(CONV, { force: true })).resolves.toBe(
      "recovered",
    );

    expect(replacementAborted).toBe(false);
    expect(h.deps.abortControllersRef.current[CONV]).toBe(replacement);
    // The newer stream owns the streaming flag now; we must not clear it.
    expect(h.streaming.has(CONV)).toBe(true);
  });

  it("does nothing when the local transcript is not mid-turn", async () => {
    const h = makeHarness({
      initial: [
        { id: 1, role: "user", content: "hi", state: "done" },
        { id: 2, role: "assistant", content: "hello", state: "done" },
      ],
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([])],
      inflight: [{ inflight: false }],
    });
    h.deps.attachedConvIdsRef.current.add(CONV);

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.checkStreamLiveness(CONV, { force: true })).resolves.toBe(
      "idle",
    );
    expect(h.loadConversationCalls).toEqual([]);
    expect(h.inflightProbes).toBe(0);
  });

  it("does nothing when no socket is attached", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([])],
      inflight: [{ inflight: false }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.checkStreamLiveness(CONV, { force: true })).resolves.toBe(
      "idle",
    );
    expect(h.inflightProbes).toBe(0);
  });
});

describe("checkStreamLiveness — the turn is still generating", () => {
  it("replaces a dead socket and resumes the live stream where it left off", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      // Nothing persisted yet: the turn is mid-flight, so there is nothing to
      // adopt. The only correct move is to reconnect.
      persisted: unansweredHistory(),
      streamBodies: [
        // 1. the zombie: streams a partial answer, then goes silent forever.
        //    It ends only when we abort it — that is what an OS-severed
        //    socket looks like to a suspended page.
        (signal) =>
          zombieStream(signal, 5, [
            sse(1, "turn.started", { turn_id: "t1" }),
            sse(2, "text.delta", { text: "partial " }),
          ]),
        // 2. the replacement: the server replays from our last applied id.
        () =>
          truncatedStream([
            sse(3, "text.delta", { text: "and the rest" }),
            sse(4, "turn.completed", { cost_usd: 0.02, duration_ms: 20 }),
          ]),
      ],
      inflight: [
        { inflight: true, turn_id: "t1" }, // reattach's own probe
        { inflight: true, turn_id: "t1", last_event_id: 7 }, // liveness: server is ahead of us
        { inflight: true, turn_id: "t1", last_event_id: 7 }, // the replacement's probe
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    // The first attach never settles — that is the whole point of a zombie.
    const zombie = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.deps.attachedConvIdsRef.current.has(CONV)).toBe(true);
    expect(h.attachCount()).toBe(1);
    expect(h.store[CONV][1].content).toBe("partial ");

    // Let the socket go genuinely silent — a fresh stream is never suspected.
    await vi.advanceTimersByTimeAsync(3000);
    const check = result.current.checkStreamLiveness(CONV, { force: true });
    await vi.advanceTimersByTimeAsync(5000);
    await expect(check).resolves.toBe("reconnected");
    await zombie;
    await vi.advanceTimersByTimeAsync(10);

    // A second socket was opened, and it resumed from the last event we
    // actually applied — not from zero, so nothing is replayed twice.
    expect(h.attachCount()).toBe(2);
    expect(h.streamRequests[1].lastEventId).toBe("2");

    // The turn finished on the replacement, into the SAME assistant slot: the
    // partial answer is still there and the rest is appended to it.
    expect(h.store[CONV]).toHaveLength(2);
    const last = lastOf(h);
    expect(last.id).toBe(2);
    expect(last.content).toBe("partial and the rest");
    expect(last.state).toBe("done");

    // The retired stream must not have settled the turn behind the
    // replacement's back. Without the superseded marker its teardown fires a
    // "connection dropped" failure into the transcript and forces the
    // replacement onto a second, duplicate assistant bubble.
    expect(h.store[CONV].some((m) => m.failed)).toBe(false);
    expect(h.store[CONV].some((m) => m.cancelled)).toBe(false);
    expect(h.loadConversationCalls).toEqual([]);
  }, 20000);

  it("leaves a socket alone once it proves itself during the grace window", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        // Frozen, not dead. It flushes at t=5000 — after the silence gate has
        // let the check through (t=4000) and inside the grace window it then
        // sits through (t=4000..6500). That is precisely a page thawing.
        (signal) => zombieStream(signal, 5000, [sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      inflight: [
        { inflight: true, turn_id: "t1" },
        { inflight: true, turn_id: "t1", last_event_id: 7 },
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const live = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(4000);

    const check = result.current.checkStreamLiveness(CONV, { force: true });
    await vi.advanceTimersByTimeAsync(5000);
    await expect(check).resolves.toBe("healthy");

    // No second socket, and the conversation is still attached to the first.
    expect(h.attachCount()).toBe(1);
    expect(h.deps.attachedConvIdsRef.current.has(CONV)).toBe(true);

    // Clean up the still-open stream so the test does not leak it.
    h.deps.abortControllersRef.current[CONV]?.abort();
    await live.catch(() => {});
  }, 20000);

  it("leaves a stalled-but-alive turn alone when the server has not moved past us", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [(signal) => zombieStream(signal)],
      inflight: [
        { inflight: true, turn_id: "t1" },
        // A long tool call: alive, but nothing emitted past what we applied.
        { inflight: true, turn_id: "t1", last_event_id: 0 },
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const live = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(3000);

    const check = result.current.checkStreamLiveness(CONV, { force: true });
    await vi.advanceTimersByTimeAsync(5000);
    await expect(check).resolves.toBe("healthy");
    expect(h.attachCount()).toBe(1);

    h.deps.abortControllersRef.current[CONV]?.abort();
    await live.catch(() => {});
  }, 20000);

  it("spends no probe on a stream that produced bytes recently (the watchdog gate)", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [(signal) => zombieStream(signal, 5, [sse(1, "turn.started", { turn_id: "t1" })])],
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const live = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(50);
    const probesAfterAttach = h.inflightProbes;

    // Not forced: this is a watchdog tick, and the socket just delivered.
    await expect(result.current.checkStreamLiveness(CONV)).resolves.toBe("healthy");
    expect(h.inflightProbes).toBe(probesAfterAttach);

    h.deps.abortControllersRef.current[CONV]?.abort();
    await live.catch(() => {});
  }, 20000);
});

describe("checkStreamLiveness over a live POST /chat stream", () => {
  // The walk-away scenario usually starts here, not on a reattach: the user
  // submits, the socket dies under a locked phone, and the POST's own
  // AbortController is the one that has to be retired. That abort must not be
  // mistaken for the user pressing Stop.
  it("reconnects without the retired POST marking the turn cancelled", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: unansweredHistory(),
      streamBodies: [
        // The POST's stream: a partial answer, then the socket dies.
        (signal) =>
          zombieStream(signal, 5, [
            sse(1, "turn.started", { turn_id: "t1" }),
            sse(2, "text.delta", { text: "partial " }),
          ]),
        // The replacement reattach.
        () =>
          truncatedStream([
            sse(3, "text.delta", { text: "and the rest" }),
            sse(4, "turn.completed", { cost_usd: 0.03, duration_ms: 30 }),
          ]),
      ],
      inflight: [
        { inflight: true, turn_id: "t1", last_event_id: 7 }, // liveness: server is ahead
        { inflight: true, turn_id: "t1", last_event_id: 7 }, // the replacement's probe
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const posted = result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(300);

    // The POST is streaming into its assistant slot.
    expect(h.deps.attachedConvIdsRef.current.has(CONV)).toBe(true);
    const assistant = h.store[CONV][h.store[CONV].length - 1];
    expect(assistant.content).toBe("partial ");

    // …and then the phone locks: the socket goes quiet and stays quiet.
    await vi.advanceTimersByTimeAsync(3000);
    const check = result.current.checkStreamLiveness(CONV, { force: true });
    await vi.advanceTimersByTimeAsync(5000);
    await expect(check).resolves.toBe("reconnected");
    await posted;
    await vi.advanceTimersByTimeAsync(10);

    // Same slot, both halves of the answer, and — the point of the guard —
    // no "Turn stopped" and no "Turn failed" from the retired POST.
    const last = lastOf(h);
    expect(last.id).toBe(assistant.id);
    expect(last.content).toBe("partial and the rest");
    expect(last.state).toBe("done");
    expect(h.store[CONV].some((m) => m.cancelled)).toBe(false);
    expect(h.store[CONV].some((m) => m.failed)).toBe(false);
    expect(h.loadConversationCalls).toEqual([]);
  }, 20000);
});

describe("checkStreamLiveness — silence during a quiet stretch", () => {
  // The gap the advertised keepalive cadence closes. While the agent sits in a
  // long tool call the server emits NO events, so "the server is ahead of us"
  // can never become true — the socket can die and the event-based test will
  // never notice. The keepalive does not care what the turn is doing: an
  // attached stream writes a byte every interval, so missing several in a row
  // is proof on its own.
  const HEARTBEAT = 15000;

  it("declares a socket dead on missed keepalives alone, with the server not ahead", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      heartbeatMs: HEARTBEAT,
      streamBodies: [
        (signal) =>
          zombieStream(signal, 5, [
            sse(1, "turn.started", { turn_id: "t1" }),
            sse(2, "tool.call", { id: "c1", name: "bash", input: "{}" }),
          ]),
        () =>
          truncatedStream([
            sse(3, "text.delta", { text: "tool finished, here is the answer" }),
            sse(4, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [
        { inflight: true, turn_id: "t1" },
        // The turn is alive and mid-tool-call: nothing emitted past what we
        // applied. Only the missed keepalives give the socket away.
        { inflight: true, turn_id: "t1", last_event_id: 2 },
        { inflight: true, turn_id: "t1", last_event_id: 2 },
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const zombie = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.deps.lastEventIdByConvRef.current[CONV]).toBe(2);

    // Four keepalives' worth of silence with nothing to fall behind on.
    await vi.advanceTimersByTimeAsync(4 * HEARTBEAT + 1000);
    const check = result.current.checkStreamLiveness(CONV);
    await vi.advanceTimersByTimeAsync(5000);
    await expect(check).resolves.toBe("reconnected");
    await zombie;
    await vi.advanceTimersByTimeAsync(10);

    expect(h.attachCount()).toBe(2);
    expect(h.streamRequests[1].lastEventId).toBe("2");
    expect(lastOf(h).content).toBe("tool finished, here is the answer");
    expect(h.store[CONV].some((m) => m.failed)).toBe(false);
  }, 30000);

  it("does not declare it dead before the promised keepalives are actually missed", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      heartbeatMs: HEARTBEAT,
      streamBodies: [(signal) => zombieStream(signal, 5, [sse(1, "turn.started", { turn_id: "t1" })])],
      inflight: [
        { inflight: true, turn_id: "t1" },
        { inflight: true, turn_id: "t1", last_event_id: 1 },
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const live = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);

    // Two intervals of quiet: well within what a healthy stream may do.
    await vi.advanceTimersByTimeAsync(2 * HEARTBEAT + 1000);
    const check = result.current.checkStreamLiveness(CONV);
    await vi.advanceTimersByTimeAsync(5000);
    await expect(check).resolves.toBe("healthy");
    expect(h.attachCount()).toBe(1);

    h.deps.abortControllersRef.current[CONV]?.abort();
    await live.catch(() => {});
  }, 30000);

  it("never treats silence as proof when the server reports keepalives disabled", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      // The operator turned keepalives off: there is no cadence to miss, so
      // assuming one would eventually kill every healthy stream.
      heartbeatMs: 0,
      streamBodies: [(signal) => zombieStream(signal, 5, [sse(1, "turn.started", { turn_id: "t1" })])],
      inflight: [
        { inflight: true, turn_id: "t1" },
        { inflight: true, turn_id: "t1", last_event_id: 1 },
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const live = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);

    // Four minutes of silence still is not evidence without a promised
    // cadence. (Kept under the 5-minute read idle timeout, which is a
    // separate backstop and would end the stream on its own.)
    await vi.advanceTimersByTimeAsync(240_000);
    const check = result.current.checkStreamLiveness(CONV);
    await vi.advanceTimersByTimeAsync(5000);
    await expect(check).resolves.toBe("healthy");
    expect(h.attachCount()).toBe(1);

    h.deps.abortControllersRef.current[CONV]?.abort();
    await live.catch(() => {});
  }, 30000);
});

describe("sweepStreamLiveness", () => {
  const OTHER = "conv-2";

  it("recovers a BACKGROUND conversation, not just the one on screen", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([])],
      inflight: [{ inflight: false }],
      extraConvs: [OTHER],
    });
    // Both chats are streaming in parallel; the user is looking at CONV.
    h.deps.attachedConvIdsRef.current.add(CONV);
    h.deps.attachedConvIdsRef.current.add(OTHER);
    h.streaming.add(CONV);
    h.streaming.add(OTHER);
    h.deps.activeConversationIdRef.current = CONV;

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.sweepStreamLiveness({ force: true });

    // The background chat's finished answer landed too — it is not left
    // spinning until the user happens to click into it.
    expect(h.loadConversationCalls.sort()).toEqual([CONV, OTHER]);
    expect(h.deps.attachedConvIdsRef.current.has(OTHER)).toBe(false);
    expect(h.streaming.has(OTHER)).toBe(false);
    expect(lastOf(h).content).toBe("Done — here are the results.");
    expect(h.store[OTHER][h.store[OTHER].length - 1].content).toBe(
      "Done — here are the results.",
    );
  });

  it("does nothing while the tab is hidden", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([])],
      inflight: [{ inflight: false }],
    });
    h.deps.attachedConvIdsRef.current.add(CONV);
    const spy = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.sweepStreamLiveness({ force: true });

    expect(h.inflightProbes).toBe(0);
    expect(h.loadConversationCalls).toEqual([]);
    spy.mockRestore();
  });

  it("keeps going when one conversation's check throws", async () => {
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [() => truncatedStream([])],
      inflight: [{ inflight: false }],
      extraConvs: [OTHER],
    });
    h.deps.attachedConvIdsRef.current.add(CONV);
    h.deps.attachedConvIdsRef.current.add(OTHER);
    // CONV's transcript is corrupt in a way that makes its check throw.
    Object.defineProperty(h.store, CONV, {
      get() {
        throw new Error("boom");
      },
      configurable: true,
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(result.current.sweepStreamLiveness({ force: true })).resolves.toBeUndefined();
    expect(h.loadConversationCalls).toEqual([OTHER]);
  });
});
