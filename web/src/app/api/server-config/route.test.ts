// Verifies the Next.js server-config proxy passes the chat-server's
// capability flags through to the browser and gates on a session.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextResponse } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
// chatServerProxy is the route's new boundary; the mock replicates its contract
// over chatServerFetchMock (args forwarded verbatim) and returns a clean 502
// when the fetch rejects.
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

import { GET } from "./route";

describe("GET /api/server-config", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0 });
    chatServerFetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          lockdown_available: true,
          lockdown_only: false,
          lockdown_allowed_models: ["openai/gpt-5"],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("passes the upstream capability flags through to the browser", async () => {
    const res = await GET();
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.lockdown_available).toBe(true);
    expect(body.lockdown_only).toBe(false);
    expect(body.lockdown_allowed_models).toEqual(["openai/gpt-5"]);
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET();
    expect(res.status).toBe(401);
  });

  it("forwards upstream errors verbatim", async () => {
    chatServerFetchMock.mockResolvedValue(new Response("upstream boom", { status: 500 }));
    const res = await GET();
    expect(res.status).toBe(500);
    expect(await res.text()).toBe("upstream boom");
  });

  it("returns a clean 502 when chat-server is unreachable", async () => {
    chatServerFetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const res = await GET();
    expect(res.status).toBe(502);
    const body = await res.json();
    expect(body.error).toMatch(/chat-server unreachable/);
  });
});
