// Verifies the conversation re-file proxy: CSRF + session gating and the
// verbatim passthrough. This route (with the two project-home proxies) was
// the gap that left the drag-a-chat-into-a-project flow 404ing in real
// deployments while the network-mocked e2e suite stayed green — so the
// contract here is pinned by a test that imports the actual route module.

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
// chatServerPassthrough mock replicates the real contract over
// chatServerFetchMock (status/body forwarded; a rejected fetch becomes 502).
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
  chatServerPassthrough: async (...args: unknown[]) => {
    try {
      const upstream: Response = await chatServerFetchMock(...args);
      return new NextResponse(upstream.body, { status: upstream.status });
    } catch (err) {
      return NextResponse.json(
        { error: `chat-server unreachable: ${(err as Error).message}` },
        { status: 502 },
      );
    }
  },
}));

import { POST } from "./route";

const context = { params: Promise.resolve({ conversationId: "conv-1" }) };

function refileRequest(body: string): NextRequest {
  return new NextRequest("https://fleet.example.com/api/conversations/conv-1/project", {
    method: "POST",
    body,
  });
}

describe("POST /api/conversations/[conversationId]/project", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
    chatServerFetchMock.mockResolvedValue(new Response(null, { status: 204 }));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("forwards the re-file body and passes the 204 through", async () => {
    const res = await POST(refileRequest('{"project_id":"p-growth"}'), context);
    expect(res.status).toBe(204);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/conversations/conv-1/project",
      expect.objectContaining({ method: "POST", body: '{"project_id":"p-growth"}' }),
    );
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await POST(refileRequest('{"project_id":"p-growth"}'), context);
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("short-circuits on a CSRF failure", async () => {
    verifyOriginMock.mockReturnValue({
      ok: false,
      response: NextResponse.json({ error: "bad origin" }, { status: 403 }),
    });
    const res = await POST(refileRequest('{"project_id":"p-growth"}'), context);
    expect(res.status).toBe(403);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("passes a membership 404 through for the rail's error banner", async () => {
    chatServerFetchMock.mockResolvedValue(new Response("project not found", { status: 404 }));
    const res = await POST(refileRequest('{"project_id":"someone-elses"}'), context);
    expect(res.status).toBe(404);
  });

  it("returns a clean 502 when chat-server is unreachable", async () => {
    chatServerFetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const res = await POST(refileRequest('{"project_id":"p-growth"}'), context);
    expect(res.status).toBe(502);
  });
});
