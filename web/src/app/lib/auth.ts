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
export const sessionMaxAgeSeconds = 60 * 60 * 24 * 14;
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

// Source distinguishes which cookie authenticated the request — useful for
// logout (the elcano path signs out via the auth service) and for the
// membership entry check (only elcano sessions need it; password users are
// in the user-list by construction).
export type SessionSource = "password" | "elcano";

export type Session = {
  email: string;
  exp: number;
  tenant?: string;
  source: SessionSource;
};

type SessionPayload = {
  email: string;
  exp: number;
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

export async function createSessionToken(email: string) {
  const payload = JSON.stringify({
    email: email.toLowerCase(),
    exp: Math.floor(Date.now() / 1000) + sessionMaxAgeSeconds,
  } satisfies SessionPayload);
  const encodedPayload = encodePayload(payload);
  const signature = await signPayload(encodedPayload);
  return `${encodedPayload}.${signature}`;
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
    if (!payload.email || payload.exp * 1000 < Date.now()) {
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
  if (hmac) return { email: hmac.email, exp: hmac.exp, source: "password" };

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

export function getRedirectUrl(request: NextRequest, pathname: string) {
  const host = request.headers.get("x-forwarded-host") ?? request.headers.get("host") ?? request.nextUrl.host;
  const protocol = request.headers.get("x-forwarded-proto") ?? request.nextUrl.protocol.replace(":", "");
  return new URL(pathname, `${protocol}://${host}`);
}

export function isSecureRequest(request: NextRequest) {
  const protocol = request.headers.get("x-forwarded-proto") ?? request.nextUrl.protocol.replace(":", "");
  return protocol === "https";
}
