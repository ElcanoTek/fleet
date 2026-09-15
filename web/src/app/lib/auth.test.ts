import { beforeAll, afterEach, describe, expect, it, vi } from "vitest";
import { NextResponse } from "next/server";
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
  delete process.env.NEXT_PUBLIC_PUBLIC_ORIGIN;
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
    const token = await auth.createSessionToken("bob@x.com", "epoch-1");
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: token }));
    expect(session).toMatchObject({ email: "bob@x.com", source: "password" });
  });

  it("returns an elcano session for a valid elcano_auth (Ed25519) cookie", async () => {
    const token = await makeToken(priv, { email: "carol@elcanotek.com", tenant: "elcanotek.com", exp: future });
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getElcanoCookieName()]: token }));
    expect(session).toMatchObject({ email: "carol@elcanotek.com", tenant: "elcanotek.com", source: "elcano" });
  });

  it("prefers the HMAC password cookie when both are present", async () => {
    const hmac = await auth.createSessionToken("bob@x.com", "epoch-1");
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

// The session epoch is the per-user revocation generation chat-server compares
// against the users table (internal/httpapi/auth.go#headerSessionEpoch). This
// tier cannot check whether the claim is CURRENT — that needs the database — so
// its job is narrower and load-bearing: carry the claim through, and refuse a
// cookie that has none, because a claimless cookie is admitted by the Go gate.
describe("session epoch claim", () => {
  // mintHmac signs an arbitrary payload with the session secret, standing in for
  // a cookie minted by a build that predates the epoch claim.
  async function mintHmac(payload: object): Promise<string> {
    const key = await crypto.subtle.importKey(
      "raw",
      enc.encode(SECRET),
      { name: "HMAC", hash: "SHA-256" },
      false,
      ["sign"],
    );
    const body = toBase64Url(enc.encode(JSON.stringify(payload)));
    const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, enc.encode(body)));
    return `${body}.${toBase64Url(sig)}`;
  }

  it("round-trips the epoch a token was minted with", async () => {
    const token = await auth.createSessionToken("dave@x.com", "abcdef0123456789");
    expect(await auth.verifySessionToken(token)).toMatchObject({ epoch: "abcdef0123456789" });
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: token }));
    expect(session).toMatchObject({ email: "dave@x.com", epoch: "abcdef0123456789" });
  });

  it("keeps central OIDC identity metadata distinct from password sessions", async () => {
    const token = await auth.createOidcSessionToken(
      "dave@x.com",
      "external-epoch",
      "https://auth.example.com",
      "account-123",
    );
    expect(await auth.verifySessionToken(token)).toMatchObject({
      email: "dave@x.com",
      epoch: "external-epoch",
      source: "oidc",
      issuer: "https://auth.example.com",
      subject: "account-123",
    });
  });

  it("refuses a correctly-signed token that carries no epoch claim", async () => {
    const legacy = await mintHmac({ email: "dave@x.com", exp: future });
    expect(await auth.verifySessionToken(legacy)).toBeNull();
    expect(
      await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: legacy })),
    ).toBeNull();
  });

  it("refuses an empty epoch claim", async () => {
    const empty = await mintHmac({ email: "dave@x.com", exp: future, epoch: "" });
    expect(await auth.verifySessionToken(empty)).toBeNull();
  });

  it("leaves elcano_auth sessions epoch-less — that cookie is the auth service's", async () => {
    const token = await makeToken(priv, { email: "carol@elcanotek.com", exp: future });
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getElcanoCookieName()]: token }));
    expect(session?.source).toBe("elcano");
    expect(session?.epoch).toBeUndefined();
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

// x-forwarded-* is client-supplied unless a proxy overwrites it, so a request
// that reaches `next start` directly (bypassing Caddy) can claim any host and
// protocol. When the deployment's canonical origin is configured
// (NEXT_PUBLIC_PUBLIC_ORIGIN — bootstrap writes it for every deploy), redirect
// targets and the Secure-cookie decision must come from it, not the headers.
// Without it (local dev over plain http) the header fallback still applies.
describe("getRedirectUrl / isSecureRequest — canonical origin vs forwarded headers", () => {
  // reqTo builds a minimal stand-in exposing the two surfaces these helpers
  // read: the header map and nextUrl.
  function reqTo(url: string, headers: Record<string, string> = {}): NextRequest {
    return { headers: new Headers(headers), nextUrl: new URL(url) } as unknown as NextRequest;
  }

  it("ignores x-forwarded-host/proto when the canonical origin is configured", () => {
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    const request = reqTo("http://127.0.0.1:3000/chat", {
      "x-forwarded-host": "evil.example.com",
      "x-forwarded-proto": "http",
    });
    expect(auth.getRedirectUrl(request, "/login").toString()).toBe(
      "https://fleet.example.com/login",
    );
    expect(auth.isSecureRequest(request)).toBe(true);
  });

  it("reports insecure for a configured plain-http origin (loopback deploys)", () => {
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "http://localhost:3000";
    const request = reqTo("http://localhost:3000/chat", { "x-forwarded-proto": "https" });
    expect(auth.getRedirectUrl(request, "/login").toString()).toBe("http://localhost:3000/login");
    expect(auth.isSecureRequest(request)).toBe(false);
  });

  it("falls back to forwarded headers when no origin is configured", () => {
    delete process.env.NEXT_PUBLIC_PUBLIC_ORIGIN;
    const request = reqTo("http://127.0.0.1:3000/chat", {
      "x-forwarded-host": "chat.example.com",
      "x-forwarded-proto": "https",
    });
    expect(auth.getRedirectUrl(request, "/login").toString()).toBe(
      "https://chat.example.com/login",
    );
    expect(auth.isSecureRequest(request)).toBe(true);
  });

  it("falls back to the request URL when neither origin nor headers are present", () => {
    delete process.env.NEXT_PUBLIC_PUBLIC_ORIGIN;
    const request = reqTo("http://localhost:3000/chat");
    expect(auth.getRedirectUrl(request, "/login").toString()).toBe("http://localhost:3000/login");
    expect(auth.isSecureRequest(request)).toBe(false);
  });

  it("treats an unparseable configured origin as unset", () => {
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "not a url";
    const request = reqTo("http://localhost:3000/chat", { "x-forwarded-proto": "https" });
    expect(auth.getRedirectUrl(request, "/login").toString()).toBe("https://localhost:3000/login");
    expect(auth.isSecureRequest(request)).toBe(true);
  });
});

describe("HMAC session lifetimes (ADR-0064: one day absolute, twelve hours idle)", () => {
  const HOUR = 60 * 60;
  const T0 = 1_800_000_000; // fixed mint instant, unix seconds

  function decodePayload(token: string) {
    const body = token.split(".")[0].replace(/-/g, "+").replace(/_/g, "/");
    return JSON.parse(atob(body.padEnd(Math.ceil(body.length / 4) * 4, "="))) as Record<string, unknown>;
  }

  // signWithSecret mints a token with an arbitrary payload, the way a pre-ADR
  // build would have, so the tests can present claims the library never emits.
  async function signWithSecret(payload: object) {
    const body = toBase64Url(enc.encode(JSON.stringify(payload)));
    const key = await crypto.subtle.importKey("raw", enc.encode(SECRET), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, enc.encode(body)));
    return `${body}.${toBase64Url(sig)}`;
  }

  function at(seconds: number) {
    vi.setSystemTime(seconds * 1000);
  }

  function requestAndResponse(secure = true) {
    const request = {
      headers: new Headers(secure ? { "x-forwarded-proto": "https" } : {}),
      nextUrl: new URL(secure ? "https://chat.example.com/chat" : "http://localhost:3000/chat"),
    } as unknown as NextRequest;
    return { request, response: NextResponse.next() };
  }

  afterEach(() => {
    vi.useRealTimers();
  });

  it("mints exp one day out and idle twelve hours out", async () => {
    vi.useFakeTimers();
    at(T0);
    const payload = decodePayload(await auth.createSessionToken("bob@x.com", "epoch-1"));
    expect(payload.exp).toBe(T0 + 24 * HOUR);
    expect(payload.idle).toBe(T0 + 12 * HOUR);
    expect(auth.sessionAbsoluteSeconds).toBe(24 * HOUR);
    expect(auth.sessionIdleSeconds).toBe(12 * HOUR);
  });

  it("refuses a session idle for twelve hours even though its absolute deadline is ahead", async () => {
    vi.useFakeTimers();
    at(T0);
    const token = await auth.createSessionToken("bob@x.com", "epoch-1");
    at(T0 + 12 * HOUR - 1);
    expect(await auth.verifySessionToken(token)).not.toBeNull();
    at(T0 + 12 * HOUR + 1);
    expect(await auth.verifySessionToken(token)).toBeNull();
  });

  it("refuses a correctly signed token with no idle claim (pre-ADR cookies are not grandfathered)", async () => {
    const token = await signWithSecret({ email: "bob@x.com", exp: future, epoch: "epoch-1" });
    expect(await auth.verifySessionToken(token)).toBeNull();
    const withIdle = await signWithSecret({ email: "bob@x.com", exp: future, idle: future, epoch: "epoch-1" });
    expect(await auth.verifySessionToken(withIdle)).not.toBeNull();
  });

  it("does not re-mint when the last mint is under a minute old", async () => {
    vi.useFakeTimers();
    at(T0);
    const token = await auth.createSessionToken("bob@x.com", "epoch-1");
    at(T0 + 30);
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: token }));
    const { request, response } = requestAndResponse();
    expect(await auth.refreshSessionCookie(request, response, session!)).toBeNull();
    expect(response.cookies.get(auth.getSessionCookieName())).toBeUndefined();
  });

  it("re-mints after a minute: idle moves forward, exp and every identity claim are copied", async () => {
    vi.useFakeTimers();
    at(T0);
    const token = await auth.createOidcSessionToken("Bob@x.com", "epoch-9", "https://auth.example.com", "sub-1");
    at(T0 + 61);
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: token }));
    const { request, response } = requestAndResponse();
    const refreshed = await auth.refreshSessionCookie(request, response, session!);
    expect(refreshed).not.toBeNull();
    const payload = decodePayload(refreshed!);
    expect(payload).toMatchObject({
      email: "bob@x.com",
      exp: T0 + 24 * HOUR,
      idle: T0 + 61 + 12 * HOUR,
      epoch: "epoch-9",
      source: "oidc",
      issuer: "https://auth.example.com",
      subject: "sub-1",
    });
    const cookie = response.cookies.get(auth.getSessionCookieName());
    expect(cookie?.value).toBe(refreshed);
    expect(cookie).toMatchObject({ httpOnly: true, sameSite: "lax", secure: true, path: "/" });
    expect(cookie?.maxAge).toBe(24 * HOUR - 61);
    expect(await auth.verifySessionToken(refreshed)).toMatchObject({ source: "oidc", subject: "sub-1" });
  });

  it("activity never extends the absolute deadline", async () => {
    vi.useFakeTimers();
    at(T0);
    let token = await auth.createSessionToken("bob@x.com", "epoch-1");
    // Touch at 11h (idle -> 23h) and 22h (idle -> capped at exp, 24h).
    for (const [step, wantIdle] of [
      [11 * HOUR, T0 + 23 * HOUR],
      [22 * HOUR, T0 + 24 * HOUR],
    ] as const) {
      at(T0 + step);
      const session = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: token }));
      expect(session).not.toBeNull();
      const { request, response } = requestAndResponse();
      const refreshed = await auth.refreshSessionCookie(request, response, session!);
      expect(refreshed).not.toBeNull();
      expect(decodePayload(refreshed!)).toMatchObject({ exp: T0 + 24 * HOUR, idle: wantIdle });
      token = refreshed!;
    }
    at(T0 + 24 * HOUR - 1);
    expect(await auth.verifySessionToken(token)).not.toBeNull();
    at(T0 + 24 * HOUR + 1);
    expect(await auth.verifySessionToken(token)).toBeNull();
  });

  it("stops re-minting once idle already sits on the absolute deadline", async () => {
    vi.useFakeTimers();
    at(T0);
    const minted = await auth.createSessionToken("bob@x.com", "epoch-1");
    at(T0 + 11 * HOUR);
    const first = requestAndResponse();
    const s1 = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: minted }));
    const t1 = await auth.refreshSessionCookie(first.request, first.response, s1!);
    at(T0 + 22 * HOUR + 30 * 60);
    const second = requestAndResponse();
    const s2 = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: t1! }));
    const t2 = await auth.refreshSessionCookie(second.request, second.response, s2!);
    expect(decodePayload(t2!).idle).toBe(T0 + 24 * HOUR);
    at(T0 + 22 * HOUR + 35 * 60);
    const third = requestAndResponse();
    const s3 = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: t2! }));
    expect(s3).not.toBeNull();
    expect(await auth.refreshSessionCookie(third.request, third.response, s3!)).toBeNull();
    expect(third.response.cookies.get(auth.getSessionCookieName())).toBeUndefined();
  });

  it("leaves elcano_auth sessions alone: Fleet cannot re-mint the auth service's cookie", async () => {
    const token = await makeToken(priv, { email: "carol@elcanotek.com", exp: future });
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getElcanoCookieName()]: token }));
    const { request, response } = requestAndResponse();
    expect(await auth.refreshSessionCookie(request, response, session!)).toBeNull();
    expect(response.cookies.get(auth.getSessionCookieName())).toBeUndefined();
  });

  it("mints an insecure cookie on a plain-HTTP dev request", async () => {
    vi.useFakeTimers();
    at(T0);
    const token = await auth.createSessionToken("bob@x.com", "epoch-1");
    at(T0 + 120);
    const session = await auth.getSessionFromRequest(reqWith({ [auth.getSessionCookieName()]: token }));
    const { request, response } = requestAndResponse(false);
    await auth.refreshSessionCookie(request, response, session!);
    expect(response.cookies.get(auth.getSessionCookieName())?.secure).toBe(false);
  });
});
