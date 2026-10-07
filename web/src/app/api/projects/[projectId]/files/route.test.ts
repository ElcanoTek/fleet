// Verifies the project-home Sources proxy exists and forwards verbatim —
// same rationale as the sibling conversations route test: the e2e suite
// mocks this path at the network layer, so only a real-module import
// catches a missing route.

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
const request = new NextRequest("https://fleet.example.com/api/projects/p-growth/files");

describe("GET /api/projects/[projectId]/files", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
    chatServerFetchMock.mockResolvedValue(
      new Response('{"groups":[],"files":[],"truncated":false}', { status: 200 }),
    );
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("proxies to the Go Sources handler and forwards the response", async () => {
    const res = await GET(request, context);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ groups: [], files: [], truncated: false });
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/projects/p-growth/files",
    );
  });

  it("forwards an id-shaped focus to the Go handler", async () => {
    const res = await GET(
      new NextRequest(
        "https://fleet.example.com/api/projects/p-growth/files?focus=0b6c7a1e-1f2d-4c3b-9a8e-7d6c5b4a3f21",
      ),
      context,
    );
    expect(res.status).toBe(200);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "alice@example.com" }),
      "/projects/p-growth/files?focus=0b6c7a1e-1f2d-4c3b-9a8e-7d6c5b4a3f21",
    );
  });

  it("refuses a focus that is not an id, without calling upstream", async () => {
    for (const bad of ["../x", "a b", "x".repeat(65), ""]) {
      chatServerFetchMock.mockClear();
      const res = await GET(
        new NextRequest(
          `https://fleet.example.com/api/projects/p-growth/files?focus=${encodeURIComponent(bad)}`,
        ),
        context,
      );
      expect(res.status).toBe(400);
      expect(chatServerFetchMock).not.toHaveBeenCalled();
    }
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET(request, context);
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });
});
