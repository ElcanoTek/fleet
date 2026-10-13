// Verifies the push renew proxy: CSRF + session gating, the owner-scoped
// answer passed through, and the 501 "not configured" pass-through.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest, NextResponse } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();
const verifyOriginMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
  // The real origin check, used by one test, falls back to the Host header.
  getConfiguredOrigin: () => null,
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

const RENEW = {
  old_endpoint: "https://relay.example/old",
  endpoint: "https://relay.example/new",
  keys: { auth: "a", p256dh: "p" },
};

function renewRequest(): NextRequest {
  return new NextRequest("https://fleet.example.com/api/push/renew", {
    method: "POST",
    body: JSON.stringify(RENEW),
  });
}

describe("POST /api/push/renew", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "epoch-1" });
    chatServerFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ renewed: true }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("forwards the renewal as the signed-in user and passes the answer through", async () => {
    const res = await POST(renewRequest());
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ renewed: true });
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com", epoch: "epoch-1" }),
      "/push/renew",
      expect.objectContaining({ method: "POST", body: JSON.stringify(RENEW) }),
    );
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await POST(renewRequest());
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("short-circuits on a CSRF failure", async () => {
    verifyOriginMock.mockReturnValue({
      ok: false,
      response: NextResponse.json({ error: "bad origin" }, { status: 403 }),
    });
    const res = await POST(renewRequest());
    expect(res.status).toBe(403);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("passes the server's Viewer refusal through with its read_only marker", async () => {
    // The browser tells this 403 (not this user's to renew: drop the new
    // subscription) from the origin check's (no answer: retry) by its body.
    chatServerFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "read_only" }), {
        status: 403,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await POST(renewRequest());
    expect(res.status).toBe(403);
    expect(await res.json()).toEqual({ error: "read_only" });
  });

  it("answers an origin-check failure with a 403 that carries no server refusal marker", async () => {
    const { verifyOrigin } = await vi.importActual<typeof import("@/app/lib/csrf")>("@/app/lib/csrf");
    verifyOriginMock.mockImplementation(verifyOrigin);
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const res = await POST(
      new NextRequest("https://fleet.example.com/api/push/renew", {
        method: "POST",
        headers: { origin: "https://elsewhere.example" },
        body: JSON.stringify(RENEW),
      }),
    );
    expect(res.status).toBe(403);
    const { error } = (await res.json()) as { error: string };
    expect(["read_only", "not_a_member"]).not.toContain(error);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("passes the 501 not-configured error through", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "push_disabled" }), {
        status: 501,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await POST(renewRequest());
    expect(res.status).toBe(501);
  });

  it("returns a clean 502 when chat-server is unreachable", async () => {
    chatServerFetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const res = await POST(renewRequest());
    expect(res.status).toBe(502);
  });
});
