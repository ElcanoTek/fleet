import { describe, expect, it } from "vitest";
import { createSSEParser, type TaskStreamFrame } from "./orchestratorApi";

// #508: the live-activity SSE parser must assemble frames across arbitrary
// chunk boundaries, skip heartbeats, and survive malformed payloads.

function collect(): { frames: TaskStreamFrame[]; feed: (chunk: string) => void } {
  const frames: TaskStreamFrame[] = [];
  const feed = createSSEParser((f) => frames.push(f));
  return { frames, feed };
}

describe("createSSEParser", () => {
  it("parses complete frames with id/event/data lines", () => {
    const { frames, feed } = collect();
    feed('id: 1\nevent: tool_call\ndata: {"type":"tool_call","name":"bash","input":"ls"}\n\n');
    feed('id: 2\nevent: status\ndata: {"type":"status","status":"stopped","stopped_by":"alice"}\n\n');
    expect(frames).toHaveLength(2);
    expect(frames[0]).toMatchObject({ type: "tool_call", name: "bash" });
    expect(frames[1]).toMatchObject({ type: "status", status: "stopped", stopped_by: "alice" });
  });

  it("assembles frames split across chunk boundaries", () => {
    const { frames, feed } = collect();
    feed('data: {"type":"agent_mes');
    feed('sage","content":"hello"}');
    feed("\n\ndata: ");
    feed('{"type":"tool_result","output":"ok"}\n\n');
    expect(frames).toHaveLength(2);
    expect(frames[0]).toMatchObject({ type: "agent_message", content: "hello" });
    expect(frames[1]).toMatchObject({ type: "tool_result", output: "ok" });
  });

  it("skips heartbeats and malformed data without dying", () => {
    const { frames, feed } = collect();
    feed(": heartbeat\n\n");
    feed("data: not-json\n\n");
    feed('data: {"type":"status","status":"succeeded"}\n\n');
    expect(frames).toHaveLength(1);
    expect(frames[0].status).toBe("succeeded");
  });

  // #589: parity with the hardened chat parser (parseSseChunk). A proxy that
  // normalizes line endings to CRLF must not stall the stream, and a
  // multi-line `data:` payload must be reassembled per the SSE spec (joined
  // with "\n") rather than corrupted by bare concatenation.
  it("handles CRLF frame delimiters and CRLF line endings", () => {
    const { frames, feed } = collect();
    feed('event: tool_call\r\ndata: {"type":"tool_call","name":"bash","input":"ls"}\r\n\r\n');
    feed('data: {"type":"status","status":"succeeded"}\r\n\r\n');
    expect(frames).toHaveLength(2);
    expect(frames[0]).toMatchObject({ type: "tool_call", name: "bash" });
    expect(frames[1]).toMatchObject({ type: "status", status: "succeeded" });
  });

  it("handles a CRLF delimiter split across chunk boundaries", () => {
    const { frames, feed } = collect();
    feed('data: {"type":"agent_message","content":"hello"}\r\n\r');
    expect(frames).toHaveLength(0); // mid-delimiter — stays buffered, nothing lost
    feed("\n");
    expect(frames).toHaveLength(1);
    expect(frames[0]).toMatchObject({ type: "agent_message", content: "hello" });
  });

  it("reassembles multi-line data with newline separators", () => {
    const { frames, feed } = collect();
    feed('data: {\ndata:   "type": "agent_message",\ndata:   "content": "hello"\ndata: }\n\n');
    expect(frames).toHaveLength(1);
    expect(frames[0]).toMatchObject({ type: "agent_message", content: "hello" });
  });
});
