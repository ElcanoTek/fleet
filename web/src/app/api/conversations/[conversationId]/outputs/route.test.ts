// The outputs proxies (ADR-0079): the owner's listing (GET) and the per-file
// toggle (POST outputs/share), plus the team-link status read. Each is a thin
// passthrough; the test imports the real modules so a missing route fails
// here rather than as a silent 404 in production.

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
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
  chatServerPassthrough: async (...args: unknown[]) => {
    const upstream: Response = await chatServerFetchMock(...args);
    return new NextResponse(upstream.body, { status: upstream.status });
  },
}));

import { GET } from "./route";
import { POST } from "./share/route";
import { GET as GET_TEAM_LINK } from "../team-link/route";

const context = { params: Promise.resolve({ conversationId: "conv-1" }) };
const body = '{"outputs":[],"total":0,"shared_count":0,"team_visible":false}';

describe("conversation outputs proxies", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
    chatServerFetchMock.mockResolvedValue(new Response(body, { status: 200 }));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("GET outputs forwards to the Go handler", async () => {
    const res = await GET(new NextRequest("https://fleet.example.com/api/conversations/conv-1/outputs"), context);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe(body);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/conversations/conv-1/outputs",
    );
  });

  it("POST outputs/share forwards the toggle", async () => {
    const toggle = '{"path":"out/a.csv","shared":false}';
    const res = await POST(
      new NextRequest("https://fleet.example.com/api/conversations/conv-1/outputs/share", { method: "POST", body: toggle }),
      context,
    );
    expect(res.status).toBe(200);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/conversations/conv-1/outputs/share",
      expect.objectContaining({ method: "POST", body: toggle }),
    );
  });

  it("POST outputs/share short-circuits on a CSRF failure", async () => {
    verifyOriginMock.mockReturnValue({
      ok: false,
      response: NextResponse.json({ error: "bad origin" }, { status: 403 }),
    });
    const res = await POST(
      new NextRequest("https://fleet.example.com/api/conversations/conv-1/outputs/share", { method: "POST", body: "{}" }),
      context,
    );
    expect(res.status).toBe(403);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("GET team-link forwards to the Go handler", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response('{"status":"open","viewer_email":"alice@example.com"}', { status: 200 }),
    );
    const res = await GET_TEAM_LINK(
      new NextRequest("https://fleet.example.com/api/conversations/conv-1/team-link"),
      context,
    );
    expect(res.status).toBe(200);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/conversations/conv-1/team-link",
    );
  });

  it("returns 401 without a session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET(new NextRequest("https://fleet.example.com/api/conversations/conv-1/outputs"), context);
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });
});
