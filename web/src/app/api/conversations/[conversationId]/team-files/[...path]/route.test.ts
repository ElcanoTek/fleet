// The teammate file download proxy (ADR-0079) — the first cross-user file
// read. The Go handler owns every gate; what this pins is the proxy's own
// half: session gating, the encoded upstream path, verbatim 404s, and the
// hardening headers re-applied to bytes another user's agent wrote.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
}));

import { GET, HEAD } from "./route";

const request = new NextRequest("https://fleet.example.com/api/conversations/conv-1/team-files/out/x");
const ctx = (path: string[]) => ({
  params: Promise.resolve({ conversationId: "conv-1", path }),
});

describe("GET /api/conversations/[conversationId]/team-files/[...path]", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "bob@example.com", exp: 0, epoch: "e1" });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("re-encodes each segment and streams a shared file with hardening headers", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response("a,b\n", { status: 200, headers: { "Content-Type": "text/csv" } }),
    );
    const res = await GET(request, ctx(["out", "Q3 report.csv"]));
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("a,b\n");
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.objectContaining({ email: "bob@example.com" }),
      "/conversations/conv-1/team-files/out/Q3%20report.csv",
      { method: "GET" },
    );
    expect(res.headers.get("X-Content-Type-Options")).toBe("nosniff");
    expect(res.headers.get("Content-Security-Policy")).toBe("sandbox");
    expect(res.headers.get("Content-Disposition")).toBeNull();
  });

  it("forces active document types to download", async () => {
    for (const type of ["text/html; charset=utf-8", "image/svg+xml", "application/xml"]) {
      chatServerFetchMock.mockResolvedValue(
        new Response("<x/>", { status: 200, headers: { "Content-Type": type } }),
      );
      const res = await GET(request, ctx(["page.html"]));
      expect(res.headers.get("Content-Disposition")).toBe(
        "attachment; filename*=UTF-8''page.html",
      );
    }
  });

  it("passes the gate's 404 through untouched", async () => {
    chatServerFetchMock.mockResolvedValue(new Response("not found\n", { status: 404 }));
    const res = await GET(request, ctx(["held.json"]));
    expect(res.status).toBe(404);
  });

  it("returns 401 without a session and never calls upstream", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET(request, ctx(["out", "x.csv"]));
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("returns a clean 502 when chat-server is unreachable", async () => {
    chatServerFetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const res = await GET(request, ctx(["x.csv"]));
    expect(res.status).toBe(502);
  });

  it("forwards HEAD as a HEAD and answers the headers with no body", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response(null, {
        status: 200,
        headers: { "Content-Type": "image/svg+xml", "Content-Length": "42" },
      }),
    );
    const res = await HEAD(request, ctx(["out", "chart.svg"]));
    expect(res.status).toBe(200);
    expect(chatServerFetchMock).toHaveBeenCalledWith(
      expect.anything(),
      "/conversations/conv-1/team-files/out/chart.svg",
      { method: "HEAD" },
    );
    expect(res.body).toBeNull();
    expect(res.headers.get("Content-Length")).toBe("42");
    expect(res.headers.get("X-Content-Type-Options")).toBe("nosniff");
    expect(res.headers.get("Content-Disposition")).toBe(
      "attachment; filename*=UTF-8''chart.svg",
    );

    chatServerFetchMock.mockResolvedValue(new Response(null, { status: 404 }));
    const miss = await HEAD(request, ctx(["held.json"]));
    expect(miss.status).toBe(404);
  });
});
