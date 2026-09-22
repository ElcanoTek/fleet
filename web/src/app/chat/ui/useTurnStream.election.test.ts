import { renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useTurnStream, type TurnStreamDeps } from "./useTurnStream";
import {
  createRecoveryElection,
  type RecoveryBeat,
  type RecoveryElection,
  type RecoveryLockManager,
  type RecoveryRelayChannel,
} from "./recoveryElection";
import type { HistoryEntry, Message } from "./history";

// Two tabs open on the same conversation, both holding a slot whose outcome a
// dead socket left unknown (#1595).
//
// Before this, each tab ran its own recovery chain: two ladders of /inflight
// probes re-learning the same "still nothing" for as long as the outage
// lasted. The election lets ONE of them do the asking — and nothing more than
// that. These tests pin the three things that has to be true of it:
//
//   * the tab that stands down is never worse off. Its slot stays mid-flight
//     (never a spinner frozen over a terminal verdict), and the moment the
//     asking tab reaches the server, that tab goes and gets its own content —
//     it is woken, not told what to think;
//   * a tab that dies mid-recovery has its work picked up, because the lock it
//     held is released by the browser and granted to the next tab;
//   * where the primitives are missing, every tab keeps its own chain exactly
//     as it did before — correct, and merely duplicative.

const CONV = "conv-1";

const sse = (id: number, event: string, data: unknown) =>
  `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

const midTurnTranscript = (): Message[] => [
  { id: 1, role: "user", content: "run the long job", state: "done" },
  { id: 2, role: "assistant", content: "", state: "streaming" },
];

const unansweredHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
];

const answeredHistory = (): HistoryEntry[] => [
  { role: "user", type: "text", content: { text: "run the long job" } },
  {
    role: "assistant",
    type: "text",
    content: { text: "Done — here are the results." },
  },
];

// ── One server, two tabs ────────────────────────────────────────────────────
//
// Both tabs talk to the same fake server, which is the point: the quantity
// under test is how many requests it receives, so it has to be counted in one
// place. `online` is the outage — every request throws while it is false,
// which is what a browser does with the radio off.
type Server = {
  online: boolean;
  inflight: { inflight: boolean; turn_id?: string };
  history: HistoryEntry[];
  probes: number;
  attaches: number;
  body: (signal?: AbortSignal) => ReadableStream<Uint8Array>;
};

// A turn that is still generating: it delivers what it has and then holds the
// socket open, exactly as a real one does between tokens. Nothing ends it but
// an abort, so a tab attached to it is inside its recovery tick for as long as
// the test runs — which is the whole point of the case it serves.
const liveStream =
  (frames: string[]) =>
  (signal?: AbortSignal): ReadableStream<Uint8Array> => {
    const encoder = new TextEncoder();
    let sent = false;
    return new ReadableStream<Uint8Array>({
      pull(controller) {
        if (!sent) {
          sent = true;
          for (const frame of frames) controller.enqueue(encoder.encode(frame));
          return;
        }
        return new Promise<void>((_resolve, reject) => {
          signal?.addEventListener("abort", () => {
            reject(new DOMException("aborted", "AbortError"));
          });
        });
      },
    });
  };

// The socket that dies with the radio: it hands over its frames, takes the
// network down, and only then errors — the order a locking phone produces.
const severedStream =
  (server: Server, frames: string[]) => (): ReadableStream<Uint8Array> => {
    const encoder = new TextEncoder();
    let i = 0;
    return new ReadableStream<Uint8Array>({
      pull(controller) {
        if (i < frames.length) {
          controller.enqueue(encoder.encode(frames[i++]));
          return;
        }
        server.online = false;
        controller.error(new TypeError("Load failed"));
      },
    });
  };

const makeServer = (): Server => {
  const server: Server = {
    online: true,
    inflight: { inflight: true, turn_id: "t1" },
    history: unansweredHistory(),
    probes: 0,
    attaches: 0,
    body: () => new ReadableStream<Uint8Array>({ start: (c) => c.close() }),
  };
  const json = (value: unknown) =>
    new Response(JSON.stringify(value), {
      status: 200,
      headers: { "content-type": "application/json" },
    });

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      // Counted before the outage check: what is under test is the requests
      // the BROWSER sends, and during an outage none of them arrive.
      if (url.includes("/inflight")) server.probes += 1;
      if (!server.online) throw new TypeError("Failed to fetch");
      if (url.includes("/inflight")) return json(server.inflight);
      if (url.includes("/queue")) return json({ items: [] });
      // Before the transcript fetch: both live under /api/conversations/.
      if (url.includes("/turns/")) {
        return new Response(JSON.stringify({ error: "turn not found" }), {
          status: 404,
          headers: { "content-type": "application/json" },
        });
      }
      if (url.includes("/stream")) {
        server.attaches += 1;
        return new Response(server.body(init?.signal ?? undefined), {
          status: 200,
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (url.includes("/api/conversations/"))
        return json({ history: server.history });
      return new Response("{}", { status: 404 });
    }),
  );
  return server;
};

type Tab = {
  deps: TurnStreamDeps;
  store: Map<string, Message[]>;
  streaming: Set<string>;
  slot: () => Message;
  lastBeat: () => RecoveryBeat | undefined;
  reports: () => boolean[];
};

// Which way a tab's last beat went is the one thing a test cannot read off the
// server, and the takeover case has to know which tab is doing the asking.
const instrument = (
  election: RecoveryElection,
): {
  election: RecoveryElection;
  lastBeat: () => RecoveryBeat | undefined;
  reports: () => boolean[];
} => {
  const beats: RecoveryBeat[] = [];
  const reports: boolean[] = [];
  return {
    election: {
      ...election,
      book: (convId: string, delayMs: number, run: () => void) => {
        const beat = election.book(convId, delayMs, run);
        beats.push(beat);
        return beat;
      },
      report: (convId: string, answered: boolean) => {
        reports.push(answered);
        election.report(convId, answered);
      },
    },
    lastBeat: () => beats[beats.length - 1],
    reports: () => reports,
  };
};

const makeTab = (server: Server, election: RecoveryElection): Tab => {
  const store = new Map<string, Message[]>([[CONV, midTurnTranscript()]]);
  const streaming = new Set<string>();
  const noop = () => {};
  const asyncNoop = async () => {};
  const instrumented = instrument(election);

  const deps: TurnStreamDeps = {
    setConvMessages: (convId, updater) => {
      const prev = store.get(convId) ?? [];
      store.set(convId, typeof updater === "function" ? updater(prev) : updater);
    },
    getConvMessages: (convId: string) => store.get(convId) ?? [],
    renameConvKey: noop,
    patchAssistantMessage: (convId, assistantId, updater) => {
      store.set(
        convId,
        (store.get(convId) ?? []).map((m) =>
          m.id === assistantId ? updater(m) : m,
        ),
      );
    },
    startThinkingCrossfade: noop,
    refreshConversations: asyncNoop,
    loadConversation: async (convId: string) => {
      const { historyToMessages } = await import("./history");
      store.set(convId, historyToMessages(server.history));
    },
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
    messagesByConvRef: { current: store },
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
    recoveryElection: instrumented.election,
  };

  return {
    deps,
    store,
    streaming,
    slot: () => {
      const slots = store.get(CONV) ?? [];
      return slots[slots.length - 1];
    },
    lastBeat: instrumented.lastBeat,
    reports: instrumented.reports,
  };
};

// The same lock bus both tabs' elections run on — one browser, one lock
// namespace. `killHolder` is the only way to model the tab that dies without
// releasing anything, which is the case the whole design turns on.
const makeLockBus = () => {
  type Waiter = { callback: () => Promise<void>; signal?: AbortSignal };
  const waiting = new Map<string, Waiter[]>();
  const held = new Map<string, number>();
  let tokens = 0;

  const pump = (name: string): void => {
    if (held.has(name)) return;
    const queue = waiting.get(name) ?? [];
    while (queue.length > 0) {
      const waiter = queue.shift();
      if (!waiter || waiter.signal?.aborted) continue;
      const token = ++tokens;
      held.set(name, token);
      void waiter.callback().then(() => {
        if (held.get(name) !== token) return;
        held.delete(name);
        pump(name);
      });
      return;
    }
  };

  const manager: RecoveryLockManager = {
    request: (name, options, callback) => {
      const queue = waiting.get(name) ?? [];
      queue.push({ callback, signal: options.signal });
      waiting.set(name, queue);
      pump(name);
      return new Promise((_resolve, reject) => {
        options.signal?.addEventListener("abort", () => {
          reject(new DOMException("aborted", "AbortError"));
        });
      });
    },
  };

  return {
    manager,
    holders: () => [...held.keys()],
    killHolder: () => {
      for (const name of [...held.keys()]) {
        held.delete(name);
        pump(name);
      }
    },
  };
};

const makeRelayBus = () => {
  const listeners = new Set<(message: unknown) => void>();
  return (): RecoveryRelayChannel => {
    let mine: ((message: unknown) => void) | null = null;
    return {
      postMessage: (message: unknown) => {
        for (const listener of [...listeners]) {
          if (listener !== mine) listener(message);
        }
      },
      listen: (handler: (message: unknown) => void) => {
        if (mine) listeners.delete(mine);
        mine = handler;
        listeners.add(handler);
      },
      close: () => {
        if (mine) listeners.delete(mine);
        mine = null;
      },
    };
  };
};

// Both tabs, each holding a mid-flight slot with a chain armed on it, with the
// server already unreachable — the state two tabs are in when the phone locks
// or the VPN drops mid-turn.
const armBothTabs = async (server: Server, first: Tab, second: Tab) => {
  const tabs = [first, second].map((tab) =>
    renderHook(() => useTurnStream(tab.deps)),
  );
  for (const tab of tabs) {
    server.online = true;
    server.body = severedStream(server, [
      sse(1, "turn.started", { turn_id: "t1" }),
      sse(2, "text.delta", { text: "partial " }),
    ]);
    await tab.result.current.reattachToConv(CONV);
    // Long enough for the finalizer to ask Postgres (and fail to reach it, so
    // the slot's outcome stays unknown and a chain is armed), far short of the
    // chain's first tick at 1 s.
    await vi.advanceTimersByTimeAsync(10);
  }
  return tabs;
};

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("two tabs waiting out the same outage", () => {
  it("spends one ladder of probes, and both tabs still get the answer", async () => {
    vi.useFakeTimers();
    const server = makeServer();
    const bus = makeLockBus();
    const openChannel = makeRelayBus();
    const elect = () =>
      createRecoveryElection({ locks: bus.manager, openChannel });
    const one = makeTab(server, elect());
    const two = makeTab(server, elect());

    const hooks = await armBothTabs(server, one, two);

    // Both chains tick, both find the server unreachable, and from here one of
    // them is doing the asking.
    await vi.advanceTimersByTimeAsync(1100);
    expect([one.lastBeat(), two.lastBeat()].sort()).toEqual([
      "elected",
      "local",
    ]);

    // The ladder's next rungs are +2 s, +4 s, +8 s. THREE probes, which is one
    // tab's worth: before the election this window cost six.
    const before = server.probes;
    await vi.advanceTimersByTimeAsync(15_000);
    expect(server.probes - before).toBe(3);

    // And the tab that stood down is not sitting on a verdict: its slot is
    // exactly where the other tab's is — mid-flight, partial answer kept,
    // conversation still busy, Stop still offered.
    for (const tab of [one, two]) {
      expect(tab.slot().state).toBe("streaming");
      expect(tab.slot().content).toBe("partial ");
      expect(tab.slot().failed).toBeUndefined();
      expect(tab.streaming.has(CONV)).toBe(true);
    }

    // The radio comes back, and the turn finished while it was off.
    server.online = true;
    server.inflight = { inflight: false };
    server.history = answeredHistory();

    // One tick of the asking tab's ladder. Its probe reaches the server, and
    // that is relayed — so the other tab resolves its own slot in the same
    // beat rather than waiting out its own ladder.
    const probesBefore = server.probes;
    await vi.advanceTimersByTimeAsync(16_100);
    expect(server.probes - probesBefore).toBeGreaterThanOrEqual(2);

    for (const tab of [one, two]) {
      expect(tab.slot().content).toBe("Done — here are the results.");
      expect(tab.slot().state).toBe("done");
      expect(tab.slot().failed).toBeUndefined();
      expect(tab.streaming.has(CONV)).toBe(false);
    }

    // The chains are over, so the lock is back: a tab that hits an outage
    // later must not find it still held by a conversation nobody is
    // recovering any more.
    await vi.advanceTimersByTimeAsync(10);
    expect(bus.holders()).toEqual([]);

    for (const hook of hooks) hook.unmount();
  }, 20000);

  it("wakes the other tab the moment the server answers, mid-stream", async () => {
    // The case that decides whether standing down is safe at all. The turn is
    // still generating when the radio comes back, so the asking tab attaches
    // and sits inside that stream for as long as it runs — it will not end its
    // chain, and no lock will change hands. The other tab has to be told, or
    // it would watch a thinking indicator for the length of the turn where
    // before it would have been rendering the same tokens.
    vi.useFakeTimers();
    const server = makeServer();
    const bus = makeLockBus();
    const openChannel = makeRelayBus();
    const elect = () =>
      createRecoveryElection({ locks: bus.manager, openChannel });
    const one = makeTab(server, elect());
    const two = makeTab(server, elect());

    const hooks = await armBothTabs(server, one, two);
    await vi.advanceTimersByTimeAsync(1100);
    const attachesBefore = server.attaches;

    server.online = true;
    server.inflight = { inflight: true, turn_id: "t1" };
    server.body = liveStream([sse(3, "text.delta", { text: "and the rest" })]);

    // One rung of the asking tab's ladder (+2 s).
    await vi.advanceTimersByTimeAsync(2100);

    // Both tabs are on the live turn, and both are rendering it.
    expect(server.attaches - attachesBefore).toBe(2);
    for (const tab of [one, two]) {
      expect(tab.slot().content).toBe("partial and the rest");
      expect(tab.slot().state).toBe("streaming");
      expect(tab.slot().failed).toBeUndefined();
    }

    for (const hook of hooks) hook.unmount();
  }, 20000);

  it("picks the work up when the asking tab dies mid-recovery", async () => {
    vi.useFakeTimers();
    const server = makeServer();
    const bus = makeLockBus();
    const openChannel = makeRelayBus();
    const elect = () =>
      createRecoveryElection({ locks: bus.manager, openChannel });
    const one = makeTab(server, elect());
    const two = makeTab(server, elect());

    const hooks = await armBothTabs(server, one, two);
    await vi.advanceTimersByTimeAsync(1100);

    const asking = one.lastBeat() === "local" ? 0 : 1;
    const standingDown = asking === 0 ? two : one;

    // The tab that was asking is gone — crashed, closed, killed by the OS. It
    // never said so; the browser released the lock it held.
    bus.killHolder();
    // …and its timers go with it, so nothing below can be its work.
    hooks[asking].unmount();

    // The lock is now the other tab's, and the beat it parked runs on the
    // deadline it was booked with. It is asking for itself from here.
    const before = server.probes;
    await vi.advanceTimersByTimeAsync(15_000);
    expect(server.probes - before).toBeGreaterThanOrEqual(2);

    server.online = true;
    server.inflight = { inflight: false };
    server.history = answeredHistory();
    await vi.advanceTimersByTimeAsync(31_000);

    expect(standingDown.slot().content).toBe("Done — here are the results.");
    expect(standingDown.slot().state).toBe("done");
    expect(standingDown.streaming.has(CONV)).toBe(false);

    hooks[asking === 0 ? 1 : 0].unmount();
  }, 20000);
});

describe("without the primitives, every tab recovers for itself", () => {
  it("runs both ladders and settles both tabs", async () => {
    vi.useFakeTimers();
    const server = makeServer();
    // What an insecure context (and jsdom) gives you: no navigator.locks. The
    // election does not exist, and nothing pretends otherwise.
    const elect = () => createRecoveryElection({ locks: null });
    const one = makeTab(server, elect());
    const two = makeTab(server, elect());
    expect(one.deps.recoveryElection?.mode).toBe("independent");

    const hooks = await armBothTabs(server, one, two);

    await vi.advanceTimersByTimeAsync(1100);
    expect(one.lastBeat()).toBe("local");
    expect(two.lastBeat()).toBe("local");

    // Two ladders over the same three rungs: six probes, not three. That is
    // the duplication #1595 is about, and it is what this deployment shape
    // keeps — correct, and merely twice.
    const before = server.probes;
    await vi.advanceTimersByTimeAsync(15_000);
    expect(server.probes - before).toBe(6);

    server.online = true;
    server.inflight = { inflight: false };
    server.history = answeredHistory();
    await vi.advanceTimersByTimeAsync(16_100);

    for (const tab of [one, two]) {
      expect(tab.slot().content).toBe("Done — here are the results.");
      expect(tab.streaming.has(CONV)).toBe(false);
    }

    for (const hook of hooks) hook.unmount();
  }, 20000);
});
