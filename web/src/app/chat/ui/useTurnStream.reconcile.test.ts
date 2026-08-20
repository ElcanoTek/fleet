import { describe, expect, it } from "vitest";
import { persistedAnswersLocalTurn } from "./useTurnStream";
import type { HistoryEntry, Message } from "./history";

// Walk-away-and-come-back recovery.
//
// A phone that locks mid-turn severs the SSE socket while the server keeps
// generating. The turn finishes and lands in Postgres; the browser never
// hears about it. Every finalizer in the turn loop used to settle that
// orphaned slot by stamping `state: "done"` in place, which renders as "The
// assistant finished without a written reply." — a claim the DB flatly
// contradicts, and one the user could only clear by refreshing.
//
// persistedAnswersLocalTurn is the predicate that replaces the guess: does
// the canonical transcript already answer the turn we are holding open, i.e.
// would refreshing right now show the reply? Its second condition (user-turn
// coverage) is what keeps recovery from adopting a STALE transcript whose
// last assistant reply belongs to the PREVIOUS turn.

const user = (text: string): HistoryEntry => ({
  role: "user",
  type: "text",
  content: { text },
});
const assistant = (text: string): HistoryEntry => ({
  role: "assistant",
  type: "text",
  content: { text },
});
const toolCall = (id: string, name: string): HistoryEntry => ({
  role: "assistant",
  type: "tool_call",
  content: { id, name, input: "{}" },
});

// The in-memory transcript as the client holds it while a turn streams: the
// user's prompt plus an assistant slot that never received its answer.
const localMidTurn = (prompts: string[]): Message[] => [
  ...prompts.flatMap((p, i): Message[] => [
    { id: i * 2 + 1, role: "user", content: p, state: "done" },
    { id: i * 2 + 2, role: "assistant", content: "", state: "done" },
  ]).slice(0, prompts.length * 2 - 1),
  { id: 999, role: "assistant", content: "", state: "streaming" },
];

describe("persistedAnswersLocalTurn", () => {
  it("adopts the persisted reply when the DB answered the turn we lost", () => {
    const history = [user("run the long job"), assistant("all done, here are the results")];
    expect(persistedAnswersLocalTurn(history, localMidTurn(["run the long job"]))).toBe(true);
  });

  it("counts a tool-only assistant turn as answered", () => {
    const history = [user("deploy it"), toolCall("t1", "bash")];
    expect(persistedAnswersLocalTurn(history, localMidTurn(["deploy it"]))).toBe(true);
  });

  it("refuses a transcript that stops at the user's prompt (turn still running)", () => {
    const history = [user("run the long job")];
    expect(persistedAnswersLocalTurn(history, localMidTurn(["run the long job"]))).toBe(false);
  });

  it("refuses a STALE transcript whose reply belongs to the previous turn", () => {
    // Two prompts locally, one answered turn on the server: adopting this
    // would drop the user's newest prompt out of the transcript and pass off
    // the old answer as the new one.
    const history = [user("first"), assistant("answer to first")];
    const local = localMidTurn(["first", "second"]);
    expect(persistedAnswersLocalTurn(history, local)).toBe(false);
  });

  it("accepts a transcript that ran ahead of us (queued input already answered)", () => {
    const history = [
      user("first"),
      assistant("answer to first"),
      user("second"),
      assistant("answer to second"),
    ];
    expect(persistedAnswersLocalTurn(history, localMidTurn(["first", "second"]))).toBe(true);
  });

  it("refuses a persisted turn the server recorded as failed", () => {
    // historyToMessages only marks `failed` from an explicit error row; a
    // transcript ending in a user prompt is the common not-yet-answered
    // shape, and an empty history can never be a recovery.
    expect(persistedAnswersLocalTurn([], localMidTurn(["anything"]))).toBe(false);
    expect(persistedAnswersLocalTurn(null, localMidTurn(["anything"]))).toBe(false);
    expect(persistedAnswersLocalTurn(undefined, localMidTurn(["anything"]))).toBe(false);
  });

  it("does not treat a trailing user prompt as an answer", () => {
    const history = [user("first"), assistant("answer"), user("second")];
    expect(persistedAnswersLocalTurn(history, localMidTurn(["first", "second"]))).toBe(false);
  });
});
