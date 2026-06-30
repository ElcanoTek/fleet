import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";
import { verifySessionToken } from "@/app/lib/auth";
import { __resetDiscoveryCacheForTest } from "@/app/lib/oidc";
import { GET } from "./route";

const discovery = {
  issuer: "https://idp.example.com",
  authorization_endpoint: "https://idp.example.com/authorize",
  token_endpoint: "https://idp.example.com/token",
};

function b64url(o: unknown): string {
  return Buffer.from(JSON.stringify(o)).toString("base64url");
}

function idToken(overrides: Record<string, unknown> = {}): string {
  const claims = {
    iss: "https://idp.example.com",
    aud: "client-123",
    exp: Math.floor(Date.now() / 1000) + 600,
    nonce: "the-nonce",
    email: "Alice@Example.com",
    email_verified: true,
    ...overrides,
  };
  return `${b64url({ alg: "RS256" })}.${b64url(claims)}.sig`;
}

// stubFetch routes the discovery GET and the token-endpoint POST. The token
// response carries whatever id_token the test wants to exercise.
function stubFetch(token: string | null) {
  globalThis.fetch = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/.well-known/openid-configuration")) {
      return new Response(JSON.stringify(discovery), { status: 200 });
    }
    if (u === "https://idp.example.com/token") {
      expect(init?.method).toBe("POST");
      if (token === null) return new Response("bad", { status: 400 });
      return new Response(JSON.stringify({ id_token: token, access_token: "at" }), { status: 200 });
    }
    throw new Error(`unexpected fetch: ${u}`);
  }) as unknown as typeof fetch;
}

function callbackReq(params: Record<string, string>, cookies: Record<string, string>) {
  const qs = new URLSearchParams(params).toString();
  const req = new NextRequest(`https://chat.example.com/api/auth/oidc/callback?${qs}`, {
    headers: { "x-forwarded-host": "chat.example.com", "x-forwarded-proto": "https" },
  });
  for (const [k, v] of Object.entries(cookies)) req.cookies.set(k, v);
  return req;
}

const GOOD_COOKIES = {
  fleet_oidc_state: "the-state",
  fleet_oidc_nonce: "the-nonce",
  fleet_oidc_verifier: "the-verifier",
};

describe("GET /api/auth/oidc/callback", () => {
  const original = process.env;

  beforeEach(() => {
    process.env = { ...original };
    process.env.APP_SESSION_SECRET = "test-session-secret-please-ignore";
    process.env.FLEET_OIDC_ISSUER = "https://idp.example.com";
    process.env.FLEET_OIDC_CLIENT_ID = "client-123";
    process.env.FLEET_OIDC_CLIENT_SECRET = "secret-xyz";
    __resetDiscoveryCacheForTest();
  });
  afterEach(() => {
    process.env = original;
    vi.restoreAllMocks();
  });

  it("mints elcano_session for a valid login and redirects home", async () => {
    stubFetch(idToken());
    const res = await GET(callbackReq({ code: "auth-code", state: "the-state" }, GOOD_COOKIES));

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("https://chat.example.com/");

    const session = res.cookies.get("elcano_session");
    expect(session?.value).toBeTruthy();
    const payload = await verifySessionToken(session!.value);
    expect(payload?.email).toBe("alice@example.com");

    // Temp cookies are cleared.
    expect(res.cookies.get("fleet_oidc_state")?.value).toBe("");
    expect(res.cookies.get("fleet_oidc_state")?.maxAge).toBe(0);
  });

  it("rejects a state mismatch (CSRF) without exchanging the code", async () => {
    stubFetch(idToken());
    const res = await GET(callbackReq({ code: "c", state: "attacker-state" }, GOOD_COOKIES));
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_error");
    expect(res.cookies.get("elcano_session")?.value).toBeFalsy();
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it("surfaces a provider error param", async () => {
    stubFetch(idToken());
    const res = await GET(callbackReq({ error: "access_denied", state: "the-state" }, GOOD_COOKIES));
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_denied");
  });

  it("rejects a token whose nonce does not match", async () => {
    stubFetch(idToken({ nonce: "wrong-nonce" }));
    const res = await GET(callbackReq({ code: "c", state: "the-state" }, GOOD_COOKIES));
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_error");
    expect(res.cookies.get("elcano_session")?.value).toBeFalsy();
  });

  it("enforces the email-domain allowlist", async () => {
    process.env.FLEET_OIDC_ALLOWED_DOMAINS = "elcanotek.com";
    stubFetch(idToken({ email: "alice@example.com" }));
    const res = await GET(callbackReq({ code: "c", state: "the-state" }, GOOD_COOKIES));
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_domain");
    expect(res.cookies.get("elcano_session")?.value).toBeFalsy();
  });

  it("admits an allowed domain", async () => {
    process.env.FLEET_OIDC_ALLOWED_DOMAINS = "example.com";
    stubFetch(idToken({ email: "alice@example.com" }));
    const res = await GET(callbackReq({ code: "c", state: "the-state" }, GOOD_COOKIES));
    expect(res.headers.get("location")).toBe("https://chat.example.com/");
    expect(res.cookies.get("elcano_session")?.value).toBeTruthy();
  });

  it("bounces with oidc_error when the token endpoint rejects the code", async () => {
    stubFetch(null);
    const res = await GET(callbackReq({ code: "bad-code", state: "the-state" }, GOOD_COOKIES));
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_error");
  });

  it("bounces to oidc_unavailable when OIDC is not configured", async () => {
    delete process.env.FLEET_OIDC_ISSUER;
    const res = await GET(callbackReq({ code: "c", state: "the-state" }, GOOD_COOKIES));
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_unavailable");
  });
});
