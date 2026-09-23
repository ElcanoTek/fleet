import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";
import { POST } from "./route";

const encoder = new TextEncoder();

async function fixtureToken(
  kind: "logout" | "access" = "logout",
): Promise<{ raw: string; publicKey: string }> {
  const pair = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
  const publicKey = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  const kid = Buffer.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", publicKey)).slice(0, 16),
  ).toString("base64url");
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString("base64url");
  const body = `${encode({ typ: `${kind}+jwt`, alg: "EdDSA", kid })}.${encode({
    iss: "https://auth.example.com",
    sub: "account-123",
    aud: "fleet",
    email: "alice@example.com",
    iat: Math.floor(Date.now() / 1000),
    exp: Math.floor(Date.now() / 1000) + 300,
    jti: kind === "logout" ? "event-123" : "access-7",
    events:
      kind === "logout"
        ? { "http://schemas.openid.net/event/backchannel-logout": {} }
        : {
            "urn:elcanotek:event:application-access": {
              action: "grant",
              version: 7,
              settings: { chat_role: "member", ops_role: "none" },
            },
          },
  })}`;
  const signature = new Uint8Array(
    await crypto.subtle.sign({ name: "Ed25519" }, pair.privateKey, encoder.encode(body)),
  );
  return {
    raw: `${body}.${Buffer.from(signature).toString("base64url")}`,
    publicKey: Buffer.from(publicKey).toString("base64"),
  };
}

describe("POST /api/auth/backchannel-logout", () => {
  const original = process.env;
  beforeEach(() => {
    process.env = {
      ...original,
      APP_SESSION_SECRET: "test-session-secret",
      CHAT_SERVER_TOKEN: "test-chat-token",
      CHAT_SERVER_URL: "http://chat.example.com",
      FLEET_OIDC_ISSUER: "https://auth.example.com",
      FLEET_OIDC_CLIENT_ID: "fleet",
      FLEET_OIDC_CLIENT_SECRET: "test-client-secret",
    };
  });
  afterEach(() => {
    process.env = original;
    vi.restoreAllMocks();
  });

  it("forwards a verified central subject to Fleet's internal revocation endpoint", async () => {
    const token = await fixtureToken();
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    globalThis.fetch = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
      expect(String(url)).toBe("http://chat.example.com/auth/external-session-revoke");
      expect(new Headers(init?.headers).get("X-User-Email")).toBe("alice@example.com");
      expect(JSON.parse(String(init?.body))).toEqual({
        event_id: "event-123",
        issuer: "https://auth.example.com",
        subject: "account-123",
      });
      return new Response(null, { status: 204 });
    }) as typeof fetch;
    const form = new URLSearchParams({ logout_token: token.raw });
    const encodedForm = form.toString();
    const request = new NextRequest("https://fleet.example.com/api/auth/backchannel-logout", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "Content-Length": String(Buffer.byteLength(encodedForm)),
      },
      body: encodedForm,
    });

    const response = await POST(request);

    expect(response.status).toBe(204);
  });

  it("forwards a verified desired access grant to Fleet's internal provisioning endpoint", async () => {
    const token = await fixtureToken("access");
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    globalThis.fetch = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
      expect(String(url)).toBe("http://chat.example.com/auth/external-access");
      expect(new Headers(init?.headers).get("X-User-Email")).toBe("alice@example.com");
      expect(JSON.parse(String(init?.body))).toEqual({
        event_id: "access-7",
        issuer: "https://auth.example.com",
        subject: "account-123",
        action: "grant",
        version: 7,
        issued_at: expect.any(Number),
        settings: { chat_role: "member", ops_role: "none" },
      });
      return new Response(null, { status: 204 });
    }) as typeof fetch;
    const form = new URLSearchParams({ access_token: token.raw });
    const encodedForm = form.toString();
    const request = new NextRequest("https://fleet.example.com/api/auth/backchannel-logout", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "Content-Length": String(Buffer.byteLength(encodedForm)),
      },
      body: encodedForm,
    });

    const response = await POST(request);

    expect(response.status).toBe(204);
  });

  it("rejects ambiguous requests carrying both token types", async () => {
    const token = await fixtureToken();
    const form = new URLSearchParams({ logout_token: token.raw, access_token: token.raw });
    const encodedForm = form.toString();
    const request = new NextRequest("https://fleet.example.com/api/auth/backchannel-logout", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "Content-Length": String(Buffer.byteLength(encodedForm)),
      },
      body: encodedForm,
    });

    expect((await POST(request)).status).toBe(400);
  });

  it("rejects an oversized public request before parsing it", async () => {
    const request = new NextRequest("https://fleet.example.com/api/auth/backchannel-logout", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "Content-Length": "20001",
      },
      body: "logout_token=x",
    });

    const response = await POST(request);

    expect(response.status).toBe(413);
  });
});
