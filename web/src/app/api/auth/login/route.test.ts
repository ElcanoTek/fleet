import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

// The login route forwards email+password to chat-server's /auth/verify and
// maps the upstream verdict onto /login?e=<code>. The mapping matters: the
// verify endpoint rate-limits pre-login attempts with a 429 + Retry-After
// (internal/httpapi/auth_verify.go), and folding that into the generic
// "server" bucket told throttled users the chat server was unreachable.
//
// The success path is covered here too, over the REAL fetchSessionEpoch and
// createSessionToken. Nothing else executes it: the mocked e2e suite intercepts
// POST /api/auth/login wholesale, and the live suite never runs in CI — so a
// wrong endpoint path, a renamed response key, or a chat-server predating
// GET /auth/session-epoch would turn every login into the generic "server"
// error with no test noticing.

const cookieSet = vi.fn();
vi.mock("next/headers", () => ({
  cookies: () => Promise.resolve({ set: (options: unknown) => cookieSet(options) }),
}));

import { getSessionCookieName, verifySessionToken } from "@/app/lib/auth";
import { POST } from "./route";

const ORIGIN = "https://chat.example.com";

function loginReq(fields: Record<string, string>): NextRequest {
  const form = new URLSearchParams(fields);
  return new NextRequest(`${ORIGIN}/api/auth/login`, {
    method: "POST",
    headers: {
      origin: ORIGIN,
      "content-type": "application/x-www-form-urlencoded",
    },
    body: form.toString(),
  });
}

function stubVerify(status: number, body: unknown) {
  globalThis.fetch = vi.fn(async () => new Response(JSON.stringify(body), { status })) as unknown as typeof fetch;
}

describe("POST /api/auth/login — upstream error mapping", () => {
  const originalEnv = process.env;
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    process.env = { ...originalEnv };
    process.env.CHAT_SERVER_URL = "http://127.0.0.1:8080";
    process.env.CHAT_SERVER_TOKEN = "test-token";
    delete process.env.NEXT_PUBLIC_PUBLIC_ORIGIN;
  });

  afterEach(() => {
    process.env = originalEnv;
    globalThis.fetch = originalFetch;
  });

  it("maps a 429 (rate-limited) verify to the throttle message, not 'server'", async () => {
    stubVerify(429, { ok: false, error: "too many attempts — try again in a minute" });
    const res = await POST(loginReq({ email: "alice@example.com", password: "pw" }));
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(`${ORIGIN}/login?e=throttled`);
  });

  it("maps an upstream 5xx to the 'server' code", async () => {
    stubVerify(500, { ok: false });
    const res = await POST(loginReq({ email: "alice@example.com", password: "pw" }));
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(`${ORIGIN}/login?e=server`);
  });

  it("maps a connection failure to the 'server' code", async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new Error("ECONNREFUSED");
    }) as unknown as typeof fetch;
    const res = await POST(loginReq({ email: "alice@example.com", password: "pw" }));
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(`${ORIGIN}/login?e=server`);
  });

  it("maps rejected credentials to the generic 'invalid' code", async () => {
    stubVerify(200, { ok: false, error: "invalid credentials" });
    const res = await POST(loginReq({ email: "alice@example.com", password: "pw" }));
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(`${ORIGIN}/login?e=invalid`);
  });

  it("rejects a cross-origin POST without contacting chat-server", async () => {
    const fetchSpy = vi.fn();
    globalThis.fetch = fetchSpy as unknown as typeof fetch;
    const form = new URLSearchParams({ email: "alice@example.com", password: "pw" });
    const res = await POST(
      new NextRequest(`${ORIGIN}/api/auth/login`, {
        method: "POST",
        headers: {
          origin: "https://evil.example.com",
          "content-type": "application/x-www-form-urlencoded",
        },
        body: form.toString(),
      }),
    );
    expect(res.status).toBe(403);
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

// The mint path: verify says yes, the route reads the account's session epoch
// from chat-server and stamps it into the cookie it sets. A cookie minted
// without the epoch is refused on the very next request, so this is the one
// place the two halves have to agree.
describe("POST /api/auth/login — session mint", () => {
  const originalEnv = process.env;
  const originalFetch = globalThis.fetch;
  let seen: { url: string; method: string; email: string | null }[];

  // Answers /auth/verify with the given verdict and /auth/session-epoch with
  // `epoch`. Any OTHER path throws — the route asking for the wrong endpoint is
  // exactly the failure this suite exists to catch.
  function stubChatServer(epoch: Response) {
    seen = [];
    globalThis.fetch = vi.fn(async (url: string | URL, init?: RequestInit) => {
      const path = String(url);
      const headers = new Headers(init?.headers);
      seen.push({ url: path, method: init?.method ?? "GET", email: headers.get("X-User-Email") });
      if (path.endsWith("/auth/verify")) return new Response(JSON.stringify({ ok: true }), { status: 200 });
      if (path.endsWith("/auth/session-epoch")) return epoch;
      throw new Error(`unexpected chat-server call: ${path}`);
    }) as unknown as typeof fetch;
  }

  beforeEach(() => {
    process.env = { ...originalEnv };
    process.env.CHAT_SERVER_URL = "http://127.0.0.1:8080";
    process.env.CHAT_SERVER_TOKEN = "test-token";
    process.env.APP_SESSION_SECRET = "test-session-secret";
    delete process.env.NEXT_PUBLIC_PUBLIC_ORIGIN;
    cookieSet.mockReset();
  });

  afterEach(() => {
    process.env = originalEnv;
    globalThis.fetch = originalFetch;
  });

  it("mints a cookie carrying the epoch chat-server reports", async () => {
    stubChatServer(new Response(JSON.stringify({ session_epoch: "abcdef0123456789" }), { status: 200 }));

    const res = await POST(loginReq({ email: "Alice@Example.com", password: "pw" }));
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(`${ORIGIN}/`);

    const epochCall = seen.find((c) => c.url.endsWith("/auth/session-epoch"));
    expect(epochCall).toEqual({
      url: "http://127.0.0.1:8080/auth/session-epoch",
      method: "GET",
      email: "alice@example.com",
    });

    expect(cookieSet).toHaveBeenCalledTimes(1);
    const cookie = cookieSet.mock.calls[0][0] as { name: string; value: string; httpOnly: boolean };
    expect(cookie.name).toBe(getSessionCookieName());
    expect(cookie.httpOnly).toBe(true);
    expect(await verifySessionToken(cookie.value)).toMatchObject({
      email: "alice@example.com",
      epoch: "abcdef0123456789",
    });
  });

  // A chat-server that predates GET /auth/session-epoch answers 404. Minting a
  // claimless cookie would strand the user in a login loop, so the login fails
  // outright — version skew is an outage, not a silent downgrade.
  it("refuses to mint when chat-server cannot serve the epoch", async () => {
    stubChatServer(new Response("not found", { status: 404 }));

    const res = await POST(loginReq({ email: "alice@example.com", password: "pw" }));
    expect(res.headers.get("location")).toBe(`${ORIGIN}/login?e=server`);
    expect(cookieSet).not.toHaveBeenCalled();
  });

  // Same verdict when the endpoint answers but the payload does not carry the
  // key the route reads.
  it("refuses to mint when the epoch response is empty", async () => {
    stubChatServer(new Response(JSON.stringify({ epoch: "wrong-key" }), { status: 200 }));

    const res = await POST(loginReq({ email: "alice@example.com", password: "pw" }));
    expect(res.headers.get("location")).toBe(`${ORIGIN}/login?e=server`);
    expect(cookieSet).not.toHaveBeenCalled();
  });
});
