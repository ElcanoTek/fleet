import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

// The login route forwards email+password to chat-server's /auth/verify and
// maps the upstream verdict onto /login?e=<code>. The mapping matters: the
// verify endpoint rate-limits pre-login attempts with a 429 + Retry-After
// (internal/httpapi/auth_verify.go), and folding that into the generic
// "server" bucket told throttled users the chat server was unreachable.
// The success path is exercised end-to-end by the e2e suite (it needs the
// request-scoped cookies() store); these tests pin the error mapping.

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
