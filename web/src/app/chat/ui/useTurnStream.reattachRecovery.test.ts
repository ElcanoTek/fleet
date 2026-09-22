import { StrictMode } from "react";
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

// The component's messagesByConvRef is a Map (a conv id is server-issued,
// so it must not be used as an object property name — CodeQL
// js/remote-property-injection). The harness mirrors that shape.
type Store = Map<string, Message[]>;

// Read one conversation's slot out of the Map-backed store.
const convSlot = (h: Harness, convId: string): Message[] =>
  h.store.get(convId) ?? [];
type InflightInfo = {
  inflight: boolean;
  turn_id?: string;
  last_event_id?: number;
};

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
const zombieStream = (
  signal?: AbortSignal,
  emitAfterMs?: number,
  frames: string[] = [],
) => {
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
  releaseInflight: () => void;
};

const makeHarness = (opts: {
  initial: Message[];
  persisted: HistoryEntry[];
  // Consumed in order; the last entry is reused for any further attach.
  streamBodies: Array<(signal?: AbortSignal) => ReadableStream<Uint8Array>>;
  // Consumed in order; the last entry is reused for any further probe.
  inflight: InflightInfo[];
  // Zero-based indexes of /inflight probes that THROW (TypeError "Failed to
  // fetch") instead of answering — the radio is still off. Such a probe still
  // consumes its slot in the count but not an entry of `inflight`.
  inflightRejectAt?: number[];
  // Same for the persisted-transcript fetch (GET /api/conversations/<id>).
  persistedRejectAt?: number[];
  // Indexes of /inflight probes that stay PENDING until releaseInflight() is
  // called — for driving what a callback does when it resumes after unmount.
  inflightDeferAt?: number[];
  // Zero-based indexes of /inflight probes answered 401 — an expired session
  // or an invalidated epoch, which says nothing about the TURN.
  inflightUnauthorizedAt?: number[];
  // Zero-based indexes of ATTACHES whose /stream request never returns its
  // response headers — a blackholed connect, which the reattach's own connect
  // timer is there to abort. The slot has already been created by then.
  streamNeverConnectsAt?: number[];
  onLoaded?: () => void;
  // Advertised keepalive cadence, in ms. Omit to send no header at all.
  heartbeatMs?: number;
  // Extra conversations the client already has sockets attached to, for the
  // sweep. Keyed by conv id; each gets its own mid-flight transcript.
  extraConvs?: string[];
}): Harness => {
  const store: Store = new Map([[CONV, opts.initial]]);
  for (const extra of opts.extraConvs ?? []) {
    store.set(extra, midTurnTranscript());
  }
  const messagesByConvRef = { current: store };
  const loadConversationCalls: string[] = [];
  const streamRequests: Array<{ url: string; lastEventId: string | null }> = [];
  const streaming = new Set<string>();
  let attaches = 0;
  let probes = 0;
  let persistedFetches = 0;
  let answeredProbes = 0;
  const deferredProbeReleases: Array<() => void> = [];

  const setConvMessages = (
    convId: string,
    updater: Message[] | ((prev: Message[]) => Message[]),
  ) => {
    const prev = store.get(convId) ?? [];
    store.set(convId, typeof updater === "function" ? updater(prev) : updater);
  };

  const patchAssistantMessage = (
    convId: string,
    assistantId: number,
    updater: (m: Message) => Message,
  ) => {
    store.set(
      convId,
      (store.get(convId) ?? []).map((m) => (m.id === assistantId ? updater(m) : m)),
    );
  };

  // Stands in for the component's loadConversation: swaps the in-memory
  // transcript for the canonical one, exactly as a refresh would.
  const loadConversation = async (convId: string) => {
    loadConversationCalls.push(convId);
    const { historyToMessages } = await import("./history");
    store.set(convId, historyToMessages(opts.persisted));
    // loadConversation ends by re-probing for an in-flight turn; onLoaded lets
    // a test stand in for that trailing reattach claiming the conversation.
    opts.onLoaded?.();
  };

  const nth = <T>(list: T[], i: number): T =>
    list[Math.min(i, list.length - 1)];

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
        const idx = probes;
        probes += 1;
        if ((opts.inflightRejectAt ?? []).includes(idx)) {
          throw new TypeError("Failed to fetch");
        }
        if ((opts.inflightUnauthorizedAt ?? []).includes(idx)) {
          return new Response(JSON.stringify({ error: "unauthorized" }), {
            status: 401,
            headers: { "content-type": "application/json" },
          });
        }
        if ((opts.inflightDeferAt ?? []).includes(idx)) {
          await new Promise<void>((resolve) =>
            deferredProbeReleases.push(resolve),
          );
        }
        const info = nth(opts.inflight, answeredProbes);
        answeredProbes += 1;
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
        if ((opts.streamNeverConnectsAt ?? []).includes(attaches)) {
          attaches += 1;
          await new Promise<never>((_, reject) => {
            init?.signal?.addEventListener("abort", () => {
              reject(new DOMException("Aborted", "AbortError"));
            });
          });
        }
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
        const idx = persistedFetches;
        persistedFetches += 1;
        if ((opts.persistedRejectAt ?? []).includes(idx)) {
          throw new TypeError("Failed to fetch");
        }
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
    getConvMessages: (convId: string) => store.get(convId) ?? [],
    renameConvKey: noop,
    patchAssistantMessage,
    startThinkingCrossfade: noop,
    refreshConversations: asyncNoop,
    loadConversation,
    loadMemories: asyncNoop,
    loadRankedModels: asyncNoop,
    loadCatalogModels: asyncNoop,
    nextPendingKey: () => "__pending__:1",
    isPendingKey: (key: string | null) =>
      !!key && key.startsWith("__pending__"),
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
    abortControllersRef: { current: new Map() },
    attachedConvIdsRef: { current: new Set<string>() },
    lastEventIdByConvRef: { current: new Map() },
    currentTurnIdByConvRef: { current: new Map() },
    reattachInFlightRef: { current: new Set<string>() },
    streamPulseRef: { current: new Map() },
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
    releaseInflight: () => {
      for (const release of deferredProbeReleases.splice(0)) release();
    },
  };
};

const midTurnTranscript = (): Message[] => [
  { id: 1, role: "user", content: "run the long job", state: "done" },
  { id: 2, role: "assistant", content: "", state: "streaming" },
];

const answeredHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
  {
    role: "assistant",
    type: "text",
    content: { text: "Done — here are the results." },
  },
];

// A turn that reasoned and then finished without writing prose. Legitimately
// `done` with empty content, which is the shape a replay gap also has.
const reasoningOnlyHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
  {
    role: "assistant",
    type: "reasoning",
    content: { text: "a long silent deliberation" },
  },
];

const unansweredHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
];

const lastOf = (h: Harness) => convSlot(h, CONV)[convSlot(h, CONV).length - 1];

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
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
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
      streamBodies: [
        () => truncatedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
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
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);

    expect(h.loadConversationCalls).toEqual([]);
    const last = lastOf(h);
    expect(last.state).toBe("done");
    expect(last.failed).toBe(true);
    expect(last.content).toBe(
      "The connection dropped before the response finished.",
    );
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

// The phone-unlock signature (#1583): the OS severed the socket while the
// radio was off, and the page wakes — and its probes run — before the network
// is back. "Could not ask the server" must never be reported as "the server
// said the turn is gone": the turn is usually still running, and a refresh a
// moment later shows the full reply. Reproduced on fleetdev with the network
// offline and the proxy restarted mid-turn.
describe("an unreachable server is not a failed turn", () => {
  it("leaves the slot mid-flight and recovers once the probes reach the server", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        // 1. the socket dies after a partial answer (the OS killed it).
        () =>
          severedStream([
            sse(1, "turn.started", { turn_id: "t1" }),
            sse(2, "text.delta", { text: "partial " }),
          ]),
        // 2. once the radio is back, the replacement replays the rest.
        () =>
          truncatedStream([
            sse(3, "text.delta", { text: "and the rest" }),
            sse(4, "turn.completed", { cost_usd: 0.02, duration_ms: 20 }),
          ]),
      ],
      // probe 0: the initial reattach's own probe (answers);
      // probe 1: the first backoff re-probe (+1 s), radio still off (throws);
      // probe 2: the second re-probe (+2 s), radio back (answers);
      // probe 3: the replacement reattach's own probe (answers).
      inflightRejectAt: [1],
      inflight: [{ inflight: true, turn_id: "t1" }],
      // The pump's finally asks Postgres while the radio is still off.
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);

    // The old behaviour stamped `failed: true` with the raw "network error"
    // right here. The slot must instead still look mid-flight, keeping the
    // partial answer, so every recovery path keeps treating it as recoverable.
    let last = lastOf(h);
    expect(last.failed).toBeUndefined();
    expect(last.state).toBe("streaming");
    expect(last.content).toBe("partial ");
    expect(h.loadConversationCalls).toEqual([]);

    // First backoff tick (+1 s): still unreachable — nothing changes, no
    // verdict. Second tick (+2 s): the server answers "still generating" —
    // reattach, and the replacement finishes the same slot.
    await vi.advanceTimersByTimeAsync(1100);
    expect(lastOf(h).state).toBe("streaming");
    expect(lastOf(h).failed).toBeUndefined();
    await vi.advanceTimersByTimeAsync(2100);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.attachCount()).toBe(2);
    expect(h.streamRequests[1].lastEventId).toBe("2");
    last = lastOf(h);
    expect(last.id).toBe(2);
    expect(last.content).toBe("partial and the rest");
    expect(last.state).toBe("done");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);

  it("adopts the persisted answer when the turn finished while the radio was off", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      // probe 0 answers (reattach); probe 1 (+1 s) throws, radio off; probe 2
      // (+2 s) answers "nothing in flight, nothing retained" — the long job
      // finished and its buffer is gone; Postgres has the reply.
      inflightRejectAt: [1],
      inflight: [{ inflight: true, turn_id: "t1" }, { inflight: false }],
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).failed).toBeUndefined();
    expect(lastOf(h).state).toBe("streaming");

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(2100);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.loadConversationCalls).toEqual([CONV]);
    expect(lastOf(h).content).toBe("Done — here are the results.");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);

  it("never stamps failed while the server stays unreachable; the chain is bounded", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      // Every probe after the initial reattach throws — the radio never comes
      // back within the retry window.
      inflightRejectAt: [1, 2, 3, 4, 5, 6, 7, 8],
      inflight: [{ inflight: true, turn_id: "t1" }],
      persistedRejectAt: [0, 1, 2, 3, 4, 5, 6, 7, 8],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    // Well past the whole backoff chain (1+2+4+8+16 s).
    await vi.advanceTimersByTimeAsync(60_000);

    const last = lastOf(h);
    expect(last.failed).toBeUndefined();
    expect(last.state).toBe("streaming");
    // Bounded: one probe per backoff step, then it stops asking.
    expect(h.inflightProbes).toBeLessThanOrEqual(1 + 1 + 5);
    expect(h.loadConversationCalls).toEqual([]);
  }, 20000);
});

// Codex on #1584 found four ways the recovery chain could still lose a turn.
// Each of these drives one of them.
describe("the recovery chain owns the unsettled slot", () => {
  it("no other finalizer stamps failed while a chain is pending", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      // A fresh submission, not a reattach: this is the path where the catch
      // arms the chain and the stream's own finally runs right behind it.
      initial: [],
      // Postgres is REACHABLE and holds no completed answer — the turn is
      // still running. Settling on that read would call a live turn failed,
      // and the chain would then find a terminal slot and give up.
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
        () =>
          truncatedStream([
            sse(2, "text.delta", { text: "the answer" }),
            sse(3, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflightRejectAt: [0], // the catch's probe: the radio is still off
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);
    const slot = lastOf(h);
    expect(slot.role).toBe("assistant");
    expect(slot.failed).toBeUndefined();
    expect(slot.state).toBe("streaming");

    // The chain settles it later, on an answer the server actually gave.
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).content).toBe("the answer");
    expect(lastOf(h).state).toBe("done");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);

  it("recovers a replay-gap slot, which already reads as done", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [
        // The terminal event arrives after a gap, but the answer does not:
        // the slot lands `done` and empty. Re-deriving "the last mid-flight
        // message" would skip it and leave the empty bubble for good.
        () =>
          truncatedStream([
            sse(1, "reconnect", { type: "resumed", missed_events: 4 }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }, { inflight: false }],
      persistedRejectAt: [0], // the gap settle cannot reach Postgres
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).content).toBe("");
    expect(lastOf(h).failed).toBeUndefined();

    // The chain carries the gap shape, so the retry accepts this terminal
    // slot and adopts the answer Postgres had all along.
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.loadConversationCalls).toEqual([CONV]);
    expect(lastOf(h).content).toBe("Done — here are the results.");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);

  it("treats an in-progress reattach as ownership instead of settling", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      inflight: [
        { inflight: true, turn_id: "t1" }, // reattach's own probe
        { inflight: true, turn_id: "t1" }, // first retry tick: turn is alive
        { inflight: false }, // second tick: finished, nothing retained
      ],
      persistedRejectAt: [0], // the settle that arms the chain
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).failed).toBeUndefined();

    // An online/focus handler is inside reattachToConv but has not claimed the
    // conversation yet. reattachToConv answers false for that reason alone;
    // settling on it would stamp failed over the stream being opened.
    h.deps.reattachInFlightRef.current.add(CONV);
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).failed).toBeUndefined();
    expect(lastOf(h).state).toBe("streaming");

    // It releases without claiming; the next tick settles honestly.
    h.deps.reattachInFlightRef.current.delete(CONV);
    await vi.advanceTimersByTimeAsync(2100);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).failed).toBe(true);
  }, 20000);

  it("clears pending recovery timers when the hook unmounts", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }],
      persistedRejectAt: [0], // the settle that arms the chain
    });

    const { result, unmount } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    const probesAtUnmount = h.inflightProbes;

    // A timer outliving /chat could open an SSE stream the unmounted parent
    // can no longer abort.
    unmount();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.inflightProbes).toBe(probesAtUnmount);
    expect(h.attachCount()).toBe(1);
  }, 20000);
});

// Codex round 2 on #1584: ownership was a PENDING TIMER, which is absent
// exactly when it matters — during the tick's awaits, and after the backoff.
describe("recovery ownership spans the whole chain", () => {
  it("keeps the conversation busy until the outcome is known", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }, { inflight: false }],
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    // The turn may still be running on the server: Stop must stay offered and
    // a follow-up must queue rather than race, so the conversation stays busy.
    expect(h.streaming.has(CONV)).toBe(true);

    // The chain settles it (nothing in flight, nothing persisted) and only
    // then is the conversation free.
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).failed).toBe(true);
    expect(h.streaming.has(CONV)).toBe(false);
  }, 20000);

  it("keeps probing at a steady beat after the backoff is spent", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      // Every probe after the reattach's own throws: a long outage.
      inflightRejectAt: Array.from({ length: 40 }, (_, i) => i + 1),
      inflight: [{ inflight: true, turn_id: "t1" }],
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(31_100); // the whole backoff
    const afterBackoff = h.inflightProbes;
    expect(afterBackoff).toBeGreaterThan(4);

    // The old chain stopped here and nothing else would ever look again: the
    // conversation is not in attachedConvIdsRef, so the watchdog skips it.
    await vi.advanceTimersByTimeAsync(90_000);
    expect(h.inflightProbes).toBeGreaterThan(afterBackoff);
    expect(lastOf(h).failed).toBeUndefined();
    expect(lastOf(h).state).toBe("streaming");
  }, 20000);

  it("does not settle when a live turn simply could not be reattached", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      // probe 0: reattach's own. probe 1: the retry tick — the turn IS live.
      // probe 2: the reattach it triggers, which loses the same flap.
      inflightRejectAt: [2],
      inflight: [{ inflight: true, turn_id: "t1" }],
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // A failed reattach is not evidence the turn is gone — the probe had just
    // said it was alive. Settling on it would stamp a live turn failed.
    expect(lastOf(h).failed).toBeUndefined();
    expect(lastOf(h).state).toBe("streaming");
    expect(h.streaming.has(CONV)).toBe(true);
  }, 20000);

  it("a callback already past its timer does nothing after unmount", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
        () => truncatedStream([sse(2, "text.delta", { text: "late" })]),
      ],
      inflightDeferAt: [1], // the retry tick's probe hangs
      inflight: [{ inflight: true, turn_id: "t1" }],
      persistedRejectAt: [0],
    });

    const { result, unmount } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);

    // Fire the tick: its probe is now in flight, so no timer id can reach it.
    await vi.advanceTimersByTimeAsync(1100);
    const attachesBefore = h.attachCount();
    unmount();
    // The probe resolves after the parent's cleanup has run. Opening a stream
    // now would leave a socket nothing can abort.
    h.releaseInflight();
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.attachCount()).toBe(attachesBefore);
  }, 20000);
});

// Codex round 3 on #1584: ownership suppresses the busy flag, so the chain
// owes the conversation an idle; and a chain recovers ONE turn, which the
// server may have moved past by the time a tick fires.
describe("the recovery chain hands the conversation back", () => {
  it("frees the conversation once its recovered stream completes", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
        () =>
          truncatedStream([
            sse(2, "text.delta", { text: "the answer" }),
            sse(3, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }],
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.streaming.has(CONV)).toBe(true);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // Both finalizers skip markConvIdle while the chain owns the slot, so if
    // the chain does not hand it back the composer keeps offering Stop for a
    // finished turn and routes follow-ups as if one were running.
    expect(lastOf(h).content).toBe("the answer");
    expect(lastOf(h).state).toBe("done");
    expect(h.streaming.has(CONV)).toBe(false);
  }, 20000);

  it("adopts its own turn's answer when the server has moved to a new turn", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      // Our turn finished and its answer is in Postgres; a queued input then
      // started the NEXT turn, which /inflight reports.
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
        // The successor's own stream, which the chase attaches and runs to a
        // terminal event.
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "the successor's answer" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [
        { inflight: true, turn_id: "t1" }, // reattach's own probe: our turn
        { inflight: true, turn_id: "t2" }, // the retry tick: a different turn
      ],
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    const attachesBefore = h.attachCount();

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // OUR turn is adopted from the canonical transcript rather than having
    // the successor's replay poured into its bubble. (The successor's own
    // stream may adopt again when it ends; what matters is that an adoption
    // happened and no replay was appended to our slot.)
    expect(h.loadConversationCalls[0]).toBe(CONV);
    expect(
      convSlot(h, CONV).some((m) => m.content === "Done — here are the results."),
    ).toBe(true);
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
    // The successor is then followed, because a turn the server is running
    // with no stream on screen is the other half of this bug: it would show
    // nothing until a reload — and its answer lands in its OWN slot, below
    // ours rather than inside it.
    expect(h.attachCount()).toBe(attachesBefore + 1);
    expect(lastOf(h).content).toBe("the successor's answer");
    expect(lastOf(h).state).toBe("done");
  }, 20000);

  it("still recovers after React's Strict Mode setup-cleanup-setup cycle", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }, { inflight: false }],
      persistedRejectAt: [0],
    });

    // The simulated cleanup used to set the unmount flag for good, which made
    // every later chain refuse to arm: recovery silently dead in development.
    const { result } = renderHook(() => useTurnStream(h.deps), {
      wrapper: StrictMode,
    });
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.loadConversationCalls).toEqual([CONV]);
    expect(lastOf(h).content).toBe("Done — here are the results.");
  }, 20000);
});

// A chain must recover the turn it was armed for. Before this, a fresh
// submission left the PREVIOUS turn's id in place until turn.started arrived,
// so a socket that died in that window armed the chain with the wrong
// identity — and the chain then treated the genuinely live turn as a newer
// one, reconciled an unpersisted answer and failed a running slot (#1584).
describe("a fresh turn does not inherit the previous turn's identity", () => {
  it("recovers the live turn instead of mistaking it for a newer one", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      // Nothing persisted: the turn is still running, so a premature
      // reconcile has nothing to adopt and would stamp the slot failed.
      persisted: unansweredHistory(),
      streamBodies: [
        // Dies before turn.started: no identity was ever learned for it.
        () => severedStream([]),
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "the answer" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflightRejectAt: [0], // the catch's probe: radio off, chain armed
      inflight: [{ inflight: true, turn_id: "t-live" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    // Seed the ref as a previous turn on this conversation would have.
    h.deps.currentTurnIdByConvRef.current.set(CONV, "t-previous");
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).failed).toBeUndefined();

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // With the stale id the chain would have called t-live "a different turn",
    // reconciled nothing, and failed the slot.
    expect(h.loadConversationCalls).toEqual([]);
    expect(lastOf(h).content).toBe("the answer");
    expect(lastOf(h).state).toBe("done");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);
});

// Codex round 4 on #1584: the identity checks had holes of their own.
describe("an unidentified turn is resolved by the transcript, not by guessing", () => {
  it("adopts and follows the successor when our turn is already answered", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      // Our slot IS answered in Postgres: our turn finished while we were
      // away, so the live turn /inflight reports must be a successor.
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([]), // died before turn.started
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "the successor's answer" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflightRejectAt: [0], // the catch's probe: arms the chain, no identity
      inflight: [{ inflight: true, turn_id: "t-successor" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);
    const attachesBefore = h.attachCount();

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // With no ids to compare, the transcript decides: our answer is adopted
    // instead of the successor's replay being appended to our bubble...
    expect(h.loadConversationCalls[0]).toBe(CONV);
    expect(
      convSlot(h, CONV).some((m) => m.content === "Done — here are the results."),
    ).toBe(true);
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
    // ...and the successor is still followed, into its own slot.
    expect(h.attachCount()).toBe(attachesBefore + 1);
    expect(lastOf(h).content).toBe("the successor's answer");
  }, 20000);

  it("attaches when the transcript shows our turn is still unanswered", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      // Nothing answered: the live turn is almost certainly still ours.
      persisted: unansweredHistory(),
      streamBodies: [
        () => severedStream([]),
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "ours after all" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflightRejectAt: [0],
      inflight: [{ inflight: true, turn_id: "t-live" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).content).toBe("ours after all");
    expect(lastOf(h).state).toBe("done");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);
});

// A submission the server never took started no turn, so there is nothing to
// recover — and a previous turn still inside its retain window must not be
// mistaken for it (#1584).
describe("recovery arms only for a submission the server accepted", () => {
  it("does not attach to a retained earlier turn when the POST never lands", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: unansweredHistory(),
      // The POST itself throws: no response, no turn.
      streamBodies: [() => severedStream([])],
      // If the gate were missing, this retained earlier turn would look like
      // ours and its replay would land in this submission's slot.
      inflight: [{ inflight: false, turn_id: "t-earlier-retained" }],
    });
    const postFails = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input) === "/api/chat")
          throw new TypeError("Failed to fetch");
        return (globalThis as { __origFetch?: typeof fetch }).__origFetch!(
          input,
          init,
        );
      },
    );
    const orig = globalThis.fetch;
    (globalThis as { __origFetch?: typeof fetch }).__origFetch = orig;
    vi.stubGlobal("fetch", postFails);

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(2000);

    // No attach to the merely-RETAINED turn, and the slot is settled honestly
    // rather than left waiting on a turn that never existed.
    expect(h.attachCount()).toBe(0);
    const last = lastOf(h);
    expect(last.role).toBe("assistant");
    expect(last.state).toBe("done");
    expect(last.failed).toBe(true);
  }, 20000);
});

// A turn the server registered and started, whose POST response headers never
// reached us, is still OUR turn: /inflight reports it live, and refusing to
// reattach there would fail a running turn (#1584).
describe("a live turn is trusted even when the POST response was lost", () => {
  it("reattaches to an inflight turn although the POST threw", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: unansweredHistory(),
      // The POST never reaches the harness (stubbed to throw), so the first
      // stream the harness serves is the reattach's.
      streamBodies: [
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "it was ours" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      // The probe reports a LIVE turn, not a retained one.
      inflight: [{ inflight: true, turn_id: "t-live" }],
    });
    const orig = globalThis.fetch;
    (globalThis as { __origFetch?: typeof fetch }).__origFetch = orig;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input) === "/api/chat")
          throw new TypeError("Failed to fetch");
        return (globalThis as { __origFetch?: typeof fetch }).__origFetch!(
          input,
          init,
        );
      }),
    );

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(50);

    expect(h.attachCount()).toBeGreaterThan(0);
    expect(lastOf(h).content).toBe("it was ours");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);
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
    await expect(
      result.current.checkStreamLiveness(CONV, { force: true }),
    ).resolves.toBe("recovered");
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
        h.deps.abortControllersRef.current.set(CONV, replacement);
        h.deps.attachedConvIdsRef.current.add(CONV);
        h.streaming.add(CONV);
      },
    });
    const doomed = new AbortController();
    h.deps.abortControllersRef.current.set(CONV, doomed);
    h.deps.attachedConvIdsRef.current.add(CONV);
    h.streaming.add(CONV);

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(
      result.current.checkStreamLiveness(CONV, { force: true }),
    ).resolves.toBe("recovered");

    expect(replacementAborted).toBe(false);
    expect(h.deps.abortControllersRef.current.get(CONV)).toBe(replacement);
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
    await expect(
      result.current.checkStreamLiveness(CONV, { force: true }),
    ).resolves.toBe("idle");
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
    await expect(
      result.current.checkStreamLiveness(CONV, { force: true }),
    ).resolves.toBe("idle");
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
    expect(convSlot(h, CONV)[1].content).toBe("partial ");

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
    expect(convSlot(h, CONV)).toHaveLength(2);
    const last = lastOf(h);
    expect(last.id).toBe(2);
    expect(last.content).toBe("partial and the rest");
    expect(last.state).toBe("done");

    // The retired stream must not have settled the turn behind the
    // replacement's back. Without the superseded marker its teardown fires a
    // "connection dropped" failure into the transcript and forces the
    // replacement onto a second, duplicate assistant bubble.
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
    expect(convSlot(h, CONV).some((m) => m.cancelled)).toBe(false);
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
        (signal) =>
          zombieStream(signal, 5000, [
            sse(1, "turn.started", { turn_id: "t1" }),
          ]),
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
    h.deps.abortControllersRef.current.get(CONV)?.abort();
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

    h.deps.abortControllersRef.current.get(CONV)?.abort();
    await live.catch(() => {});
  }, 20000);

  it("spends no probe on a stream that produced bytes recently (the watchdog gate)", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      streamBodies: [
        (signal) =>
          zombieStream(signal, 5, [sse(1, "turn.started", { turn_id: "t1" })]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const live = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(50);
    const probesAfterAttach = h.inflightProbes;

    // Not forced: this is a watchdog tick, and the socket just delivered.
    await expect(result.current.checkStreamLiveness(CONV)).resolves.toBe(
      "healthy",
    );
    expect(h.inflightProbes).toBe(probesAfterAttach);

    h.deps.abortControllersRef.current.get(CONV)?.abort();
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
    const assistant = convSlot(h, CONV)[convSlot(h, CONV).length - 1];
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
    expect(convSlot(h, CONV).some((m) => m.cancelled)).toBe(false);
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
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
    expect(h.deps.lastEventIdByConvRef.current.get(CONV)).toBe(2);

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
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 30000);

  it("does not declare it dead before the promised keepalives are actually missed", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: unansweredHistory(),
      heartbeatMs: HEARTBEAT,
      streamBodies: [
        (signal) =>
          zombieStream(signal, 5, [sse(1, "turn.started", { turn_id: "t1" })]),
      ],
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

    h.deps.abortControllersRef.current.get(CONV)?.abort();
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
      streamBodies: [
        (signal) =>
          zombieStream(signal, 5, [sse(1, "turn.started", { turn_id: "t1" })]),
      ],
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

    h.deps.abortControllersRef.current.get(CONV)?.abort();
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
    expect(convSlot(h, OTHER)[convSlot(h, OTHER).length - 1].content).toBe(
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
    const spy = vi
      .spyOn(document, "visibilityState", "get")
      .mockReturnValue("hidden");

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
    // CONV's transcript is corrupt in a way that makes its check throw. The
    // store is a Map, so the fault goes on the read itself rather than on an
    // object property getter.
    const realGet = h.store.get.bind(h.store);
    h.store.get = (convId: string) => {
      if (convId === CONV) throw new Error("boom");
      return realGet(convId);
    };

    const { result } = renderHook(() => useTurnStream(h.deps));
    await expect(
      result.current.sweepStreamLiveness({ force: true }),
    ).resolves.toBeUndefined();
    expect(h.loadConversationCalls).toEqual([OTHER]);
  });
});

// Codex round 9 on #1584: the successor chase was the one recovery path with
// no ownership of its own — it could adopt a transcript that had not yet
// caught up, leave a conversation busy with nothing running, and outlive Stop.
describe("chasing a successor is owned, gated and cancellable", () => {
  it("does not adopt a stale transcript while the successor is still running", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      // Postgres answers OUR turn. It has not yet caught up with the
      // successor's: startTurn exposes a turn before the manager commits its
      // user message, so this transcript looks "finished" for the successor
      // too, and only /inflight can say otherwise.
      persisted: answeredHistory(),
      streamBodies: [() => severedStream([])],
      // 0: the submit catch's probe (arms the chain, no identity).
      // 2: the chase's first reattach probe — the flap is still on, so that
      //    reattach fails without proving anything about the successor.
      inflightRejectAt: [0, 2],
      inflight: [{ inflight: true, turn_id: "t-successor" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // One reload: the chain adopting OUR answer. The chase's own reattach
    // failed, but the server still reports the successor running, so nothing
    // may be adopted on its behalf.
    expect(h.loadConversationCalls).toEqual([CONV]);

    const attachesBefore = h.attachCount();
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // It comes back and ATTACHES once the flap clears — the successor is put
    // on screen rather than replaced by a transcript that predates it.
    expect(h.attachCount()).toBe(attachesBefore + 1);
  }, 20000);

  it("frees the conversation after adopting a successor that had already finished", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: answeredHistory(),
      streamBodies: [() => severedStream([])],
      inflightRejectAt: [0],
      // The chain's tick sees a successor; by the time the chase asks, that
      // successor has finished and its retained buffer has expired, so there
      // is nothing live and nothing to attach to — ever.
      inflight: [
        { inflight: true, turn_id: "t-successor" },
        { inflight: false },
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(50);
    // The answer came from the database, so no turn is running and the
    // composer must stop offering Stop.
    expect(h.streaming.has(CONV)).toBe(false);
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);

  it("stops chasing when the user presses Stop", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: answeredHistory(),
      streamBodies: [() => severedStream([])],
      // Every reattach probe after the chain's tick loses the flap, so the
      // chase keeps coming back until something cancels it.
      inflightRejectAt: [0, 2, 3, 4, 5, 6, 7, 8],
      inflight: [{ inflight: true, turn_id: "t-successor" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    expect(h.streaming.has(CONV)).toBe(true);

    result.current.cancelRecovery(CONV);
    expect(h.streaming.has(CONV)).toBe(false);

    const attachesAfterStop = h.attachCount();
    await vi.advanceTimersByTimeAsync(60000);
    // No further attach, and nothing re-marks the conversation busy behind
    // the user's back.
    expect(h.attachCount()).toBe(attachesAfterStop);
    expect(h.streaming.has(CONV)).toBe(false);
  }, 20000);
});

// Codex round 10 on #1584: the chain compared turn ids and then handed the
// attach to reattachToConv, which took its own /inflight look. Between the
// two, the recovered turn can finish and a queued successor start — and that
// second, unconstrained probe would attach to the successor while reusing the
// predecessor's still-open slot, pouring one turn's replay into another turn's
// bubble.
describe("a reattach is bound to the turn its caller identified", () => {
  it("declines a successor that started between the two probes", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: unansweredHistory(),
      streamBodies: [
        // Learns its identity, then dies: the chain is armed with "t-ours".
        () => severedStream([sse(1, "turn.started", { turn_id: "t-ours" })]),
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "someone else's answer" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflightRejectAt: [0], // the catch's probe: radio off, chain armed
      inflight: [
        { inflight: true, turn_id: "t-ours" }, // the chain's tick: still ours
        { inflight: true, turn_id: "t-successor" }, // the reattach's own look
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);
    const attachesBefore = h.attachCount();

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(10);
    // Nothing was attached, so nothing was poured into our bubble, and the
    // slot is still open for the chain's next tick rather than stamped.
    expect(h.attachCount()).toBe(attachesBefore);
    expect(lastOf(h).content).toBe("");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
    expect(h.streaming.has(CONV)).toBe(true);
  }, 20000);
});

// Codex round 11 on #1584: two ownership holes that only open once a chase or
// a nudge is in play.
describe("ownership holds through a chase and across nudged ticks", () => {
  it("does not settle a successor slot whose own stream was severed", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      // Postgres answers OUR turn, so the chain adopts it and chases the
      // successor the probe reports.
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([]), // ours: died before turn.started
        // The successor's stream: some text, then the socket dies with no
        // terminal event. Its outcome is UNKNOWN, not failed.
        () => severedStream([sse(1, "text.delta", { text: "partial" })]),
      ],
      inflightRejectAt: [0],
      inflight: [{ inflight: true, turn_id: "t-successor" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    // The chase was still registered while that stream's finalizer ran, so
    // nothing declared an outcome: the partial text is kept, the slot stays
    // open for the chain the chase handed it to, and the conversation stays
    // busy. Settling here would have reconciled against a transcript that
    // answers the PREDECESSOR and thrown the successor's text away.
    expect(lastOf(h).content).toBe("partial");
    expect(lastOf(h).state).toBe("streaming");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
    expect(h.streaming.has(CONV)).toBe(true);
  }, 20000);

  it("lets only the newest tick act when a nudge lands mid-probe", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: unansweredHistory(),
      streamBodies: [() => severedStream([])],
      inflightRejectAt: [0], // the catch's probe: arms the chain
      inflightDeferAt: [1], // the first tick's probe hangs until released
      inflight: [{ inflight: true, turn_id: "t-live" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    // The tick fires and blocks inside its probe, having already dropped its
    // timer entry — which is exactly when a focus or online event arrives.
    await vi.advanceTimersByTimeAsync(1100);
    const attachesBefore = h.attachCount();
    result.current.nudgeRecovery(CONV);

    // Release the stale tick's probe: it must exit rather than act on a slot
    // the newer tick now owns.
    h.releaseInflight();
    await vi.advanceTimersByTimeAsync(20);
    expect(h.attachCount()).toBe(attachesBefore);

    // The newer tick is what attaches.
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    expect(h.attachCount()).toBe(attachesBefore + 1);
  }, 20000);
});

// Codex round 12 on #1584: Stop has to settle whatever the chain was holding,
// whichever shape it is, and the queue follower must not attach over it.
describe("a confirmed Stop settles every shape recovery was holding", () => {
  it("marks a replay-gap slot cancelled, although it already reads as done", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: midTurnTranscript(),
      persisted: answeredHistory(),
      streamBodies: [
        // Terminal event after a gap, no answer: the slot lands `done` and
        // empty, so a state test would skip it and leave a blank bubble.
        () =>
          truncatedStream([
            sse(1, "reconnect", { type: "resumed", missed_events: 4 }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }, { inflight: false }],
      persistedRejectAt: [0], // the gap settle cannot reach Postgres
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);
    expect(lastOf(h).content).toBe("");

    result.current.cancelRecovery(CONV);
    expect(lastOf(h).cancelled).toBe(true);
    expect(h.streaming.has(CONV)).toBe(false);
  }, 20000);

  it("settles the slot a chase had already attached", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([]), // ours
        () => severedStream([sse(1, "text.delta", { text: "partial" })]), // the successor's
      ],
      inflightRejectAt: [0],
      inflight: [{ inflight: true, turn_id: "t-successor" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    expect(lastOf(h).state).toBe("streaming");

    result.current.cancelRecovery(CONV);
    // The reattach created that slot and its finalizer deferred to the chase,
    // so nothing else would ever settle it.
    expect(lastOf(h).cancelled).toBe(true);
    expect(lastOf(h).state).toBe("done");
    expect(h.streaming.has(CONV)).toBe(false);
  }, 20000);
});

// Codex round 13 on #1584: a reattach creates its assistant slot before it
// opens the stream, so a connect that never establishes leaves a thinking
// bubble behind. The recovery callers re-probe, but the generic ones — the
// initial conversation load, the tab-return handler — own no chain, and the
// liveness watchdog skips a conversation that is no longer attached.
describe("a reattach that never connects does not strand its slot", () => {
  it("hands the slot to the chain instead of spinning under a Send button", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: unansweredHistory(),
      streamBodies: [
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "the answer" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [{ inflight: true, turn_id: "t1" }],
      streamNeverConnectsAt: [0], // the first attach's connect is blackholed
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    void result.current.reattachToConv(CONV).catch(() => {});
    await vi.advanceTimersByTimeAsync(10);

    // The connect timer fires and aborts a stream that never arrived.
    await vi.advanceTimersByTimeAsync(8100);
    // Busy, not idle: the chain owns an unknown outcome, so Stop stays
    // offered rather than Send over a spinning bubble.
    expect(h.streaming.has(CONV)).toBe(true);
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);

    // And the chain does the work the generic caller could not.
    const attachesBefore = h.attachCount();
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    expect(h.attachCount()).toBe(attachesBefore + 1);
    expect(lastOf(h).content).toBe("the answer");
    expect(lastOf(h).state).toBe("done");
  }, 20000);
});

// Codex round 14 on #1584: a chased successor can end in the replay-gap
// shape — missed events, then a terminal event with no content — which reads
// as `done` and empty. The handoff helper hard-coded the non-gap shape, so it
// did not see that slot: the chase ended and idled the conversation over an
// empty reply whose answer was sitting in the database.
describe("a successor that ends in the replay-gap shape is still recovered", () => {
  it("hands the gap slot to the chain instead of leaving an empty reply", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([]), // ours: died before turn.started
        // The successor's replay: events were missed and the turn sealed
        // without delivering any answer content.
        () =>
          truncatedStream([
            sse(1, "reconnect", { type: "resumed", missed_events: 4 }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflightRejectAt: [0],
      inflight: [
        { inflight: true, turn_id: "t-successor" }, // the chain's tick
        { inflight: true, turn_id: "t-successor" }, // the chase's reattach
        { inflight: false }, // by the next tick the successor is over
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    // The chain adopts our answer and chases the successor, whose stream
    // seals empty.
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);

    // The chain, armed with the gap shape, adopts the canonical answer.
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    expect(h.loadConversationCalls.length).toBeGreaterThan(1);
    expect(lastOf(h).content).toBe("Done — here are the results.");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);
});

// Codex round 15 on #1584: two queued successors can drain in quick
// succession, so the turn the chase set out to follow may already be over by
// the time it attaches. An unbound attach took the LATER turn and replayed its
// answer under the earlier one's committed prompt — the first answer missing,
// the second misattributed, until a reload.
describe("a chase follows the successor it discovered", () => {
  it("adopts the finished successor before re-targeting the next one", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      persisted: answeredHistory(),
      streamBodies: [
        () => severedStream([]), // ours: died before turn.started
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "the second successor's answer" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflightRejectAt: [0],
      inflight: [
        { inflight: true, turn_id: "t-a" }, // the chain's tick discovers A
        { inflight: true, turn_id: "t-b" }, // A is over; B is running
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    // The bound attach declined a turn that was not the one discovered, and
    // the chase reloaded the canonical transcript rather than writing B's
    // reply under A's prompt. Two reloads: our own answer, then A's.
    expect(h.loadConversationCalls.length).toBe(2);

    // It then re-targets and attaches to B.
    const attachesBefore = h.attachCount();
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    expect(h.attachCount()).toBe(attachesBefore + 1);
    expect(lastOf(h).content).toBe("the second successor's answer");
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);
});

// Codex round 16 on #1584: a session that expires mid-turn makes /inflight
// answer 401. That is silence wearing a different hat — it says nothing about
// whether the turn is running — but it was classified as a definitive "no turn
// exists", which stamped the slot failed over a backend turn still generating.
describe("an authentication failure is indeterminate, not an answer", () => {
  it("keeps the slot open when /inflight answers 401", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      // Postgres has no answer yet — the turn really is still running.
      persisted: unansweredHistory(),
      streamBodies: [() => severedStream([])],
      inflightRejectAt: [0], // the catch's probe: arms the chain
      inflightUnauthorizedAt: [1], // the tick's probe: the session expired
      inflight: [{ inflight: true, turn_id: "t1" }],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    // Nothing was settled and nothing was stamped: the chain comes back.
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
    expect(h.streaming.has(CONV)).toBe(true);

    // And it recovers once the session is good again.
    await vi.advanceTimersByTimeAsync(2100);
    await vi.advanceTimersByTimeAsync(20);
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
  }, 20000);
});

// Codex round 18 on #1584: when the liveness probe says the turn has ended,
// the reconcile releases the attach handle before it reloads. If that reload
// cannot be made, reporting health strands the bubble: every later sweep
// enumerates only attached conversations, so nothing would ever look again.
describe("liveness hands an unreachable reconcile to recovery", () => {
  it("arms the chain instead of reporting health over a stranded bubble", async () => {
    vi.useFakeTimers();
    const HEARTBEAT = 1000;
    const h = makeHarness({
      initial: [],
      persisted: answeredHistory(),
      heartbeatMs: HEARTBEAT,
      streamBodies: [
        (signal) => zombieStream(signal, undefined, []),
        () =>
          truncatedStream([
            sse(1, "text.delta", { text: "the answer" }),
            sse(2, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
      inflight: [
        { inflight: true, turn_id: "t1" }, // the attach
        { inflight: false }, // the liveness probe: the turn is over
        { inflight: false }, // the chain's tick
      ],
      // The reconcile's history request cannot be made.
      persistedRejectAt: [0],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    const zombie = result.current.reattachToConv(CONV);
    await vi.advanceTimersByTimeAsync(10);

    await vi.advanceTimersByTimeAsync(4 * HEARTBEAT + 1000);
    await result.current.checkStreamLiveness(CONV);
    await vi.advanceTimersByTimeAsync(20);
    // Busy and owned, not quietly "healthy" over a bubble nothing will visit.
    expect(result.current.isRecoveringConv(CONV)).toBe(true);
    expect(h.streaming.has(CONV)).toBe(true);

    // And the chain finishes the job once the database can be read.
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    expect(convSlot(h, CONV).some((m) => m.failed)).toBe(false);
    void zombie;
  }, 20000);
});

// Codex round 21 on #1584: a reasoning-only reply is legitimately `done` with
// empty content — turn.completed keeps the reasoning it accumulated. Probing
// the replay-gap shape unconditionally read that finished turn as unsettled,
// so a confirmed Stop during a chase reached back and stamped the previous,
// completed turn cancelled.
describe("a reasoning-only reply is an answer, not a gap", () => {
  it("is not cancelled when Stop ends a chase that has no slot yet", async () => {
    vi.useFakeTimers();
    const h = makeHarness({
      initial: [],
      // Our own turn's answer is reasoning-only.
      persisted: reasoningOnlyHistory(),
      streamBodies: [() => severedStream([])],
      inflightRejectAt: [0],
      inflight: [
        // The chain's tick: a successor is running, so it adopts our answer
        // and starts a chase bound to that id.
        { inflight: true, turn_id: "t-successor" },
        // The chase's reattach sees a different id and declines BEFORE
        // creating a slot, so the chase has none of its own.
        { inflight: true, turn_id: "t-other" },
        // Its own probe is back on the turn it is chasing, so it simply
        // waits for the next tick.
        { inflight: true, turn_id: "t-successor" },
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.submitPrompt("run the long job");
    await vi.advanceTimersByTimeAsync(10);
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(20);
    expect(lastOf(h).reasoning).toBe("a long silent deliberation");

    result.current.cancelRecovery(CONV);
    // The chase had no slot, so Stop must settle nothing — least of all the
    // finished turn above it.
    expect(lastOf(h).cancelled).toBeUndefined();
    expect(lastOf(h).reasoning).toBe("a long silent deliberation");
  }, 20000);
});
