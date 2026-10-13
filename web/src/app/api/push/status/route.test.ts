// Verifies the push status proxy: CSRF + session gating, the owner-scoped
// answer passed through with the endpoint in the body, and the 501 "not
// configured" pass-through.

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

function statusRequest(): NextRequest {
  return new NextRequest("https://fleet.example.com/api/push/status", {
    method: "POST",
    body: JSON.stringify({ endpoint: "https://relay.example/ep" }),
  });
}

describe("POST /api/push/status", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "epoch-1" });
    chatServerFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ subscribed: true }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("asks the chat server as the signed-in user, with the endpoint in the body", async () => {
    const res = await POST(statusRequest());
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ subscribed: true });
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com", epoch: "epoch-1" }),
      "/push/status",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ endpoint: "https://relay.example/ep" }),
      }),
    );
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await POST(statusRequest());
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("short-circuits on a CSRF failure", async () => {
    verifyOriginMock.mockReturnValue({
      ok: false,
      response: NextResponse.json({ error: "bad origin" }, { status: 403 }),
    });
    const res = await POST(statusRequest());
    expect(res.status).toBe(403);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("passes the 501 not-configured error through", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "push_disabled" }), {
        status: 501,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await POST(statusRequest());
    expect(res.status).toBe(501);
  });

  it("returns a clean 502 when chat-server is unreachable", async () => {
    chatServerFetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const res = await POST(statusRequest());
    expect(res.status).toBe(502);
  });
});
