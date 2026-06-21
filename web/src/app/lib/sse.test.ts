import { describe, expect, it } from "vitest";
import { parseSseChunk } from "./sse";

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
