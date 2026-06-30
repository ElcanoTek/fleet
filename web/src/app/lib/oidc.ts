// OIDC / OAuth2 Single Sign-On (#240).
//
// Architecture: SSO lives in the Next.js layer, NOT the Go chat server. Next
// already owns login + session cookies (see lib/auth.ts); the chat server only
// trusts X-Chat-Server-Token + X-User-Email and scopes by email. So an OIDC
// login is just "prove the email via an external IdP, then mint the SAME
// elcano_session HMAC cookie the password form mints" — every downstream gate
// (middleware, getServerSession, the chat-server membership check) keeps working
// unchanged. This mirrors how the existing "Use Elcano email" path (elcano_auth)
// authenticates without the chat server knowing anything about the IdP.
//
// Flow: Authorization Code + PKCE (S256) for a confidential client. The ID token
// is read directly from the token endpoint over server-to-server TLS, so per
// OIDC Core §3.1.3.7 the client MAY validate the token's claims (iss / aud / exp
// / nonce) without re-verifying its signature — the TLS channel to the token
// endpoint is the integrity guarantee. That is what lets this ship with zero new
// crypto dependencies. State + nonce + PKCE defend the redirect/callback against
// CSRF and code interception.

import type { NextRequest } from "next/server";

// Temp cookies that carry the per-attempt CSRF material from /start to
// /callback. Short-lived, httpOnly, SameSite=Lax (so they survive the IdP's
// top-level GET redirect back to us). Cleared on callback.
export const OIDC_STATE_COOKIE = "fleet_oidc_state";
export const OIDC_NONCE_COOKIE = "fleet_oidc_nonce";
export const OIDC_VERIFIER_COOKIE = "fleet_oidc_verifier";
export const oidcTempCookieMaxAgeSeconds = 600; // 10 minutes to complete login

export type OidcConfig = {
  issuer: string;
  clientId: string;
  clientSecret: string;
  scopes: string;
  // allowedDomains is a lowercased email-domain allowlist (no leading "@").
  // Empty means "any domain the IdP authenticates" — the chat-server user-list
  // is then the only membership gate, exactly as with elcano_auth.
  allowedDomains: string[];
  buttonLabel: string;
  // redirectUri pins the callback URL when set (must match the IdP
  // registration). When unset it is derived from the request host.
  redirectUri?: string;
};

// getOidcConfig reads FLEET_OIDC_* at request time and returns null unless the
// three load-bearing values (issuer + client id + secret) are all present. A
// partial config is treated as "disabled" rather than a half-wired flow.
export function getOidcConfig(): OidcConfig | null {
  const issuer = (process.env.FLEET_OIDC_ISSUER ?? "").trim().replace(/\/+$/, "");
  const clientId = (process.env.FLEET_OIDC_CLIENT_ID ?? "").trim();
  const clientSecret = (process.env.FLEET_OIDC_CLIENT_SECRET ?? "").trim();
  if (!issuer || !clientId || !clientSecret) return null;

  const scopes = (process.env.FLEET_OIDC_SCOPES ?? "").trim() || "openid email profile";
  const allowedDomains = (process.env.FLEET_OIDC_ALLOWED_DOMAINS ?? "")
    .split(",")
    .map((d) => d.trim().toLowerCase().replace(/^@/, ""))
    .filter(Boolean);
  const buttonLabel = (process.env.FLEET_OIDC_BUTTON_LABEL ?? "").trim() || "Sign in with SSO";
  const redirectUri = process.env.FLEET_OIDC_REDIRECT_URI?.trim() || undefined;

  return { issuer, clientId, clientSecret, scopes, allowedDomains, buttonLabel, redirectUri };
}

// oidcEnabled is the single gate the login page + middleware consult. Resolving
// it from runtime env (not a NEXT_PUBLIC build flag) keeps SSO a pure ops switch.
export function oidcEnabled(): boolean {
  return getOidcConfig() !== null;
}

export type DiscoveryDoc = {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  userinfo_endpoint?: string;
};

// discoveryCache memoizes the per-issuer well-known document for the lifetime of
// the server process. The endpoints are static config; re-fetching them on every
// login would add a round-trip and a failure mode for no benefit.
const discoveryCache = new Map<string, Promise<DiscoveryDoc>>();

export async function discover(issuer: string, fetchImpl: typeof fetch = fetch): Promise<DiscoveryDoc> {
  const cached = discoveryCache.get(issuer);
  if (cached) return cached;
  const promise = (async () => {
    const url = `${issuer.replace(/\/+$/, "")}/.well-known/openid-configuration`;
    const res = await fetchImpl(url, { headers: { Accept: "application/json" } });
    if (!res.ok) throw new Error(`OIDC discovery failed: ${res.status}`);
    const doc = (await res.json()) as Partial<DiscoveryDoc>;
    if (!doc.authorization_endpoint || !doc.token_endpoint || !doc.issuer) {
      throw new Error("OIDC discovery document missing required endpoints");
    }
    return doc as DiscoveryDoc;
  })();
  // Cache the promise so concurrent logins share one fetch; drop it on failure
  // so a transient error doesn't poison every subsequent attempt.
  discoveryCache.set(issuer, promise);
  promise.catch(() => discoveryCache.delete(issuer));
  return promise;
}

// __resetDiscoveryCacheForTest clears the memo so tests can re-stub discovery.
export function __resetDiscoveryCacheForTest() {
  discoveryCache.clear();
}

// ── base64url + random helpers (no Buffer; runs in node + edge) ─────────────

function bytesToBase64Url(bytes: Uint8Array): string {
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function base64UrlToString(value: string): string {
  const normalized = value.replace(/-/g, "+").replace(/_/g, "/");
  const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=");
  return atob(padded);
}

// randomUrlSafe returns `bytes` of CSPRNG output, base64url-encoded — used for
// state, nonce, and the PKCE verifier.
export function randomUrlSafe(bytes = 32): string {
  const buf = crypto.getRandomValues(new Uint8Array(bytes));
  return bytesToBase64Url(buf);
}

// pkceChallenge derives the S256 code_challenge from a verifier.
export async function pkceChallenge(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return bytesToBase64Url(new Uint8Array(digest));
}

// decodeJwtClaims base64url-decodes a JWT's payload WITHOUT verifying its
// signature. Safe here only because the token is read directly from the token
// endpoint over TLS (see file header) — never use this on a token received via
// the front channel (e.g. an implicit-flow fragment).
export function decodeJwtClaims(jwt: string): Record<string, unknown> | null {
  const parts = jwt.split(".");
  if (parts.length !== 3) return null;
  try {
    return JSON.parse(base64UrlToString(parts[1])) as Record<string, unknown>;
  } catch {
    return null;
  }
}

// emailDomainAllowed enforces the optional domain allowlist. An empty allowlist
// admits any domain (the chat-server user-list remains the membership gate).
export function emailDomainAllowed(email: string, allowedDomains: string[]): boolean {
  if (allowedDomains.length === 0) return true;
  const at = email.lastIndexOf("@");
  if (at < 0) return false;
  return allowedDomains.includes(email.slice(at + 1).toLowerCase());
}

export type IdTokenValidation = { ok: true; email: string } | { ok: false; reason: string };

// validateIdToken checks the security-relevant claims of an ID token obtained
// from the token endpoint: issuer match, audience contains our client_id, not
// expired, nonce echoes the one we planted, and (when present) azp matches.
// It then extracts a verified email. Signature is intentionally not re-checked
// (see file header). `nowSeconds` is injectable for deterministic tests.
export function validateIdToken(
  claims: Record<string, unknown> | null,
  config: OidcConfig,
  discovery: DiscoveryDoc,
  expectedNonce: string,
  nowSeconds: number = Math.floor(Date.now() / 1000),
): IdTokenValidation {
  if (!claims) return { ok: false, reason: "unparseable id_token" };

  if (claims.iss !== discovery.issuer) return { ok: false, reason: "issuer mismatch" };

  const aud = claims.aud;
  const audOk = Array.isArray(aud) ? aud.includes(config.clientId) : aud === config.clientId;
  if (!audOk) return { ok: false, reason: "audience mismatch" };

  // azp (authorized party) must be our client when present — guards the
  // multi-audience case.
  if (typeof claims.azp === "string" && claims.azp !== config.clientId) {
    return { ok: false, reason: "azp mismatch" };
  }

  if (typeof claims.exp !== "number" || claims.exp <= nowSeconds) {
    return { ok: false, reason: "expired" };
  }

  if (claims.nonce !== expectedNonce) return { ok: false, reason: "nonce mismatch" };

  const email = typeof claims.email === "string" ? claims.email.trim().toLowerCase() : "";
  if (!email) return { ok: false, reason: "no email claim" };
  // Honor an explicit email_verified:false; absent is treated as verified since
  // not every IdP emits the claim.
  if (claims.email_verified === false) return { ok: false, reason: "email not verified" };

  return { ok: true, email };
}

// buildRedirectUri returns the callback URL the IdP redirects back to. A pinned
// FLEET_OIDC_REDIRECT_URI wins; otherwise it is derived from the request's
// forwarded host so dev and prod work without a hardcoded origin.
export function buildRedirectUri(config: OidcConfig, request: NextRequest): string {
  if (config.redirectUri) return config.redirectUri;
  const host =
    request.headers.get("x-forwarded-host") ?? request.headers.get("host") ?? request.nextUrl.host;
  const proto = request.headers.get("x-forwarded-proto") ?? request.nextUrl.protocol.replace(":", "");
  return new URL("/api/auth/oidc/callback", `${proto}://${host}`).toString();
}
