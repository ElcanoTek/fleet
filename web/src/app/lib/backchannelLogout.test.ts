import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { verifyBackchannelLogoutToken } from "./backchannelLogout";

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
  });

  it("verifies the signature, issuer, audience, event, and replay key", async () => {
    const token = await logoutToken(pair);
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010),
    ).resolves.toMatchObject({ subject: "account-123", eventId: "event-123", email: "alice@example.com" });
  });

  it("rejects a token minted for another application", async () => {
    const token = await logoutToken(pair, { aud: "lens" });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010),
    ).rejects.toThrow();
  });

  it("rejects malformed email identity data even when it is signed", async () => {
    const token = await logoutToken(pair, { email: "not-an-email" });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010),
    ).rejects.toThrow();
  });

  it("rejects a token whose expiry has passed", async () => {
    const token = await logoutToken(pair, { exp: 1_100 });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_200),
    ).rejects.toThrow();
  });

  it("rejects a token without an expiry", async () => {
    const token = await logoutToken(pair, { exp: undefined });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(token.raw, "https://auth.example.com", "fleet", 1_010),
    ).rejects.toThrow();
  });
});
