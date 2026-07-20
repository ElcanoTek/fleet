import { describe, expect, it } from "vitest";
import { classifyQueueSubmitResponse } from "./useTurnStream";

// #824: a mode:"queue" submission's response is only a queue ack when the
// server actually queued it. With a stale client busy flag the server starts
// a direct turn and answers with a live SSE stream — misclassifying that as
// an ack leaves a billed turn running with nobody reading it (no user bubble,
// no stream, nothing in the queue until a page refresh).

const res = (status: number, contentType: string | null) => ({
  ok: status >= 200 && status < 300,
  status,
  headers: {
    get: (name: string) =>
      name.toLowerCase() === "content-type" ? contentType : null,
  },
});

describe("classifyQueueSubmitResponse", () => {
  it("202 JSON ack is queued", () => {
    expect(
      classifyQueueSubmitResponse(res(202, "application/json; charset=utf-8")),
    ).toBe("queued");
  });

  it("200 JSON is queued (idempotent replay of an accepted input)", () => {
    expect(
      classifyQueueSubmitResponse(res(200, "application/json; charset=utf-8")),
    ).toBe("queued");
  });

  it("200 SSE is a live turn, not an ack — the stale-busy race", () => {
    expect(classifyQueueSubmitResponse(res(200, "text/event-stream"))).toBe(
      "stream",
    );
  });

  it("content-type match is case-insensitive and parameter-tolerant", () => {
    expect(
      classifyQueueSubmitResponse(res(200, "Text/Event-Stream; charset=utf-8")),
    ).toBe("stream");
  });

  it("refusals are errors: 429 queue-full, 5xx", () => {
    expect(classifyQueueSubmitResponse(res(429, "text/plain"))).toBe("error");
    expect(classifyQueueSubmitResponse(res(500, "text/plain"))).toBe("error");
  });

  it("missing content-type defaults to queued (never abandon an accepted ack)", () => {
    expect(classifyQueueSubmitResponse(res(202, null))).toBe("queued");
  });
});
