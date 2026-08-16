// Verifies the project-home chat-list proxy exists and forwards verbatim —
// the e2e suite mocks this path at the network layer, so only a test that
// imports the real route module catches the route going missing again.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest, NextResponse } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
  chatServerPassthrough: async (...args: unknown[]) => {
    const upstream: Response = await chatServerFetchMock(...args);
    return new NextResponse(upstream.body, { status: upstream.status });
  },
}));

import { GET } from "./route";

const context = { params: Promise.resolve({ projectId: "p-growth" }) };
const request = new NextRequest("https://fleet.example.com/api/projects/p-growth/conversations");

describe("GET /api/projects/[projectId]/conversations", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
    chatServerFetchMock.mockResolvedValue(
      new Response('{"conversations":[]}', { status: 200 }),
    );
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("proxies to the Go chat-list handler and forwards the response", async () => {
    const res = await GET(request, context);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ conversations: [] });
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/projects/p-growth/conversations",
    );
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET(request, context);
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });
});
