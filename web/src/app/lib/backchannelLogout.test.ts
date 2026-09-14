import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { resetJwksCacheForTests, resolveSigningKeys, verifyBackchannelLogoutToken } from "./backchannelLogout";

const encoder = new TextEncoder();

function b64url(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString("base64url");
}

async function logoutToken(pair: CryptoKeyPair, overrides: Record<string, unknown> = {}) {
  const publicKey = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", publicKey));
  const header = { typ: "logout+jwt", alg: "EdDSA", kid: b64url(digest.slice(0, 16)) };
  const claims = {
    iss: "https://auth.example.com",
    sub: "account-123",
    aud: "fleet",
    email: "alice@example.com",
    iat: 1_000,
    exp: 1_300,
    jti: "event-123",
    events: { "http://schemas.openid.net/event/backchannel-logout": {} },
    ...overrides,
  };
  const body = `${Buffer.from(JSON.stringify(header)).toString("base64url")}.${Buffer.from(
    JSON.stringify(claims),
  ).toString("base64url")}`;
  const signature = new Uint8Array(
    await crypto.subtle.sign({ name: "Ed25519" }, pair.privateKey, encoder.encode(body)),
  );
  return { raw: `${body}.${b64url(signature)}`, publicKey: Buffer.from(publicKey).toString("base64") };
}

describe("verifyBackchannelLogoutToken", () => {
  let pair: CryptoKeyPair;

  beforeEach(async () => {
    pair = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
  });

  afterEach(() => {
    delete process.env.AUTH_SIGNING_PUBKEY;
    delete process.env.AUTH_SIGNING_PREVIOUS_PUBKEYS;
    resetJwksCacheForTests();
  });

  const noNetwork = (async () => {
    throw new Error("no network in tests");
  }) as unknown as typeof fetch;

  const jwksFor = async (...pairs: CryptoKeyPair[]) => {
    const keys = [];
    for (const p of pairs) {
      const publicKey = new Uint8Array(await crypto.subtle.exportKey("raw", p.publicKey));
      keys.push({ kty: "OKP", crv: "Ed25519", x: Buffer.from(publicKey).toString("base64url") });
    }
    return JSON.stringify({ keys });
  };

  it("verifies a token whose key is only published in Auth's JWKS", async () => {
    const token = await logoutToken(pair);
    const fetchImpl = (async () => new Response(await jwksFor(pair), { status: 200 })) as unknown as typeof fetch;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010, fetchImpl),
    ).resolves.toMatchObject({ subject: "account-123" });
  });

  it("falls back to static env keys when the JWKS fetch fails", async () => {
    const token = await logoutToken(pair);
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010, noNetwork),
    ).resolves.toMatchObject({ subject: "account-123" });
  });

  it("caches the JWKS and refreshes at most once a minute for unknown kids", async () => {
    let calls = 0;
    const fetchImpl = (async () => {
      calls += 1;
      return new Response(await jwksFor(pair), { status: 200 });
    }) as unknown as typeof fetch;
    await resolveSigningKeys("https://auth.example.com", null, fetchImpl, 1_000_000);
    await resolveSigningKeys("https://auth.example.com", null, fetchImpl, 1_000_000 + 5_000);
    expect(calls).toBe(1);
    // Unknown kid: one refresh, then rate-limited.
    await resolveSigningKeys("https://auth.example.com", "unknown-kid", fetchImpl, 1_000_000 + 70_000);
    await resolveSigningKeys("https://auth.example.com", "unknown-kid", fetchImpl, 1_000_000 + 80_000);
    expect(calls).toBe(2);
  });

  it("ignores malformed JWKS entries", async () => {
    const doc = JSON.stringify({
      keys: [
        { kty: "RSA", n: "x", e: "AQAB" },
        { kty: "OKP", crv: "Ed25519", x: "dG9vLXNob3J0" },
        "not-a-key",
      ],
    });
    const fetchImpl = (async () => new Response(doc, { status: 200 })) as unknown as typeof fetch;
    expect(await resolveSigningKeys("https://auth.example.com", null, fetchImpl, 2_000_000)).toEqual([]);
  });

  it("verifies the signature, issuer, audience, event, and replay key", async () => {
    const token = await logoutToken(pair);
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010, noNetwork),
    ).resolves.toMatchObject({ subject: "account-123", eventId: "event-123", email: "alice@example.com" });
  });

  it("rejects a token minted for another application", async () => {
    const token = await logoutToken(pair, { aud: "lens" });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010, noNetwork),
    ).rejects.toThrow();
  });

  it("rejects malformed email identity data even when it is signed", async () => {
    const token = await logoutToken(pair, { email: "not-an-email" });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010, noNetwork),
    ).rejects.toThrow();
  });

  it("rejects a token whose expiry has passed", async () => {
    const token = await logoutToken(pair, { exp: 1_100 });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_200, noNetwork),
    ).rejects.toThrow();
  });

  it("rejects a token without an expiry", async () => {
    const token = await logoutToken(pair, { exp: undefined });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010, noNetwork),
    ).rejects.toThrow();
  });
});
