import { beforeAll, afterEach, describe, expect, it } from "vitest";
import type { NextRequest } from "next/server";

// Exercises the auth lib: the Ed25519 verifier (verifyElcanoToken, mirrors
// auth/internal/token/token.go), the unified two-cookie session resolution
// (getSessionFromRequest), and the auth-service URL/cookie config helpers.
//
// We generate a real Ed25519 keypair, set its public half as
// AUTH_SIGNING_PUBKEY, and sign tokens the same way auth does:
//   base64url(payloadJSON) + "." + base64url(sig over the body STRING).
// The module caches the imported public key on first use, so the whole file
// shares one keypair; that's why the env + dynamic import happen in beforeAll.

const enc = new TextEncoder();
const SECRET = "test-session-secret-which-is-long-enough";

function toBase64Url(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function toStdBase64(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

async function makeToken(priv: CryptoKey, payload: object): Promise<string> {
  const body = toBase64Url(enc.encode(JSON.stringify(payload)));
  const sig = new Uint8Array(await crypto.subtle.sign({ name: "Ed25519" }, priv, enc.encode(body)));
  return `${body}.${toBase64Url(sig)}`;
}

// reqWith builds a minimal stand-in for NextRequest exposing only the cookie
// accessor getSessionFromRequest uses.
function reqWith(cookies: Record<string, string>): NextRequest {
  return {
    cookies: {
      get: (name: string) => (name in cookies ? { value: cookies[name] } : undefined),
    },
  } as unknown as NextRequest;
}

let auth: typeof import("./auth");
let priv: CryptoKey;
const future = Math.floor(Date.now() / 1000) + 3600;

beforeAll(async () => {
  const pair = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, [
    "sign",
    "verify",
  ])) as CryptoKeyPair;
  priv = pair.privateKey;
  const rawPub = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  process.env.AUTH_SIGNING_PUBKEY = toStdBase64(rawPub);
  process.env.APP_SESSION_SECRET = SECRET;

  auth = await import("./auth");
});

afterEach(() => {
  delete process.env.AUTH_LOGIN_URL;
  delete process.env.AUTH_COOKIE_NAME;
});

describe("verifyElcanoToken", () => {
  it("accepts a valid, unexpired token and returns email + tenant", async () => {
    const token = await makeToken(priv, { email: "alice@elcanotek.com", tenant: "elcanotek.com", exp: future });
    const session = await auth.verifyElcanoToken(token);
    expect(session).not.toBeNull();
    expect(session?.email).toBe("alice@elcanotek.com");
    expect(session?.tenant).toBe("elcanotek.com");
  });

  it("rejects a tampered payload (signature no longer matches)", async () => {
    const token = await makeToken(priv, { email: "alice@elcanotek.com", exp: future });
    const tamperedBody = toBase64Url(enc.encode(JSON.stringify({ email: "attacker@evil.com", exp: future })));
    const forged = `${tamperedBody}.${token.split(".")[1]}`;
    expect(await auth.verifyElcanoToken(forged)).toBeNull();
  });

  it("rejects an expired token", async () => {
    const token = await makeToken(priv, { email: "alice@elcanotek.com", exp: Math.floor(Date.now() / 1000) - 1 });
    expect(await auth.verifyElcanoToken(token)).toBeNull();
  });

  it("rejects a token with no email", async () => {
    const token = await makeToken(priv, { tenant: "elcanotek.com", exp: future });
    expect(await auth.verifyElcanoToken(token)).toBeNull();
  });

  it("rejects a token signed by a different key", async () => {
    const other = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
    const token = await makeToken(other.privateKey, { email: "alice@elcanotek.com", exp: future });
    expect(await auth.verifyElcanoToken(token)).toBeNull();
  });

  it("rejects malformed input", async () => {
    expect(await auth.verifyElcanoToken(null)).toBeNull();
    expect(await auth.verifyElcanoToken("")).toBeNull();
    expect(await auth.verifyElcanoToken("no-dot-here")).toBeNull();
    expect(await auth.verifyElcanoToken("garbage.notavalidsig")).toBeNull();
    expect(await auth.verifyElcanoToken(".")).toBeNull();
  });
});

describe("getSessionFromRequest (two-cookie resolution)", () => {
  it("returns a password session for a valid elcano_session (HMAC) cookie", async () => {
    const token = await auth.createSessionToken("bob@x.com");
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: token }));
    expect(session).toMatchObject({ email: "bob@x.com", source: "password" });
  });

  it("returns an elcano session for a valid elcano_auth (Ed25519) cookie", async () => {
    const token = await makeToken(priv, { email: "carol@elcanotek.com", tenant: "elcanotek.com", exp: future });
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getElcanoCookieName()]: token }));
    expect(session).toMatchObject({ email: "carol@elcanotek.com", tenant: "elcanotek.com", source: "elcano" });
  });

  it("prefers the HMAC password cookie when both are present", async () => {
    const hmac = await auth.createSessionToken("bob@x.com");
    const elcano = await makeToken(priv, { email: "carol@elcanotek.com", exp: future });
    const session = await auth.getSessionFromRequest(
      reqWith({ [auth.getSessionCookieName()]: hmac, [auth.getElcanoCookieName()]: elcano }),
    );
    expect(session).toMatchObject({ email: "bob@x.com", source: "password" });
  });

  it("falls back to elcano_auth when the HMAC cookie is invalid", async () => {
    const elcano = await makeToken(priv, { email: "carol@elcanotek.com", exp: future });
    const session = await auth.getSessionFromRequest(
      reqWith({ [auth.getSessionCookieName()]: "garbage.token", [auth.getElcanoCookieName()]: elcano }),
    );
    expect(session).toMatchObject({ email: "carol@elcanotek.com", source: "elcano" });
  });

  it("returns null when neither cookie is valid", async () => {
    expect(await auth.getSessionFromRequest(reqWith({}))).toBeNull();
    expect(
      await auth.getSessionFromRequest(reqWith({ [auth.getElcanoCookieName()]: "bad.token" })),
    ).toBeNull();
  });
});

describe("auth-service URL + cookie config", () => {
  it("defaults the login URL and strips trailing slashes", () => {
    expect(auth.getAuthLoginUrl()).toBe("https://auth.elcanotek.com");
    process.env.AUTH_LOGIN_URL = "http://localhost:9000/";
    expect(auth.getAuthLoginUrl()).toBe("http://localhost:9000");
  });

  it("defaults and overrides the cookie name", () => {
    expect(auth.getElcanoCookieName()).toBe("elcano_auth");
    process.env.AUTH_COOKIE_NAME = "custom_cookie";
    expect(auth.getElcanoCookieName()).toBe("custom_cookie");
  });

  it("builds a login URL with an encoded return_to", () => {
    const url = auth.buildElcanoLoginUrl("https://chat.elcanotek.com/");
    expect(url).toBe("https://auth.elcanotek.com/?return_to=https%3A%2F%2Fchat.elcanotek.com%2F");
  });
});
