import { buildSubmissionMessage } from "./genui/model";
import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  hasPendingQueueWork,
  queueDrainFollowDelaysMs,
  useTurnStream,
  type QueuedInput,
  type TurnStreamDeps,
} from "./useTurnStream";
import type { HistoryEntry, Message } from "./history";
import { closedStream, createTurnStreamHarness, type TranscriptStore } from "./turnStreamTestHarness";

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

type Store = TranscriptStore;

// Read one conversation's slot out of the Map-backed store.
const convSlot = (h: Harness, convId: string): Message[] =>
  h.store.get(convId) ?? [];
type InflightInfo = { inflight: boolean; turn_id?: string; last_event_id?: number };

const sse = (id: number, event: string, data: unknown) =>
  `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

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
  // How many times a stream was actually opened. The count matters on its own:
  // re-opening a stream for a turn already on screen is what creates an
  // un-fillable "thinking" slot, whether or not the cleanup wins the race.
  streamAttaches: number;
};

const makeHarness = (opts: {
  initial: Message[];
  persisted: HistoryEntry[];
  // Consumed in order; the last entry is reused for any further read.
  queue: QueuedInput[][];
  inflight: InflightInfo[];
  streamBodies?: Array<() => ReadableStream<Uint8Array>>;
}): Harness => {
  const base = createTurnStreamHarness({
    conversationId: CONV,
    initial: opts.initial,
    persisted: opts.persisted,
  });
  let queueReads = 0;
  let probes = 0;
  let attaches = 0;

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

  return {
    deps: base.deps,
    store: base.store,
    loadConversationCalls: base.loadConversationCalls,
    get queueReads() {
      return queueReads;
    },
    get inflightProbes() {
      return probes;
    },
    get streamAttaches() {
      return attaches;
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
  it("keeps executing same-tool cards when a pending card is superseded", async () => {
    const initial = answeredTranscript();
    initial[1].approvals = [
      { id: "running", tool: "bash", status: "pending", executing: true, summary: {} },
      { id: "pending", tool: "bash", status: "pending", summary: {} },
    ];
    const h = makeHarness({
      initial,
      persisted: drainedHistory(),
      queue: [[queuedRow("q1", "running")], []],
      inflight: [{ inflight: true, turn_id: "t2" }],
      streamBodies: [() => closedStream([
        sse(1, "turn.started", { turn_id: "t2", input_id: "q1", queued: true }),
        sse(2, "user.message", { text: "stage again" }),
        sse(3, "tool.approval_superseded", { tool: "bash" }),
        sse(4, "turn.completed", {}),
      ])],
    });
    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.followQueueDrain(CONV);
    const cards = convSlot(h, CONV).flatMap((message) => message.approvals ?? []);
    expect(cards.find((card) => card.id === "running")).toMatchObject({ status: "pending", executing: true });
    expect(cards.find((card) => card.id === "pending")).toMatchObject({ status: "rejected" });
  });

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

    const msgs = convSlot(h, CONV);
    // The queued follow-up finally has a user bubble AND an answer.
    expect(msgs.map((m) => m.role)).toEqual(["user", "assistant", "user", "assistant"]);
    expect(msgs[2].content).toBe("keep it clear and concise");
    expect(msgs[3].content).toBe("Rewritten for the client.");
    expect(msgs[3].state).toBe("done");
    // Streamed live — no need to re-read the transcript from Postgres.
    expect(h.loadConversationCalls).toEqual([]);
  });

  it("text.replace retracts a superseded pre-audit draft", async () => {
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      queue: [[queuedRow("q1", "running")], []],
      inflight: [{ inflight: true, turn_id: "t2" }],
      streamBodies: [
        () =>
          closedStream([
            sse(1, "turn.started", { turn_id: "t2", input_id: "q1", queued: true }),
            sse(2, "user.message", { text: "keep it clear and concise" }),
            sse(3, "text.delta", { text: "DRAFT_SHOULD_VANISH" }),
            sse(4, "text.delta", { text: "Rewritten for the client." }),
            sse(5, "text.replace", { text: "Rewritten for the client." }),
            sse(6, "turn.completed", { cost_usd: 0.02, duration_ms: 20 }),
          ]),
      ],
    });

    const { result } = renderHook(() => useTurnStream(h.deps));
    await result.current.followQueueDrain(CONV);

    const msgs = convSlot(h, CONV);
    expect(msgs[3].content).toBe("Rewritten for the client.");
    expect(msgs[3].content).not.toContain("DRAFT_SHOULD_VANISH");
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

    const contents = convSlot(h, CONV).map((m) => m.content);
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
    expect(convSlot(h, CONV).map((m) => m.content)).toEqual([
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
    expect(result.current.queuedInputs.get(CONV)?.map((i) => i.id)).toEqual(["q1"]);
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
    expect(convSlot(h, CONV).map((m) => m.content)).toEqual([
      "run the analysis",
      "Here is the analysis.",
    ]);
    expect(convSlot(h, CONV).some((m) => m.state === "thinking" || m.state === "streaming")).toBe(
      false,
    );
    // The message is not lost — it is on the chip strip with a send-now button.
    expect(result.current.queuedInputs.get(CONV)?.map((i) => i.id)).toEqual(["q1"]);
  });

  // The orphan this whole describe exists to prevent, pinned at the point where
  // it is deterministic rather than as the ~5% race it surfaces as in CI.
  //
  // followQueueDrain loops after a successful reattach, and the queue snapshot it
  // re-reads can still show the row it just streamed as "running" — that read
  // happens milliseconds after the turn ended and the server's view lags. So it
  // goes back into reattachToConv for a turn ALREADY on screen. The probe still
  // answers inflight:true for the same turn_id, so the finished-turn guard (which
  // only fires on inflight:false) is bypassed, and the reattach appends a fresh
  // "thinking" slot. Every replayed event is then dropped by the Last-Event-ID
  // dedup, because we already applied all of them, so nothing ever lands in that
  // slot and no terminal event clears it.
  //
  // settleStreamedSlot usually erases the slot on the way out — which is why this
  // only shows up sometimes. It stops erasing it when another attach is in flight
  // concurrently, because loadConversation short-circuits while the conversation
  // still looks attached, and the user is left with a spinner under a finished
  // answer.
  //
  // Asserting on the stream count rather than on the leftover slot is what makes
  // this deterministic: the second stream is the defect, and the cleanup that
  // sometimes hides it is not part of the contract.
  it("does not re-stream a turn already on screen when the queue snapshot lags", async () => {
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      // The lag: the row still reads "running" on the re-read after its turn
      // has been streamed to completion, and only then goes away.
      queue: [[queuedRow("q1", "running")], [queuedRow("q1", "running")], []],
      // The server has not caught up either — same turn, still inflight.
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
    await result.current.followQueueDrain(CONV);

    // The drained turn is on screen exactly once...
    expect(convSlot(h, CONV).map((m) => m.content)).toEqual([
      "run the analysis",
      "Here is the analysis.",
      "keep it clear and concise",
      "Rewritten for the client.",
    ]);
    // ...and the follower opened ONE stream to put it there. A second open is
    // the bug: it can only append a slot whose every event is already applied.
    expect(h.streamAttaches).toBe(1);
    expect(convSlot(h, CONV).some((m) => m.state === "thinking" || m.state === "streaming")).toBe(
      false,
    );
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
    //
    // Unlike its siblings above, this case runs on REAL timers (afterEach
    // restores them and it never installs fake ones), so it races the real
    // follow schedule: queueDrainFollowDelaysMs starts at 250ms, and only after
    // that first attempt does the drain fetch, reattach, stream and withdraw
    // the optimistic pair. vi.waitFor's default 1s deadline is enough on an
    // idle machine and not enough on a loaded CI runner.
    //
    // The deadline below must stay UNDER this test's own timeout (the third
    // argument to it(), raised for exactly this reason). vitest's default
    // testTimeout is 5s and the repo overrides it nowhere, so a waitFor asking
    // for longer than that can never reach its own deadline — vitest aborts the
    // test first, and the useful "expected 5 to be 4" diff is replaced by a
    // bare "Test timed out in 5000ms" that reads like a slow test rather than
    // the defect it is.
    await vi.waitFor(() => expect(convSlot(h, CONV).length).toBe(4), { timeout: 15000, interval: 25 });

    const msgs = convSlot(h, CONV);
    // No orphan: the optimistic pair was withdrawn and the drained turn's own
    // replay rendered the exchange.
    expect(msgs.map((m) => m.role)).toEqual(["user", "assistant", "user", "assistant"]);
    expect(msgs[2].content).toBe("keep it clear and concise");
    expect(msgs[3].content).toBe("Rewritten for the client.");
    expect(msgs.some((m) => m.state === "thinking" || m.state === "streaming")).toBe(false);
  }, 20_000);
});

// A generative-UI card counts a refused message as unanswered and an accepted
// one as answered, even when its turn then fails (#1700). The bubble of a
// message the server never took carries notSent so the two can be told apart.
describe("a submission the server refused", () => {
  it("is marked notSent and reported as not sent", async () => {
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: [
        { role: "user", type: "text", content: { text: "run the analysis" } },
        { role: "assistant", type: "text", content: { text: "Here is the analysis." } },
      ],
      queue: [[]],
      inflight: [{ inflight: false }],
    });
    const harnessFetch = globalThis.fetch;
    const seen: Message[][] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input) === "/api/chat") {
          return new Response("slow down", { status: 429, headers: { "Retry-After": "5" } });
        }
        seen.push([...(h.store.get(CONV) ?? [])]);
        return harnessFetch(input, init);
      }),
    );
    const { result } = renderHook(() => useTurnStream(h.deps));
    const sent = await result.current.submitPrompt("[UI submission] card=c1 action=go\n{}");
    expect(sent).toBe(false);
    // While the bubble is on screen it says the server never took it.
    const bubble = seen
      .flat()
      .find((m) => m.role === "user" && m.content.startsWith("[UI submission]"));
    expect(bubble?.notSent).toBe(true);
  });
});

describe("a submission whose acknowledgement was lost", () => {
  it("is cleared of notSent once /inflight names it, and reported as sent", async () => {
    let ours = "";
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: drainedHistory(),
      queue: [[]],
      inflight: [{ inflight: true, turn_id: "t2" }],
      streamBodies: [
        () =>
          closedStream([
            sse(1, "turn.started", { turn_id: "t2" }),
            sse(2, "text.delta", { text: "Got it." }),
            sse(3, "turn.completed", { cost_usd: 0.01, duration_ms: 10 }),
          ]),
      ],
    });
    const harnessFetch = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/chat") {
          ours = (JSON.parse(String(init?.body)) as { submission_id: string }).submission_id;
          // The server took it; the response never arrived.
          throw new TypeError("network error");
        }
        if (url.includes("/inflight")) {
          return new Response(JSON.stringify({ inflight: true, turn_id: "t2", submission_id: ours }), {
            status: 200,
            headers: { "content-type": "application/json" },
          });
        }
        return harnessFetch(input, init);
      }),
    );
    const { result } = renderHook(() => useTurnStream(h.deps));
    const sent = await result.current.submitPrompt("[UI submission] card=c1 action=go\n{}");
    expect(sent).toBe(true);
    const bubble = convSlot(h, CONV).find((m) => m.role === "user" && m.content.startsWith("[UI submission]"));
    expect(bubble).toBeDefined();
    expect(bubble?.notSent).toBeFalsy();
  });
});

describe("a card answer queued behind a running turn", () => {
  const refusedQueue = async (text: string, fromCard = false) => {
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    const harnessFetch = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) =>
        String(input) === "/api/chat" ? new Response("boom", { status: 500 }) : harnessFetch(input, init),
      ),
    );
    const setPromptForKey = vi.fn();
    const deps = { ...h.deps, setPromptForKey, streamingConvsRef: { current: new Set([CONV]) } };
    const { result } = renderHook(() => useTurnStream(deps));
    expect(await result.current.submitPrompt(text, { fromCard })).toBe(false);
    return setPromptForKey;
  };

  it("never writes its marker text into the composer when refused", async () => {
    const setPromptForKey = await refusedQueue("[UI submission] card=c1 action=go\n{}", true);
    expect(setPromptForKey).not.toHaveBeenCalled();
  });

  it("treats typed text that merely looks like a card marker as composer text", async () => {
    const setPromptForKey = await refusedQueue("[UI submission] means the card was sent");
    expect(setPromptForKey).toHaveBeenLastCalledWith(CONV, "[UI submission] means the card was sent");
  });

  it("still gives typed text back to the composer when refused", async () => {
    const setPromptForKey = await refusedQueue("keep it short");
    expect(setPromptForKey).toHaveBeenLastCalledWith(CONV, "keep it short");
  });
});

describe("a lost-ack submission while another submission's turn runs", () => {
  const run = async (queueHasOurs: boolean) => {
    let ours = "";
    const h = makeHarness({ initial: answeredTranscript(), persisted: drainedHistory(), queue: [[]], inflight: [{ inflight: true, turn_id: "t9" }] });
    const harnessFetch = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const json = (body: unknown) =>
          new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
        if (url === "/api/chat") {
          ours = (JSON.parse(String(init?.body)) as { submission_id: string }).submission_id;
          throw new TypeError("network error");
        }
        if (url.includes("/inflight")) {
          // A turn for SOMEONE ELSE's submission is running.
          return json({ inflight: true, turn_id: "t9", submission_id: `not-${ours}` });
        }
        if (url.includes("/queue")) {
          return json({ items: queueHasOurs ? [{ ...queuedRow("q1"), submission_id: ours }] : [queuedRow("q2")] });
        }
        return harnessFetch(input, init);
      }),
    );
    vi.useFakeTimers();
    const { result } = renderHook(() => useTurnStream(h.deps));
    const sending = result.current.submitPrompt("[UI submission] card=c1 action=go\n{}");
    await vi.advanceTimersByTimeAsync(10);
    const sent = await sending;
    const bubble = convSlot(h, CONV).find((m) => m.role === "user" && m.content.startsWith("[UI submission]"));
    return { sent, bubble };
  };

  it("is reported as sent once the queue holds a row with its submission id", async () => {
    const { sent, bubble } = await run(true);
    expect(sent).toBe(true);
    expect(bubble).toBeDefined();
    expect(bubble?.notSent).toBeFalsy();
  });

  it("stays not sent when no queue row is its own (another tab's turn)", async () => {
    const { sent, bubble } = await run(false);
    expect(sent).toBe(false);
    expect(bubble?.notSent).toBe(true);
  });
});

describe("a queued (busy-conversation) submission whose response was lost", () => {
  const run = async (queueHasOurs: boolean, persistedHasOurs = false) => {
    let ours = "";
    const h = makeHarness({
      initial: answeredTranscript(),
      persisted: persistedHasOurs
        ? [
            { role: "user", type: "text", content: { text: "run the analysis" } },
            { role: "assistant", type: "text", content: { text: "Here is the analysis." } },
            { role: "user", type: "text", content: { text: "keep it short" } },
            { role: "assistant", type: "text", content: { text: "Done." } },
          ]
        : [],
      queue: [[]],
      inflight: [{ inflight: false }],
    });
    const harnessFetch = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/chat") {
          const body = JSON.parse(String(init?.body)) as { submission_id?: string; mode?: string };
          expect(body.mode).toBe("queue");
          ours = body.submission_id ?? "";
          throw new TypeError("network error");
        }
        if (url.includes("/queue")) {
          const items = queueHasOurs ? [{ ...queuedRow("q1"), submission_id: ours }] : [];
          return new Response(JSON.stringify({ items }), { status: 200, headers: { "content-type": "application/json" } });
        }
        return harnessFetch(input, init);
      }),
    );
    const setPromptForKey = vi.fn();
    const deps = { ...h.deps, setPromptForKey, streamingConvsRef: { current: new Set([CONV]) } };
    const { result } = renderHook(() => useTurnStream(deps));
    const sent = await result.current.submitPrompt("keep it short");
    return { sent, ours, setPromptForKey };
  };

  it("is reported sent when the queue holds its row", async () => {
    const { sent, ours, setPromptForKey } = await run(true);
    expect(ours).not.toBe("");
    expect(sent).toBe(true);
    expect(setPromptForKey).not.toHaveBeenCalledWith(CONV, "keep it short");
  });

  it("is reported sent when it already drained and completed (persisted, no longer queued)", async () => {
    const { sent, setPromptForKey } = await run(false, true);
    expect(sent).toBe(true);
    expect(setPromptForKey).not.toHaveBeenCalledWith(CONV, "keep it short");
  });

  it("is reported not sent, text given back, when the queue does not", async () => {
    const { sent, setPromptForKey } = await run(false);
    expect(sent).toBe(false);
    expect(setPromptForKey).toHaveBeenLastCalledWith(CONV, "keep it short");
  });
});

describe("a lost queued response when the server cannot be asked", () => {
  const run = async (fromCard: boolean) => {
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input) === "/api/chat") throw new TypeError("network error");
        // Every follow-up read fails too: the outcome is unknown.
        return new Response("upstream down", { status: 503 });
      }),
    );
    const setPromptForKey = vi.fn();
    const deps = { ...h.deps, setPromptForKey, streamingConvsRef: { current: new Set([CONV]) } };
    const { result } = renderHook(() => useTurnStream(deps));
    const sent = await result.current.submitPrompt("[UI submission] card=c1 action=go\n{}", { fromCard });
    return { sent, setPromptForKey };
  };

  it("holds a card answer as possibly sent rather than inviting a duplicate", async () => {
    const { sent, setPromptForKey } = await run(true);
    expect(sent).toBe(true);
    expect(setPromptForKey).not.toHaveBeenCalled();
  });

  it("gives typed text back so it is not lost", async () => {
    const { sent, setPromptForKey } = await run(false);
    expect(sent).toBe(false);
    expect(setPromptForKey).toHaveBeenLastCalledWith(CONV, "[UI submission] card=c1 action=go\n{}");
  });
});

describe("a direct card send whose response is lost while the server is unreachable", () => {
  it("is held as possibly sent while recovery owns the outcome", async () => {
    vi.useFakeTimers();
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input) === "/api/chat") throw new TypeError("network error");
        return new Response("upstream down", { status: 503 });
      }),
    );
    const { result } = renderHook(() => useTurnStream(h.deps));
    const sending = result.current.submitPrompt("[UI submission] card=c1 action=go\n{}", { fromCard: true });
    await vi.advanceTimersByTimeAsync(10);
    expect(await sending).toBe(true);
  });
});

describe("a held card send that recovery later proves absent", () => {
  const run = async (landed: boolean, queueFailures = 0) => {
    vi.useFakeTimers();
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input) === "/api/chat") throw new TypeError("network error");
        return new Response("upstream down", { status: 503 });
      }),
    );
    const { result } = renderHook(() => useTurnStream(h.deps));
    const onUnsent = vi.fn();
    const sending = result.current.submitPrompt("[UI submission] card=c1 action=go\n{}", { fromCard: true, onUnsent });
    await vi.advanceTimersByTimeAsync(10);
    expect(await sending).toBe(true);
    expect(onUnsent).not.toHaveBeenCalled();
    // The server comes back: no turn, nothing queued, nothing persisted.
    const json = (body: unknown) =>
      new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes("/queue")) {
          if (queueFailures-- > 0) return new Response("upstream down", { status: 503 });
          return json({ items: [] });
        }
        if (url.includes("/inflight")) return json({ inflight: false, turn_id: "" });
        if (url.includes("/api/conversations/"))
          return json({
            history: [
              { role: "user", type: "text", content: { text: "run the analysis" } },
              { role: "assistant", type: "text", content: { text: "Here is the analysis." } },
              ...(landed ? [{ role: "user", type: "text", content: { text: "[UI submission] card=c1 action=go\n{}" } }] : []),
            ],
          });
        return new Response("{}", { status: 404 });
      }),
    );
    await vi.advanceTimersByTimeAsync(120_000);
    held = convSlot(h, CONV).filter((m) => m.role === "user" && m.content === "[UI submission] card=c1 action=go\n{}");
    return onUnsent;
  };
  let held: Message[] = [];

  it("tells the card, so its hold is released", async () => {
    expect(await run(false)).toHaveBeenCalledTimes(1);
  });

  it("leaves the hold when the server does hold the submission", async () => {
    expect(await run(true)).not.toHaveBeenCalled();
    // ...and the answer stops reading as refused, so the card locks on it.
    expect(held.some((m) => m.notSent)).toBe(false);
  });

  it("keeps asking when the answer is still unknown after recovery", async () => {
    expect(await run(false, 3)).toHaveBeenCalledTimes(1);
  });
});

describe("a held queued card send", () => {
  const run = async (landed: boolean) => {
    vi.useFakeTimers();
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input) === "/api/chat") throw new TypeError("network error");
        return new Response("upstream down", { status: 503 });
      }),
    );
    const deps = { ...h.deps, streamingConvsRef: { current: new Set([CONV]) } };
    const { result } = renderHook(() => useTurnStream(deps));
    const onUnsent = vi.fn();
    const text = "[UI submission] card=c1 action=go\n{}";
    const sending = result.current.submitPrompt(text, { fromCard: true, onUnsent });
    await vi.advanceTimersByTimeAsync(10);
    expect(await sending).toBe(true);
    // The server comes back.
    const json = (body: unknown) =>
      new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes("/queue")) return json({ items: [] });
        if (url.includes("/api/conversations/"))
          return json({ history: landed ? [{ role: "user", type: "text", content: { text } }] : [] });
        return new Response("{}", { status: 404 });
      }),
    );
    await vi.advanceTimersByTimeAsync(120_000);
    return onUnsent;
  };

  it("releases the hold once the server shows it never arrived", async () => {
    expect(await run(false)).toHaveBeenCalledTimes(1);
  });

  it("keeps the hold when the server has it", async () => {
    expect(await run(true)).not.toHaveBeenCalled();
  });
});

describe("a lost queued card response whose presence probes hang", () => {
  it("times the probes out instead of leaving the card sending for good", async () => {
    vi.useFakeTimers();
    // AbortSignal.timeout runs on the platform's own timers; route it through
    // the faked ones so the bound can be observed.
    const timeoutSpy = vi.spyOn(AbortSignal, "timeout").mockImplementation((ms: number) => {
      const c = new AbortController();
      setTimeout(() => c.abort(new DOMException("timed out", "TimeoutError")), ms);
      return c.signal;
    });
    try {
      const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
      vi.stubGlobal(
        "fetch",
        vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
          if (String(input) === "/api/chat") return Promise.reject(new TypeError("network error"));
          // Accepted, then blackholed: only the abort ends it.
          return new Promise<Response>((_, reject) => {
            init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
          });
        }),
      );
      const deps = { ...h.deps, streamingConvsRef: { current: new Set([CONV]) } };
      const { result } = renderHook(() => useTurnStream(deps));
      let settled: boolean | undefined;
      void result.current.submitPrompt("[UI submission] card=c1 action=go\n{}", { fromCard: true }).then((v) => {
        settled = v;
      });
      await vi.advanceTimersByTimeAsync(20_000);
      // Unknown, so the card holds (true) rather than hanging in Sending…
      expect(settled).toBe(true);
    } finally {
      timeoutSpy.mockRestore();
    }
  });
});

describe("a queued card answer removed from the queue", () => {
  it("releases the card's hold once the server no longer has it", async () => {
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    let sid = "";
    let removed = false;
    const json = (body: unknown, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/chat") {
          sid = (JSON.parse(String(init?.body)) as { submission_id: string }).submission_id;
          return json({ queued: true, input: { id: "q1", client_input_id: "c-q1", mode: "queued", state: "queued", position: 1 }, conversation_id: CONV }, 202);
        }
        if (url.includes("/queue/") && init?.method === "DELETE") {
          removed = true;
          return json({});
        }
        if (url.includes("/queue")) return json({ items: removed ? [] : [{ ...queuedRow("q1"), submission_id: sid }] });
        if (url.includes("/api/conversations/")) return json({ history: [] });
        return new Response("{}", { status: 404 });
      }),
    );
    const deps = { ...h.deps, streamingConvsRef: { current: new Set([CONV]) } };
    const { result } = renderHook(() => useTurnStream(deps));
    const onUnsent = vi.fn();
    await act(async () => {
      expect(await result.current.submitPrompt("[UI submission] card=c1 action=go\n{}", { fromCard: true, onUnsent })).toBe(true);
    });
    expect(onUnsent).not.toHaveBeenCalled();
    await act(async () => {
      await result.current.removeQueuedInput(CONV, "q1");
    });
    await vi.waitFor(() => expect(onUnsent).toHaveBeenCalledTimes(1));
  });
});

describe("a queued card answer whose ack was lost, later removed", () => {
  it("is still watched, so its removal releases the card", async () => {
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    let sid = "";
    let removed = false;
    const json = (body: unknown, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/chat") {
          sid = (JSON.parse(String(init?.body)) as { submission_id: string }).submission_id;
          // The server queued it, but the response never arrived.
          throw new TypeError("network error");
        }
        if (url.includes("/queue/") && init?.method === "DELETE") {
          removed = true;
          return json({});
        }
        if (url.includes("/queue")) return json({ items: removed ? [] : [{ ...queuedRow("q1"), submission_id: sid }] });
        if (url.includes("/api/conversations/")) return json({ history: [] });
        return new Response("{}", { status: 404 });
      }),
    );
    const deps = { ...h.deps, streamingConvsRef: { current: new Set([CONV]) } };
    const { result } = renderHook(() => useTurnStream(deps));
    const onUnsent = vi.fn();
    await act(async () => {
      expect(await result.current.submitPrompt("[UI submission] card=c1 action=go\n{}", { fromCard: true, onUnsent })).toBe(true);
    });
    await act(async () => {
      await result.current.removeQueuedInput(CONV, "q1");
    });
    await vi.waitFor(() => expect(onUnsent).toHaveBeenCalledTimes(1));
  });
});

describe("a queued card answer held across a page load", () => {
  it("names its queue row, and a later page resumes the watch and releases the card once the row is gone", async () => {
    const h = makeHarness({ initial: answeredTranscript(), persisted: [], queue: [[]], inflight: [{ inflight: false }] });
    let sid = "";
    let removed = false;
    const json = (body: unknown, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/chat") {
          sid = (JSON.parse(String(init?.body)) as { submission_id: string }).submission_id;
          return json({ queued: true, input: { id: "q1", client_input_id: "c-q1", mode: "queued", state: "queued", position: 1 }, conversation_id: CONV }, 202);
        }
        if (url.includes("/queue")) return json({ items: removed ? [] : [{ ...queuedRow("q1"), submission_id: sid }] });
        if (url.includes("/api/conversations/")) return json({ history: [] });
        return new Response("{}", { status: 404 });
      }),
    );
    const deps = { ...h.deps, streamingConvsRef: { current: new Set([CONV]) } };
    const text = "[UI submission] card=c1 action=go\n{}";
    const first = renderHook(() => useTurnStream(deps));
    const onHeld = vi.fn();
    await act(async () => {
      expect(await first.result.current.submitPrompt(text, { fromCard: true, onUnsent: vi.fn(), onHeld })).toBe(true);
    });
    expect(onHeld).toHaveBeenCalledWith(CONV, sid);
    first.unmount();

    // A new page load: the in-page watcher is gone; the row is removed.
    removed = true;
    const second = renderHook(() => useTurnStream(deps));
    const onUnsent = vi.fn();
    act(() => second.result.current.resumeHeldCardSend(CONV, text, sid, onUnsent));
    await vi.waitFor(() => expect(onUnsent).toHaveBeenCalledTimes(1));
  });
});

describe("retrying a card answer's failed turn", () => {
  it("resends it as a card send: the composer's text is left alone", async () => {
    const card = buildSubmissionMessage("c1", "go", { a: 1 });
    const h = makeHarness({
      initial: [
        { id: 1, role: "user", content: card, state: "done" },
        { id: 2, role: "assistant", content: "", state: "done", failed: true },
      ] as Message[],
      persisted: [],
      queue: [[]],
      inflight: [{ inflight: false }],
    });
    const harnessFetch = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) =>
        String(input) === "/api/chat" ? new Response("boom", { status: 500 }) : harnessFetch(input, init),
      ),
    );
    const setPromptForKey = vi.fn();
    const deps = { ...h.deps, setPromptForKey };
    const { result } = renderHook(() => useTurnStream(deps));
    await act(async () => {
      await result.current.retryLastUserMessage();
    });
    expect(setPromptForKey).not.toHaveBeenCalled();
  });
});
