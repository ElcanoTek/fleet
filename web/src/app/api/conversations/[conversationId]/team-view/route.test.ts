// The team-view proxy must carry the live view's conditional poll both
// ways: If-None-Match goes upstream, and a bodiless 304 comes back with its
// ETag (chatServerPassthrough forwards ETag; a 304 has a null body).

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest, NextResponse } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerPassthrough: async (...args: unknown[]) => {
    const upstream: Response = await chatServerFetchMock(...args);
    const headers = new Headers();
    const etag = upstream.headers.get("ETag");
    if (etag) headers.set("ETag", etag);
    return new NextResponse(upstream.body, { status: upstream.status, headers });
  },
}));

import { GET } from "./route";

const context = { params: Promise.resolve({ conversationId: "conv-1" }) };

describe("GET /api/conversations/[conversationId]/team-view", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "bob@example.com", exp: 0, epoch: "e1" });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("forwards If-None-Match and passes a 304 through with its ETag", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response(null, { status: 304, headers: { ETag: 'W/"tv-1"' } }),
    );
    const req = new NextRequest("https://fleet.example.com/api/conversations/conv-1/team-view", {
      headers: { "If-None-Match": 'W/"tv-1"' },
    });
    const res = await GET(req, context);
    expect(res.status).toBe(304);
    expect(res.headers.get("ETag")).toBe('W/"tv-1"');
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "bob@example.com" }),
      "/conversations/conv-1/team-view",
      { headers: { "If-None-Match": 'W/"tv-1"' } },
    );
  });

  it("sends an unconditional read when the browser has no etag", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response('{"id":"conv-1"}', { status: 200, headers: { ETag: 'W/"tv-2"' } }),
    );
    const req = new NextRequest("https://fleet.example.com/api/conversations/conv-1/team-view");
    const res = await GET(req, context);
    expect(res.status).toBe(200);
    expect(res.headers.get("ETag")).toBe('W/"tv-2"');
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.anything(),
      "/conversations/conv-1/team-view",
      undefined,
    );
  });

  it("returns 401 without a session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const req = new NextRequest("https://fleet.example.com/api/conversations/conv-1/team-view");
    const res = await GET(req, context);
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });
});
