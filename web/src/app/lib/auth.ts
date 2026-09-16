import { cookies } from "next/headers";
import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

// chat accepts two session cookies:
//   - elcano_session: the legacy HMAC cookie minted by the password login
//     path (POST /api/auth/login → chat-server /auth/verify). Owned by chat.
//   - elcano_auth: the Ed25519 cookie minted by the auth service
//     (auth.elcanotek.com) for the "Use Elcano email" magic-link path. chat
//     holds only the public key and verifies it natively (Pattern B); the
//     local user-list gate that admits known emails lives in chat-server.
// Either valid cookie is a session; the membership check is enforced
// downstream by chat-server (403 not_a_member) for elcano_auth users.
const sessionCookieName = "elcano_session";
// Session lifetimes (ADR-0064). The Fleet session is an APPLICATION session in
// the Elcano two-layer model: while the user's central Auth session (30 days)
// is live, an expired Fleet session costs them only a redirect through the
// OIDC handoff, so these limits bound a stolen cookie and force a daily
// re-check of the account rather than deciding how often people log in.
// One day absolute, twelve hours idle is the convention for every Elcano
// application session (Explorer and Lens use the same numbers).
export const sessionAbsoluteSeconds = 60 * 60 * 24;
export const sessionIdleSeconds = 60 * 60 * 12;
// A request re-mints the cookie (pushing the idle deadline out) only when the
// previous mint is more than this old, so a page's burst of requests costs one
// Set-Cookie instead of one per request. The idle limit therefore behaves as
// "twelve hours minus at most one minute", never longer. One minute is the
// Elcano convention; keep it a constant, not a setting.
export const sessionTouchSeconds = 60;
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

// Source distinguishes which cookie authenticated the request — useful for
// logout (the elcano path signs out via the auth service) and for the
// membership entry check (only elcano sessions need it; password users are
// in the user-list by construction).
export type SessionSource = "password" | "oidc" | "elcano";

export type Session = {
  email: string;
  exp: number;
  tenant?: string;
  source: SessionSource;
  // epoch is the chat-server session epoch this cookie was minted against, and
  // is forwarded to chat-server so it can refuse a session the account has
  // since outlived (a password change moves the epoch). Absent for elcano
  // sessions: that cookie is minted by the auth service, which chat cannot add
  // a claim to.
  epoch?: string;
  issuer?: string;
  subject?: string;
  // idle is the HMAC cookie's idle deadline (unix seconds); refreshSessionCookie
  // pushes it out on activity. Absent for elcano sessions, which Fleet cannot
  // re-mint.
  idle?: number;
};

type SessionPayload = {
  email: string;
  // exp is the absolute deadline: fixed at mint, never extended by activity.
  exp: number;
  // idle is the idle deadline: min(now + sessionIdleSeconds, exp), moved
  // forward by refreshSessionCookie while the user stays active.
  idle: number;
  epoch: string;
  source?: "password" | "oidc";
  issuer?: string;
  subject?: string;
};

function getSessionSecret() {
  const secret = process.env.APP_SESSION_SECRET;
  if (!secret) {
    throw new Error("Missing required environment variable: APP_SESSION_SECRET");
  }

  return secret;
}

function bytesToBase64Url(bytes: Uint8Array) {
  const base64 = btoa(String.fromCharCode(...bytes));
  return base64.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function base64UrlToBytes(value: string) {
  const normalized = value.replace(/-/g, "+").replace(/_/g, "/");
  const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=");
  const binary = atob(padded);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

function encodePayload(payload: string) {
  return bytesToBase64Url(textEncoder.encode(payload));
}

function decodePayload(value: string) {
  return textDecoder.decode(base64UrlToBytes(value));
}

async function importSigningKey() {
  return crypto.subtle.importKey(
    "raw",
    textEncoder.encode(getSessionSecret()),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign", "verify"],
  );
}

async function signPayload(payload: string) {
  const key = await importSigningKey();
  const signature = await crypto.subtle.sign("HMAC", key, textEncoder.encode(payload));
  return bytesToBase64Url(new Uint8Array(signature));
}

async function signSessionPayload(payload: SessionPayload) {
  const encodedPayload = encodePayload(JSON.stringify(payload));
  const signature = await signPayload(encodedPayload);
  return `${encodedPayload}.${signature}`;
}

// freshDeadlines returns the exp/idle pair for a session minted now. The idle
// deadline never passes the absolute one.
function freshDeadlines(nowSeconds: number) {
  const exp = nowSeconds + sessionAbsoluteSeconds;
  return { exp, idle: Math.min(nowSeconds + sessionIdleSeconds, exp) };
}

// createSessionToken mints the HMAC cookie. `epoch` is the account's current
// chat-server session epoch (fetchSessionEpoch) and is mandatory: a cookie
// without it is refused by verifySessionToken below, so no mint path may skip it.
export async function createSessionToken(email: string, epoch: string) {
  return signSessionPayload({
    email: email.toLowerCase(),
    ...freshDeadlines(Math.floor(Date.now() / 1000)),
    epoch,
  });
}

export async function createOidcSessionToken(
  email: string,
  epoch: string,
  issuer: string,
  subject: string,
) {
  return signSessionPayload({
    email: email.toLowerCase(),
    ...freshDeadlines(Math.floor(Date.now() / 1000)),
    epoch,
    source: "oidc",
    issuer,
    subject,
  });
}

// refreshSessionCookie implements the idle limit for the HMAC cookie. A
// stateless cookie has no server-side last_seen_at to touch, so "activity"
// is recorded by re-minting the cookie with a later idle deadline. It is
// called by the request proxy on every authenticated pass-through and does
// nothing unless the session is an HMAC one whose idle deadline can move by
// at least sessionTouchSeconds; the absolute deadline is copied, never
// extended. Returns the new token when a cookie was set, for tests.
export async function refreshSessionCookie(
  request: NextRequest,
  response: NextResponse,
  session: Session,
): Promise<string | null> {
  if (session.source === "elcano" || session.idle === undefined || !session.epoch) {
    return null;
  }
  const now = Math.floor(Date.now() / 1000);
  const idle = Math.min(now + sessionIdleSeconds, session.exp);
  if (idle - session.idle < sessionTouchSeconds) {
    return null;
  }
  const token = await signSessionPayload({
    email: session.email,
    exp: session.exp,
    idle,
    epoch: session.epoch,
    ...(session.source === "oidc"
      ? { source: "oidc" as const, issuer: session.issuer, subject: session.subject }
      : {}),
  });
  response.cookies.set({
    name: sessionCookieName,
    value: token,
    httpOnly: true,
    sameSite: "lax",
    secure: isSecureRequest(request),
    // The browser drops the cookie at the absolute deadline even if the idle
    // deadline inside it is later; the server enforces both regardless.
    maxAge: session.exp - now,
    path: "/",
  });
  return token;
}

export async function verifySessionToken(token: string | undefined | null) {
  if (!token) {
    return null;
  }

  const [encodedPayload, signature] = token.split(".");
  if (!encodedPayload || !signature) {
    return null;
  }

  try {
    const key = await importSigningKey();
    const isValid = await crypto.subtle.verify(
      "HMAC",
      key,
      base64UrlToBytes(signature),
      textEncoder.encode(encodedPayload),
    );

    if (!isValid) {
      return null;
    }

    const payload = JSON.parse(decodePayload(encodedPayload)) as SessionPayload;
    const now = Date.now();
    if (!payload.email || typeof payload.exp !== "number" || payload.exp * 1000 < now) {
      return null;
    }
    // No idle claim means the cookie predates the idle limit (ADR-0064) and was
    // signed for fourteen days flat. It is not grandfathered: honouring it would
    // keep every pre-deploy cookie alive for the rest of those fourteen days.
    // Users with a live central Auth session are signed back in by the OIDC
    // handoff without a prompt; password users log in once more.
    if (typeof payload.idle !== "number" || payload.idle * 1000 < now) {
      return null;
    }
    // No epoch claim means the cookie predates per-user session revocation, and
    // nothing downstream could check it. Refusing here is what makes the
    // revocation gate unbypassable — a token minted before the claim existed
    // would otherwise sail past chat-server's "no claim, admit it" rule for the
    // whole 14 days it was signed for.
    if (!payload.epoch) {
      return null;
    }
    if (payload.source === "oidc" && (!payload.issuer || !payload.subject)) {
      return null;
    }
    if (payload.source && payload.source !== "password" && payload.source !== "oidc") {
      return null;
    }

    return payload;
  } catch {
    return null;
  }
}

// ── elcano_auth: Ed25519 cookie minted by the auth service ────────────────
//
// Verification mirrors auth/internal/token/token.go (Verify + VerifySession)
// and home/server.js byte-for-byte. Token format:
//   base64url(payloadJSON) + "." + base64url(ed25519Signature)
// The signature covers the base64url-encoded body STRING, not the raw JSON.
// Payload is {email, tenant, iat, exp}; we read email + exp. We hold only the
// PUBLIC key (AUTH_SIGNING_PUBKEY) — enough to verify, never to mint — so a
// leak of this value cannot forge a session.

export function getElcanoCookieName() {
  return process.env.AUTH_COOKIE_NAME || "elcano_auth";
}

// getElcanoCookieDomain returns the Domain the auth service mints the
// elcano_auth cookie on (AUTH_COOKIE_DOMAIN, e.g. "elcanotek.com"), or "" when
// it's a host-only cookie (local dev). Logout needs this to delete the shared
// cookie: a deletion only takes effect when Name + Domain + Path match how it
// was set.
export function getElcanoCookieDomain() {
  return process.env.AUTH_COOKIE_DOMAIN?.trim() ?? "";
}

// getAuthSigningPubkey returns the configured public key (trimmed), or "" when
// the Elcano-email path is disabled. Callers use the empty string to detect
// the disabled state without reading process.env directly.
export function getAuthSigningPubkey() {
  return process.env.AUTH_SIGNING_PUBKEY?.trim() ?? "";
}

export function getAuthLoginUrl() {
  return (process.env.AUTH_LOGIN_URL ?? "https://auth.elcanotek.com").replace(/\/+$/, "");
}

// buildElcanoLoginUrl points the browser at the auth service's login form,
// signed back to `returnTo` after a successful magic-link round-trip. auth
// re-validates return_to against its own allowlist, so an off-platform value
// is harmless.
export function buildElcanoLoginUrl(returnTo: string) {
  return `${getAuthLoginUrl()}/?return_to=${encodeURIComponent(returnTo)}`;
}

// elcanoLoginRedirect is the ONE "Use Elcano email" handoff, shared by both
// views' /api/.../auth/elcano-login routes. They differ only in where the auth
// service returns the browser after the magic-link round-trip (`returnToPath`):
// chat lands on home ("/"), the orchestrator on "/orchestrator" — so each user
// returns to the view they started in.
//
// Without a configured public key the app can never verify the elcano_auth
// cookie auth would mint, so sending the user there would trap them in a
// redirect loop (auth sets the cookie, the app can't read it, back to /login,
// click again, repeat). In that disabled state bounce to the password login
// with a message instead. This guard MUST stay intact: dropping it reintroduces
// the redirect loop.
export function elcanoLoginRedirect(request: NextRequest, returnToPath: string): NextResponse {
  if (!getAuthSigningPubkey()) {
    return NextResponse.redirect(getRedirectUrl(request, "/login?e=elcano_unavailable"), {
      status: 303,
    });
  }
  const returnTo = getRedirectUrl(request, returnToPath).toString();
  return NextResponse.redirect(buildElcanoLoginUrl(returnTo), { status: 303 });
}

// The public key is standard base64 (matching auth-admin keygen output and
// home/server.js's `Buffer.from(AUTH_SIGNING_PUBKEY, "base64")`), not the
// base64url used for the token body/signature.
function stdBase64ToBytes(value: string) {
  const binary = atob(value);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

// Imported once and cached. Returns null (and the verifier fails closed) when
// AUTH_SIGNING_PUBKEY is unset or malformed.
let elcanoKeyPromise: Promise<CryptoKey | null> | undefined;
function importElcanoPublicKey(): Promise<CryptoKey | null> {
  if (elcanoKeyPromise) return elcanoKeyPromise;
  elcanoKeyPromise = (async () => {
    const b64 = getAuthSigningPubkey();
    if (!b64) return null;
    try {
      const raw = stdBase64ToBytes(b64);
      if (raw.length !== 32) return null;
      return await crypto.subtle.importKey("raw", raw, { name: "Ed25519" }, false, ["verify"]);
    } catch {
      return null;
    }
  })();
  return elcanoKeyPromise;
}

export async function verifyElcanoToken(
  token: string | undefined | null,
): Promise<{ email: string; tenant: string; exp: number } | null> {
  if (!token) return null;
  const key = await importElcanoPublicKey();
  if (!key) return null;

  const dot = token.indexOf(".");
  if (dot < 1 || dot === token.length - 1) return null;
  const body = token.slice(0, dot);
  const signaturePart = token.slice(dot + 1);

  try {
    const signature = base64UrlToBytes(signaturePart);
    const ok = await crypto.subtle.verify({ name: "Ed25519" }, key, signature, textEncoder.encode(body));
    if (!ok) return null;

    const payload = JSON.parse(decodePayload(body)) as { email?: string; tenant?: string; exp?: number };
    if (!payload.email || typeof payload.email !== "string") return null;
    if (typeof payload.exp !== "number" || payload.exp <= Math.floor(Date.now() / 1000)) return null;

    return { email: payload.email, tenant: payload.tenant ?? "", exp: payload.exp };
  } catch {
    return null;
  }
}

// resolveSession accepts either cookie. The HMAC password cookie wins when
// both are present (it's the more specific, chat-owned session); otherwise we
// fall back to the shared elcano_auth cookie.
async function resolveSession(
  hmacToken: string | undefined | null,
  elcanoToken: string | undefined | null,
): Promise<Session | null> {
  const hmac = await verifySessionToken(hmacToken ?? null);
  if (hmac) {
    return {
      email: hmac.email,
      exp: hmac.exp,
      epoch: hmac.epoch,
      source: hmac.source === "oidc" ? "oidc" : "password",
      issuer: hmac.issuer,
      subject: hmac.subject,
      idle: hmac.idle,
    };
  }

  const elcano = await verifyElcanoToken(elcanoToken ?? null);
  if (elcano) return { email: elcano.email, exp: elcano.exp, tenant: elcano.tenant, source: "elcano" };

  return null;
}

// getServerSession reads cookies in a Server Component / Route Handler. Its
// return value still carries `email` and `exp`, so the existing callers keep
// working unchanged; `source`/`tenant` are additive.
export async function getServerSession(): Promise<Session | null> {
  const cookieStore = await cookies();
  return resolveSession(
    cookieStore.get(sessionCookieName)?.value,
    cookieStore.get(getElcanoCookieName())?.value,
  );
}

// getSessionFromRequest is the middleware-side equivalent that reads from the
// incoming request's cookies.
export async function getSessionFromRequest(request: NextRequest): Promise<Session | null> {
  return resolveSession(
    request.cookies.get(sessionCookieName)?.value,
    request.cookies.get(getElcanoCookieName())?.value,
  );
}

export function getSessionCookieName() {
  return sessionCookieName;
}

// getConfiguredOrigin returns the deployment's canonical public origin
// (NEXT_PUBLIC_PUBLIC_ORIGIN — bootstrap writes it for every deploy), or null
// when unset/unparseable. Redirect targets, the Secure-cookie decision, the
// OIDC redirect_uri (lib/oidc.ts) and the CSRF expected-host (lib/csrf.ts)
// prefer it over x-forwarded-* because those headers are client-supplied
// unless a proxy overwrites them: a request that reaches `next start`
// directly (not through Caddy) could otherwise pick the redirect host and
// mint a session cookie without Secure by claiming x-forwarded-proto: http.
export function getConfiguredOrigin(): URL | null {
  const raw = process.env.NEXT_PUBLIC_PUBLIC_ORIGIN?.trim();
  if (!raw) return null;
  try {
    return new URL(raw);
  } catch {
    return null;
  }
}

// The header fallbacks below keep local dev working (no configured origin,
// plain http on localhost) and any deploy that predates the origin env.
export function getRedirectUrl(request: NextRequest, pathname: string) {
  const configured = getConfiguredOrigin();
  if (configured) return new URL(pathname, configured);
  const host = request.headers.get("x-forwarded-host") ?? request.headers.get("host") ?? request.nextUrl.host;
  const protocol = request.headers.get("x-forwarded-proto") ?? request.nextUrl.protocol.replace(":", "");
  return new URL(pathname, `${protocol}://${host}`);
}

export function isSecureRequest(request: NextRequest) {
  const configured = getConfiguredOrigin();
  if (configured) return configured.protocol === "https:";
  const protocol = request.headers.get("x-forwarded-proto") ?? request.nextUrl.protocol.replace(":", "");
  return protocol === "https";
}
