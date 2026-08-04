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

// TEST_EPOCH is what the stubbed chat-server reports for /auth/session-epoch —
// the value the callback must stamp into the cookie it mints.
const TEST_EPOCH = "abcdef0123456789";

// stubFetch routes the discovery GET, the token-endpoint POST, and the
// chat-server session-epoch read the mint path performs. The token response
// carries whatever id_token the test wants to exercise; `epochStatus` lets a
// test make chat-server unavailable.
function stubFetch(token: string | null, epochStatus = 200) {
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
    if (u.endsWith("/auth/session-epoch")) {
      if (epochStatus !== 200) return new Response("nope", { status: epochStatus });
      return new Response(JSON.stringify({ session_epoch: TEST_EPOCH }), { status: 200 });
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
    // The mint path reads the session epoch from chat-server, which needs the
    // shared token.
    process.env.CHAT_SERVER_TOKEN = "test-chat-token";
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
    // The epoch chat-server reported is stamped in, so an admin password reset
    // evicts this SSO session too.
    expect(payload?.epoch).toBe(TEST_EPOCH);

    // Temp cookies are cleared.
    expect(res.cookies.get("fleet_oidc_state")?.value).toBe("");
    expect(res.cookies.get("fleet_oidc_state")?.maxAge).toBe(0);
  });

  // A cookie with no epoch claim is refused by verifySessionToken, so minting
  // one when chat-server is unreachable would strand the user in a login loop.
  it("refuses to mint a session when the epoch read fails", async () => {
    stubFetch(idToken(), 503);
    const res = await GET(callbackReq({ code: "auth-code", state: "the-state" }, GOOD_COOKIES));
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_error");
    expect(res.cookies.get("elcano_session")?.value).toBeFalsy();
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
