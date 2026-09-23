import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  resetJwksCacheForTests,
  resolveSigningKeys,
  verifyApplicationAccessToken,
  verifyBackchannelLogoutToken,
} from "./backchannelLogout";

const encoder = new TextEncoder();

function b64url(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString("base64url");
}

async function logoutToken(
  pair: CryptoKeyPair,
  overrides: Record<string, unknown> = {},
) {
  const publicKey = new Uint8Array(
    await crypto.subtle.exportKey("raw", pair.publicKey),
  );
  const digest = new Uint8Array(
    await crypto.subtle.digest("SHA-256", publicKey),
  );
  const header = {
    typ: "logout+jwt",
    alg: "EdDSA",
    kid: b64url(digest.slice(0, 16)),
  };
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
    await crypto.subtle.sign(
      { name: "Ed25519" },
      pair.privateKey,
      encoder.encode(body),
    ),
  );
  return {
    raw: `${body}.${b64url(signature)}`,
    publicKey: Buffer.from(publicKey).toString("base64"),
  };
}

async function accessToken(
  pair: CryptoKeyPair,
  overrides: Record<string, unknown> = {},
) {
  const publicKey = new Uint8Array(
    await crypto.subtle.exportKey("raw", pair.publicKey),
  );
  const digest = new Uint8Array(
    await crypto.subtle.digest("SHA-256", publicKey),
  );
  const header = {
    typ: "access+jwt",
    alg: "EdDSA",
    kid: b64url(digest.slice(0, 16)),
  };
  const claims = {
    iss: "https://auth.example.com",
    sub: "account-123",
    aud: "fleet",
    email: "alice@example.com",
    iat: 1_000,
    exp: 1_300,
    jti: "access-7",
    events: {
      "urn:elcanotek:event:application-access": {
        action: "grant",
        version: 7,
        settings: { chat_role: "member", ops_role: "none" },
      },
    },
    ...overrides,
  };
  const body = `${Buffer.from(JSON.stringify(header)).toString("base64url")}.${Buffer.from(
    JSON.stringify(claims),
  ).toString("base64url")}`;
  const signature = new Uint8Array(
    await crypto.subtle.sign(
      { name: "Ed25519" },
      pair.privateKey,
      encoder.encode(body),
    ),
  );
  return {
    raw: `${body}.${b64url(signature)}`,
    publicKey: Buffer.from(publicKey).toString("base64"),
  };
}

describe("verifyBackchannelLogoutToken", () => {
  let pair: CryptoKeyPair;

  beforeEach(async () => {
    pair = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, [
      "sign",
      "verify",
    ])) as CryptoKeyPair;
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
      const publicKey = new Uint8Array(
        await crypto.subtle.exportKey("raw", p.publicKey),
      );
      keys.push({
        kty: "OKP",
        crv: "Ed25519",
        x: Buffer.from(publicKey).toString("base64url"),
      });
    }
    return JSON.stringify({ keys });
  };

  it("verifies a token whose key is only published in Auth's JWKS", async () => {
    const token = await logoutToken(pair);
    const fetchImpl = (async () =>
      new Response(await jwksFor(pair), {
        status: 200,
      })) as unknown as typeof fetch;
    await expect(
      verifyBackchannelLogoutToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        fetchImpl,
      ),
    ).resolves.toMatchObject({ subject: "account-123" });
  });

  it("uses a matching static key without a JWKS request", async () => {
    const token = await logoutToken(pair);
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    let calls = 0;
    const failIfCalled = (async () => {
      calls += 1;
      throw new Error("JWKS should not be fetched for a known key");
    }) as unknown as typeof fetch;
    await expect(
      verifyBackchannelLogoutToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        failIfCalled,
      ),
    ).resolves.toMatchObject({ subject: "account-123" });
    expect(calls).toBe(0);
  });

  it("caches the JWKS and refreshes at most once a minute for unknown kids", async () => {
    let calls = 0;
    const fetchImpl = (async () => {
      calls += 1;
      return new Response(await jwksFor(pair), { status: 200 });
    }) as unknown as typeof fetch;
    await resolveSigningKeys(
      "https://auth.example.com",
      null,
      fetchImpl,
      1_000_000,
    );
    await resolveSigningKeys(
      "https://auth.example.com",
      null,
      fetchImpl,
      1_000_000 + 5_000,
    );
    expect(calls).toBe(1);
    // Unknown kid: one refresh, then rate-limited.
    await resolveSigningKeys(
      "https://auth.example.com",
      "unknown-kid",
      fetchImpl,
      1_000_000 + 70_000,
    );
    await resolveSigningKeys(
      "https://auth.example.com",
      "unknown-kid",
      fetchImpl,
      1_000_000 + 80_000,
    );
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
    const fetchImpl = (async () =>
      new Response(doc, { status: 200 })) as unknown as typeof fetch;
    expect(
      await resolveSigningKeys(
        "https://auth.example.com",
        null,
        fetchImpl,
        2_000_000,
      ),
    ).toEqual([]);
  });

  it("verifies the signature, issuer, audience, event, and replay key", async () => {
    const token = await logoutToken(pair);
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        noNetwork,
      ),
    ).resolves.toMatchObject({
      subject: "account-123",
      eventId: "event-123",
      email: "alice@example.com",
    });
  });

  it("rejects a token minted for another application", async () => {
    const token = await logoutToken(pair, { aud: "lens" });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        noNetwork,
      ),
    ).rejects.toThrow();
  });

  it("rejects malformed email identity data even when it is signed", async () => {
    const token = await logoutToken(pair, { email: "not-an-email" });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        noNetwork,
      ),
    ).rejects.toThrow();
  });

  it("rejects a token whose expiry has passed", async () => {
    const token = await logoutToken(pair, { exp: 1_100 });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_200,
        noNetwork,
      ),
    ).rejects.toThrow();
  });

  it("rejects a token without an expiry", async () => {
    const token = await logoutToken(pair, { exp: undefined });
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;
    await expect(
      verifyBackchannelLogoutToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        noNetwork,
      ),
    ).rejects.toThrow();
  });

  it("verifies versioned application access and its Fleet settings", async () => {
    const token = await accessToken(pair);
    process.env.AUTH_SIGNING_PUBKEY = token.publicKey;

    await expect(
      verifyApplicationAccessToken(
        token.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        noNetwork,
      ),
    ).resolves.toEqual({
      eventId: "access-7",
      subject: "account-123",
      email: "alice@example.com",
      issuer: "https://auth.example.com",
      action: "grant",
      version: 7,
      issuedAt: 1_000,
      settings: { chat_role: "member", ops_role: "none" },
    });
  });

  it("rejects invalid application access versions and settings", async () => {
    const invalidVersion = await accessToken(pair, {
      events: {
        "urn:elcanotek:event:application-access": {
          action: "grant",
          version: true,
        },
      },
    });
    process.env.AUTH_SIGNING_PUBKEY = invalidVersion.publicKey;
    await expect(
      verifyApplicationAccessToken(
        invalidVersion.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        noNetwork,
      ),
    ).rejects.toThrow();

    const invalidSettings = await accessToken(pair, {
      events: {
        "urn:elcanotek:event:application-access": {
          action: "grant",
          version: 8,
          settings: { chat_role: { unexpected: true } },
        },
      },
    });
    await expect(
      verifyApplicationAccessToken(
        invalidSettings.raw,
        "https://auth.example.com",
        "fleet",
        1_010,
        noNetwork,
      ),
    ).rejects.toThrow();
  });
});
