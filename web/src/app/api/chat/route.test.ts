// The chat proxy rebuilds the response header set rather than passing it
// through, so any header the browser needs has to be listed there explicitly.
// X-Fleet-Conversation-Id is one of them (#1591): it is the only id a
// brand-new chat learns when the stream dies before the `conversation` frame,
// and a proxy that quietly drops it puts the recovery chain right back where
// it started — with a pending key no endpoint knows.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();
const verifyOriginMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/csrf", () => ({
  verifyOrigin: (...args: unknown[]) => verifyOriginMock(...args),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
}));

import { POST } from "./route";

const chatRequest = (): NextRequest =>
  new NextRequest("https://fleet.example.com/api/chat", {
    method: "POST",
    body: JSON.stringify({ message: "hi", submission_id: "sub-1" }),
  });

const sseResponse = (headers: Record<string, string>): Response =>
  new Response(new ReadableStream<Uint8Array>({ start: (c) => c.close() }), {
    status: 200,
    headers: { "content-type": "text/event-stream", ...headers },
  });

describe("POST /api/chat", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({
      email: "alice@example.com",
      exp: 0,
      epoch: "e1",
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("forwards the conversation id chat-server named on the response", async () => {
    chatServerFetchMock.mockResolvedValue(
      sseResponse({ "X-Fleet-Conversation-Id": "conv-42" }),
    );

    const res = await POST(chatRequest());

    expect(res.status).toBe(200);
    expect(res.headers.get("X-Fleet-Conversation-Id")).toBe("conv-42");
    expect(res.headers.get("Content-Type")).toBe(
      "text/event-stream; charset=utf-8",
    );
  });

  it("omits the header when chat-server did not send one", async () => {
    chatServerFetchMock.mockResolvedValue(sseResponse({}));

    const res = await POST(chatRequest());

    expect(res.status).toBe(200);
    expect(res.headers.get("X-Fleet-Conversation-Id")).toBeNull();
  });

  // The heartbeat cadence had the same bug as the conversation id and went
  // unnoticed far longer, because dropping it fails SILENTLY: the client reads
  // absent as 0, which means "keepalives are off, silence proves nothing", and
  // checkStreamLiveness's missed-keepalive branch is disabled rather than
  // wrong. Every Next-proxied deployment ran the watchdog in its no-cadence
  // fallback while looking healthy.
  it("forwards the advertised heartbeat cadence", async () => {
    chatServerFetchMock.mockResolvedValue(
      sseResponse({ "X-Fleet-Heartbeat-Interval-Ms": "15000" }),
    );

    const res = await POST(chatRequest());

    expect(res.headers.get("X-Fleet-Heartbeat-Interval-Ms")).toBe("15000");
  });

  // 0 means "keepalives are disabled", which is a real cadence the operator
  // can configure — it must reach the client as 0, not be dropped as falsy.
  it("forwards a heartbeat cadence of 0 rather than dropping it", async () => {
    chatServerFetchMock.mockResolvedValue(
      sseResponse({ "X-Fleet-Heartbeat-Interval-Ms": "0" }),
    );

    const res = await POST(chatRequest());

    expect(res.headers.get("X-Fleet-Heartbeat-Interval-Ms")).toBe("0");
  });

  it("forwards the request body verbatim, submission id included", async () => {
    chatServerFetchMock.mockResolvedValue(sseResponse({}));

    await POST(chatRequest());

    const [, path, init] = chatServerFetchMock.mock.calls[0] as [
      unknown,
      string,
      { body: string },
    ];
    expect(path).toBe("/chat");
    expect(JSON.parse(init.body)).toMatchObject({ submission_id: "sub-1" });
  });
});
