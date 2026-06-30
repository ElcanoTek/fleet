import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";
import { __resetDiscoveryCacheForTest } from "@/app/lib/oidc";
import { GET } from "./route";

const discovery = {
  issuer: "https://idp.example.com",
  authorization_endpoint: "https://idp.example.com/authorize",
  token_endpoint: "https://idp.example.com/token",
};

function startReq() {
  return new NextRequest("https://chat.example.com/api/auth/oidc/start", {
    headers: { "x-forwarded-host": "chat.example.com", "x-forwarded-proto": "https" },
  });
}

describe("GET /api/auth/oidc/start", () => {
  const original = process.env;

  beforeEach(() => {
    process.env = { ...original };
    __resetDiscoveryCacheForTest();
    globalThis.fetch = vi.fn(async () =>
      new Response(JSON.stringify(discovery), { status: 200, headers: { "Content-Type": "application/json" } }),
    ) as unknown as typeof fetch;
  });
  afterEach(() => {
    process.env = original;
    vi.restoreAllMocks();
  });

  it("bounces to the password login when OIDC is not configured", async () => {
    const res = await GET(startReq());
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_unavailable");
  });

  it("redirects to the IdP authorize endpoint with code+state+nonce+PKCE and sets temp cookies", async () => {
    process.env.FLEET_OIDC_ISSUER = "https://idp.example.com";
    process.env.FLEET_OIDC_CLIENT_ID = "client-123";
    process.env.FLEET_OIDC_CLIENT_SECRET = "secret-xyz";

    const res = await GET(startReq());
    expect(res.status).toBe(303);
    const loc = new URL(res.headers.get("location")!);
    expect(`${loc.origin}${loc.pathname}`).toBe("https://idp.example.com/authorize");
    expect(loc.searchParams.get("response_type")).toBe("code");
    expect(loc.searchParams.get("client_id")).toBe("client-123");
    expect(loc.searchParams.get("redirect_uri")).toBe("https://chat.example.com/api/auth/oidc/callback");
    expect(loc.searchParams.get("scope")).toBe("openid email profile");
    expect(loc.searchParams.get("code_challenge_method")).toBe("S256");
    expect(loc.searchParams.get("code_challenge")).toBeTruthy();

    const state = loc.searchParams.get("state")!;
    const nonce = loc.searchParams.get("nonce")!;
    expect(state).toBeTruthy();
    expect(nonce).toBeTruthy();
    // The state + nonce + verifier are persisted to httpOnly cookies for the callback.
    expect(res.cookies.get("fleet_oidc_state")?.value).toBe(state);
    expect(res.cookies.get("fleet_oidc_nonce")?.value).toBe(nonce);
    expect(res.cookies.get("fleet_oidc_verifier")?.value).toBeTruthy();
    expect(res.cookies.get("fleet_oidc_state")?.httpOnly).toBe(true);
  });

  it("bounces with oidc_error when discovery fails", async () => {
    process.env.FLEET_OIDC_ISSUER = "https://idp.example.com";
    process.env.FLEET_OIDC_CLIENT_ID = "client-123";
    process.env.FLEET_OIDC_CLIENT_SECRET = "secret-xyz";
    globalThis.fetch = vi.fn(async () => new Response("boom", { status: 500 })) as unknown as typeof fetch;

    const res = await GET(startReq());
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("https://chat.example.com/login?e=oidc_error");
  });
});
