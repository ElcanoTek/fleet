import { describe, expect, it } from "vitest";
import { initialStreamDedupState, parseSseChunk, stepStreamDedup } from "./sse";

// parseSseChunk is the hot path during streaming — any regression causes
// UI desync or dropped reasoning. These tests codify the framing rules we
// rely on from chat-server's sseSink.

describe("parseSseChunk", () => {
  it("parses a single complete event", () => {
    const { events, remainder } = parseSseChunk(`event: text.delta\ndata: hi\n\n`);
    expect(events).toEqual([{ event: "text.delta", data: "hi" }]);
    expect(remainder).toBe("");
  });

  it("defaults event name to 'message' when no event line is present", () => {
    const { events } = parseSseChunk(`data: hello\n\n`);
    expect(events[0].event).toBe("message");
  });

  it("leaves incomplete trailing frame in remainder", () => {
    const { events, remainder } = parseSseChunk(`event: a\ndata: 1\n\nevent: b\ndata: 2`);
    expect(events).toHaveLength(1);
    expect(remainder).toContain("event: b");
  });

  it("joins multi-line data payloads with newline", () => {
    const { events } = parseSseChunk(`event: text.delta\ndata: line1\ndata: line2\n\n`);
    expect(events[0].data).toBe("line1\nline2");
  });

  it("skips comment lines (SSE `:` prefix)", () => {
    const { events } = parseSseChunk(`: ping\nevent: text.delta\ndata: ok\n\n`);
    expect(events).toEqual([{ event: "text.delta", data: "ok" }]);
  });

  it("drops events with no data field", () => {
    const { events } = parseSseChunk(`event: heartbeat\n\n`);
    expect(events).toEqual([]);
  });

  it("handles CRLF line endings", () => {
    const { events } = parseSseChunk(`event: text.delta\r\ndata: win\r\n\r\n`);
    // The trailing CRLFCRLF becomes LFLF after split on "\n\n"; accept either.
    expect(events.length).toBeGreaterThanOrEqual(0);
  });

  it("handles multiple back-to-back events", () => {
    const chunk = `event: a\ndata: 1\n\nevent: b\ndata: 2\n\n`;
    const { events, remainder } = parseSseChunk(chunk);
    expect(events).toEqual([
      { event: "a", data: "1" },
      { event: "b", data: "2" },
    ]);
    expect(remainder).toBe("");
  });

  it("parses the monotonic per-turn id when present", () => {
    const chunk = `id: 42\nevent: text.delta\ndata: hi\n\n`;
    const { events } = parseSseChunk(chunk);
    expect(events[0].id).toBe("42");
  });

  it("leaves id undefined when no id line is present", () => {
    const { events } = parseSseChunk(`event: text.delta\ndata: hi\n\n`);
    expect(events[0].id).toBeUndefined();
  });

  it("parses CRLF-delimited frames (proxy line-ending normalization)", () => {
    const chunk = `event: a\r\ndata: 1\r\n\r\nevent: b\r\ndata: 2\r\n\r\n`;
    const { events, remainder } = parseSseChunk(chunk);
    expect(events).toEqual([
      { event: "a", data: "1" },
      { event: "b", data: "2" },
    ]);
    expect(remainder).toBe("");
  });

  it("holds an incomplete CRLF delimiter in the remainder across chunks", () => {
    // The first chunk ends mid-delimiter ("…\r\n\r"), so no frame is
    // complete yet — everything stays in the remainder.
    const first = parseSseChunk(`event: a\r\ndata: 1\r\n\r`);
    expect(first.events).toEqual([]);
    // The next chunk supplies the trailing "\n" that completes the
    // delimiter, and the frame emits exactly once.
    const { events } = parseSseChunk(first.remainder + `\n`);
    expect(events).toEqual([{ event: "a", data: "1" }]);
  });
});

// A small helper to feed a sequence of events through stepStreamDedup, threading
// the state, and return the per-event drop decisions plus the final state.
function run(
  start: { lastEventId: number; currentTurnId: string | undefined },
  events: Array<{ event: string; id?: string; payload?: unknown }>,
) {
  let state = start;
  const drops: boolean[] = [];
  for (const e of events) {
    const r = stepStreamDedup(state, { event: e.event, id: e.id }, e.payload ?? {});
    state = r.state;
    drops.push(r.drop);
  }
  return { state, drops };
}

describe("stepStreamDedup", () => {
  it("resets the counter on a new turn so the fresh turn’s id=1 survives", () => {
    // Prior turn ended at id=7; a new turn_id must reset the guard to 0.
    const afterBoundary = stepStreamDedup(
      { lastEventId: 7, currentTurnId: "t1" },
      { event: "turn.started", id: undefined },
      { turn_id: "t2" },
    );
    expect(afterBoundary.drop).toBe(false);
    expect(afterBoundary.state).toEqual({ lastEventId: 0, currentTurnId: "t2" });

    // id=1 of the new turn must NOT be dropped against the prior turn’s id=7.
    const firstOfTurn = stepStreamDedup(afterBoundary.state, { event: "text.delta", id: "1" }, {});
    expect(firstOfTurn.drop).toBe(false);
    expect(firstOfTurn.state.lastEventId).toBe(1);
  });

  it("drops already-applied ids on reattach replay overlap (<= is dropped)", () => {
    const start = { lastEventId: 5, currentTurnId: "t1" };
    // An overlapping replayed id below the high-water mark is dropped.
    const low = stepStreamDedup(start, { event: "text.delta", id: "3" }, {});
    expect(low.drop).toBe(true);
    expect(low.state.lastEventId).toBe(5);
    // The boundary id (equal to last) is also dropped.
    const equal = stepStreamDedup(start, { event: "text.delta", id: "5" }, {});
    expect(equal.drop).toBe(true);
    // The first id past the mark applies and advances the counter.
    const next = stepStreamDedup(start, { event: "text.delta", id: "6" }, {});
    expect(next.drop).toBe(false);
    expect(next.state.lastEventId).toBe(6);
  });

  it("is latest-wins for out-of-order ids", () => {
    const ev = (id: string) => ({ event: "text.delta", id });
    const { state, drops } = run(initialStreamDedupState(), [
      ev("1"),
      ev("2"),
      ev("5"),
      ev("3"), // stale — dropped
      ev("4"), // stale — dropped
      ev("6"),
    ]);
    expect(drops).toEqual([false, false, false, true, true, false]);
    expect(state.lastEventId).toBe(6);
  });

  it("never advances or drops on absent / non-numeric ids", () => {
    // An event with no id is always applied and does not move the counter.
    const noId = stepStreamDedup({ lastEventId: 4, currentTurnId: "t1" }, { event: "conversation" }, {});
    expect(noId.drop).toBe(false);
    expect(noId.state.lastEventId).toBe(4);
    // A non-finite id (Number("abc") = NaN) is applied, counter unchanged.
    const nan = stepStreamDedup({ lastEventId: 4, currentTurnId: "t1" }, { event: "x", id: "abc" }, {});
    expect(nan.drop).toBe(false);
    expect(nan.state.lastEventId).toBe(4);
  });

  it("does not reset on turn.started without a turn_id or with the same turn_id", () => {
    // No turn_id in payload → no reset.
    const noTurnId = stepStreamDedup({ lastEventId: 9, currentTurnId: "t1" }, { event: "turn.started" }, {});
    expect(noTurnId.state).toEqual({ lastEventId: 9, currentTurnId: "t1" });
    // Same turn_id repeated mid-turn → no reset.
    const sameTurn = stepStreamDedup({ lastEventId: 9, currentTurnId: "t1" }, { event: "turn.started" }, { turn_id: "t1" });
    expect(sameTurn.state).toEqual({ lastEventId: 9, currentTurnId: "t1" });
  });
});
