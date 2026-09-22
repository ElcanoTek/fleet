// The attach proxy rebuilds the response header set rather than passing it
// through, so a header chat-server adds reaches the browser only if it is
// listed. X-Fleet-Heartbeat-Interval-Ms is the one that matters most here and
// is the one that was missing: useTurnStream sizes the stream-liveness
// watchdog from it, and absent reads as 0 — "keepalives are off, silence
// proves nothing" — which pins streamDeadSilenceMs at Infinity and disables
// the missed-keepalive branch entirely. The failure is silent, which is why it
// survived: the watchdog still runs, just permanently in its weakest mode.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
}));

import { GET } from "./route";

const streamRequest = (): NextRequest =>
  new NextRequest("https://fleet.example.com/api/conversations/c1/stream");

const context = { params: Promise.resolve({ conversationId: "c1" }) };

const sseResponse = (headers: Record<string, string>): Response =>
  new Response(new ReadableStream<Uint8Array>({ start: (c) => c.close() }), {
    status: 200,
    headers: { "content-type": "text/event-stream", ...headers },
  });

describe("GET /api/conversations/[id]/stream", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({
      email: "alice@example.com",
      exp: 0,
      epoch: "e1",
    });
  });

  it("forwards the advertised heartbeat cadence", async () => {
    chatServerFetchMock.mockResolvedValue(
      sseResponse({ "X-Fleet-Heartbeat-Interval-Ms": "15000" }),
    );

    const res = await GET(streamRequest(), context);

    expect(res.status).toBe(200);
    expect(res.headers.get("X-Fleet-Heartbeat-Interval-Ms")).toBe("15000");
    expect(res.headers.get("Content-Type")).toBe(
      "text/event-stream; charset=utf-8",
    );
  });

  it("forwards a heartbeat cadence of 0 rather than dropping it", async () => {
    chatServerFetchMock.mockResolvedValue(
      sseResponse({ "X-Fleet-Heartbeat-Interval-Ms": "0" }),
    );

    const res = await GET(streamRequest(), context);

    expect(res.headers.get("X-Fleet-Heartbeat-Interval-Ms")).toBe("0");
  });

  it("omits the header when chat-server did not send one", async () => {
    chatServerFetchMock.mockResolvedValue(sseResponse({}));

    const res = await GET(streamRequest(), context);

    expect(res.headers.get("X-Fleet-Heartbeat-Interval-Ms")).toBeNull();
  });
});
