// The per-person project UI state proxy (ADR-0079): session + CSRF gating and
// a verbatim passthrough, pinned by importing the real route module (the e2e
// suite mocks this path at the network layer).

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

import { GET, PUT } from "./route";

const context = { params: Promise.resolve({ projectId: "p-growth" }) };
const state = '{"kept_personal":false,"has_shared_chat":false,"sources_open":{}}';

describe("/api/projects/[projectId]/my-state", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
    chatServerFetchMock.mockResolvedValue(new Response(state, { status: 200 }));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("GET forwards to the Go handler", async () => {
    const res = await GET(new NextRequest("https://fleet.example.com/api/projects/p-growth/my-state"), context);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe(state);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/projects/p-growth/my-state",
    );
  });

  it("PUT forwards the partial body", async () => {
    const body = '{"sources_open":{"c1":true}}';
    const res = await PUT(
      new NextRequest("https://fleet.example.com/api/projects/p-growth/my-state", { method: "PUT", body }),
      context,
    );
    expect(res.status).toBe(200);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/projects/p-growth/my-state",
      expect.objectContaining({ method: "PUT", body }),
    );
  });

  it("PUT short-circuits on a CSRF failure", async () => {
    verifyOriginMock.mockReturnValue({
      ok: false,
      response: NextResponse.json({ error: "bad origin" }, { status: 403 }),
    });
    const res = await PUT(
      new NextRequest("https://fleet.example.com/api/projects/p-growth/my-state", { method: "PUT", body: "{}" }),
      context,
    );
    expect(res.status).toBe(403);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("returns 401 without a session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET(new NextRequest("https://fleet.example.com/api/projects/p-growth/my-state"), context);
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });
});
