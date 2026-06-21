// Route-level test for the bug-report endpoint. We import the POST handler
// directly and call it with a fabricated NextRequest. The chat-server export
// is stubbed via the global fetch mock — so is the Jules /sessions call,
// which lets us assert the exact request shape that goes to Jules.

import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const verifyOriginMock = vi.fn();
const getServerSessionMock = vi.fn();

vi.mock("@/app/lib/csrf", () => ({
  verifyOrigin: (...args: unknown[]) => verifyOriginMock(...args),
}));
vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));

// chatServerFetch is implemented as a separate module function we want to
// keep its real env-driven behavior intact for other suites; here we mock it
// because the Go chat-server isn't running under vitest.
const chatServerFetchMock = vi.fn();
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
}));

import { POST } from "./route";

function buildRequest(body: unknown, opts?: { origin?: string }): NextRequest {
  const url = "http://localhost/api/bug-report";
  const init: RequestInit & { duplex?: string } = {
    method: "POST",
    headers: {
      Origin: opts?.origin ?? "http://localhost",
      "Content-Type": "application/json",
    },
    body: typeof body === "string" ? body : JSON.stringify(body),
  };
  // NextRequest extends Request; cast through unknown to satisfy the types.
  return new NextRequest(new Request(url, init));
}

const okExport = () =>
  new Response(
    JSON.stringify({
      conversation: {
        id: "conv-1",
        title: "broken chat",
        persona: "victoria",
        model: "openai/gpt-5",
      },
      history: [
        { role: "user", type: "text", content: { text: "this is broken" } },
        { role: "assistant", type: "text", content: { text: "sorry" } },
      ],
      exported_at: new Date().toISOString(),
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );

describe("POST /api/bug-report", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  const originalFetch = global.fetch;
  const originalEnv = process.env;

  beforeEach(() => {
    process.env = {
      ...originalEnv,
      JULES_API_KEY: "fake-jules-key",
      JULES_API_BASE: "http://jules.test/v1alpha",
      JULES_SOURCE: "sources/github/ElcanoTek/chat",
      JULES_BRANCH: "dev",
      CHAT_SERVER_TOKEN: "shared",
      CHAT_SERVER_URL: "http://chat.test",
    };
    verifyOriginMock.mockReset();
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    fetchMock = vi.fn();
    global.fetch = fetchMock as unknown as typeof fetch;
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0 });
  });

  afterEach(() => {
    global.fetch = originalFetch;
    process.env = originalEnv;
    vi.restoreAllMocks();
  });

  it("rejects bad CSRF up front", async () => {
    verifyOriginMock.mockReturnValue({
      ok: false,
      response: new Response("forbidden", { status: 403 }),
    });
    const res = await POST(buildRequest({ conversationId: "c" }));
    expect(res.status).toBe(403);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("rejects unauthenticated callers with 401", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await POST(buildRequest({ conversationId: "c" }));
    expect(res.status).toBe(401);
  });

  it("rejects missing conversationId with 400", async () => {
    const res = await POST(buildRequest({}));
    expect(res.status).toBe(400);
    const body = await res.json();
    expect(body.error).toMatch(/conversationId/);
  });

  it("returns 503 when JULES_API_KEY is missing", async () => {
    delete process.env.JULES_API_KEY;
    const res = await POST(buildRequest({ conversationId: "c" }));
    expect(res.status).toBe(503);
    const body = await res.json();
    // User-facing error must not mention Jules — the integration is an
    // internal implementation detail. (Operators see the env var name in
    // the operator-facing _template.env, not in the response body.)
    expect(body.error).not.toMatch(/jules/i);
    expect(body.error).toMatch(/bug reporting/i);
  });

  it("propagates 404 when chat-server has no such conversation", async () => {
    chatServerFetchMock.mockResolvedValue(new Response("nope", { status: 404 }));
    const res = await POST(buildRequest({ conversationId: "c" }));
    expect(res.status).toBe(404);
  });

  it("accepts lockdown chats — bug reporting is a user-initiated action that's allowed in every chat type", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          conversation: { id: "conv-1", title: "sealed", lockdown: true },
          history: [{ role: "user", type: "text", content: { text: "broken in lockdown" } }],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({ name: "sessions/x", id: "x" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const res = await POST(buildRequest({ conversationId: "conv-1" }));
    expect(res.status).toBe(200);
    // The Jules HTTP call must HAVE been made even though the chat was lockdown.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("rejects empty conversations with a clear message", async () => {
    chatServerFetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({ conversation: { id: "c" }, history: [] }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const res = await POST(buildRequest({ conversationId: "c" }));
    expect(res.status).toBe(400);
    const body = await res.json();
    expect(body.error).toMatch(/no messages/);
  });

  it("forwards transcript + branch to Jules and returns the session URL on success", async () => {
    chatServerFetchMock.mockResolvedValue(okExport());
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          name: "sessions/jules-session-abc123",
          id: "jules-session-abc123",
          url: "https://jules.google.com/session/jules-session-abc123",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    const res = await POST(buildRequest({ conversationId: "conv-1", note: "hates me" }));
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body).toEqual({
      ok: true,
      sessionName: "sessions/jules-session-abc123",
      sessionId: "jules-session-abc123",
      sessionUrl: "https://jules.google.com/session/jules-session-abc123",
      truncated: false,
      messageCount: 2,
      branch: "dev",
      source: "sources/github/ElcanoTek/chat",
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("http://jules.test/v1alpha/sessions");
    expect(init.method).toBe("POST");
    const headers = new Headers(init.headers);
    expect(headers.get("X-Goog-Api-Key")).toBe("fake-jules-key");
    expect(headers.get("Content-Type")).toBe("application/json");

    const sentBody = JSON.parse(init.body as string);
    expect(sentBody.automationMode).toBe("AUTO_CREATE_PR");
    expect(sentBody.requirePlanApproval).toBe(false);
    expect(sentBody.sourceContext).toEqual({
      source: "sources/github/ElcanoTek/chat",
      githubRepoContext: { startingBranch: "dev" },
    });
    expect(sentBody.title).toMatch(/Bug report:/);
    expect(sentBody.prompt).toContain("Reporter: alice@example.com");
    expect(sentBody.prompt).toContain("hates me");
    expect(sentBody.prompt).toContain("[USER]\nthis is broken");
  });

  it("falls back to a constructed session URL when Jules omits one", async () => {
    chatServerFetchMock.mockResolvedValue(okExport());
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({ name: "sessions/abc999" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const res = await POST(buildRequest({ conversationId: "conv-1" }));
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.sessionUrl).toBe("https://jules.google.com/session/abc999");
    expect(body.sessionId).toBe("abc999");
  });

  it("surfaces a 502 with detail when Jules rejects the request", async () => {
    chatServerFetchMock.mockResolvedValue(okExport());
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "bad source" }), { status: 400 }),
    );

    const res = await POST(buildRequest({ conversationId: "conv-1" }));
    expect(res.status).toBe(502);
    const body = await res.json();
    expect(body.error).toMatch(/bug-report service returned 400/);
    expect(body.error).not.toMatch(/jules/i);
    expect(body.detail).toContain("bad source");
  });

  it("surfaces a 502 when Jules is unreachable", async () => {
    chatServerFetchMock.mockResolvedValue(okExport());
    fetchMock.mockRejectedValue(new Error("ECONNREFUSED"));

    const res = await POST(buildRequest({ conversationId: "conv-1" }));
    expect(res.status).toBe(502);
    const body = await res.json();
    expect(body.error).toMatch(/couldn't reach the bug-report service/);
    expect(body.error).not.toMatch(/jules/i);
    expect(body.detail).toContain("ECONNREFUSED");
  });

  it("aborts the Jules call and surfaces a clear timeout message", async () => {
    chatServerFetchMock.mockResolvedValue(okExport());
    fetchMock.mockImplementation((_url: string, init: RequestInit) => {
      // Reject with an AbortError as soon as the route aborts the signal,
      // mimicking what undici/fetch does when a timeout fires.
      return new Promise((_resolve, reject) => {
        const signal = init.signal as AbortSignal | undefined;
        if (signal) {
          signal.addEventListener("abort", () => {
            const err = new Error("aborted");
            err.name = "AbortError";
            reject(err);
          });
        }
      });
    });
    vi.useFakeTimers();
    try {
      const promise = POST(buildRequest({ conversationId: "conv-1" }));
      // Advance past the 30s timeout so the AbortController fires.
      await vi.advanceTimersByTimeAsync(31_000);
      const res = await promise;
      expect(res.status).toBe(502);
      const body = await res.json();
      expect(body.error).toMatch(/took too long/i);
      expect(body.error).not.toMatch(/jules/i);
    } finally {
      vi.useRealTimers();
    }
  });

  it("caps the reporter note before sending to Jules", async () => {
    chatServerFetchMock.mockResolvedValue(okExport());
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ name: "sessions/x", id: "x" }), { status: 200 }),
    );
    // 100KB note → must be truncated to 4KB before reaching Jules.
    const giantNote = "n".repeat(100_000);
    await POST(buildRequest({ conversationId: "conv-1", note: giantNote }));
    const [, init] = fetchMock.mock.calls[0];
    const sentBody = JSON.parse(init.body as string);
    // Whole prompt must be at most: header + 4KB note + transcript +
    // footer. Easiest invariant to assert: the literal "n".repeat(4001)
    // does NOT appear in the prompt because the cap is 4000.
    expect(sentBody.prompt.includes("n".repeat(4_001))).toBe(false);
    expect(sentBody.prompt).toContain("n".repeat(100));
  });
});
