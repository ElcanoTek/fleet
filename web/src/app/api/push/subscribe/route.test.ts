// Verifies the push subscribe proxy: CSRF + session gating, the 204 pass-
// through, and the 501 "not configured" pass-through the settings card keys
// its operator hint off (#292).

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest, NextResponse } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();
const verifyOriginMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/csrf", () => ({
  verifyOrigin: (...args: unknown[]) => verifyOriginMock(...args),
}));
// chatServerProxy mock replicates the real contract over chatServerFetchMock
// (args forwarded verbatim; a rejected fetch becomes a clean 502).
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
  chatServerProxy: async (...args: unknown[]) => {
    try {
      return { upstream: await chatServerFetchMock(...args) };
    } catch (err) {
      return {
        error: NextResponse.json(
          { error: `chat-server unreachable: ${(err as Error).message}` },
          { status: 502 },
        ),
      };
    }
  },
}));

import { POST } from "./route";

function subscribeRequest(): NextRequest {
  return new NextRequest("https://fleet.example.com/api/push/subscribe", {
    method: "POST",
    body: JSON.stringify({
      endpoint: "https://relay.example/ep",
      keys: { auth: "a", p256dh: "p" },
    }),
  });
}

describe("POST /api/push/subscribe", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0 });
    chatServerFetchMock.mockResolvedValue(new Response(null, { status: 204 }));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("proxies the subscription and passes the 204 through", async () => {
    const res = await POST(subscribeRequest());
    expect(res.status).toBe(204);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      "alice@example.com",
      "/push/subscribe",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await POST(subscribeRequest());
    expect(res.status).toBe(401);
  });

  it("short-circuits on a CSRF failure", async () => {
    verifyOriginMock.mockReturnValue({
      ok: false,
      response: NextResponse.json({ error: "bad origin" }, { status: 403 }),
    });
    const res = await POST(subscribeRequest());
    expect(res.status).toBe(403);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("passes the 501 not-configured error through for the settings hint", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "push_disabled" }), {
        status: 501,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await POST(subscribeRequest());
    expect(res.status).toBe(501);
    const body = await res.json();
    expect(body.error).toBe("push_disabled");
  });

  it("returns a clean 502 when chat-server is unreachable", async () => {
    chatServerFetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const res = await POST(subscribeRequest());
    expect(res.status).toBe(502);
  });
});
