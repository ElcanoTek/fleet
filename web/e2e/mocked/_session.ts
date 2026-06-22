import crypto from "node:crypto";
import type { BrowserContext } from "@playwright/test";
import { TEST_SESSION_SECRET, TEST_AUTH_PRIVATE_KEY_PEM } from "../../playwright.config";

// Session helpers for the mocked suite. Both login paths the unified middleware
// accepts can be minted here so a spec can start "already logged in" without
// standing up a Go backend:
//
//   - elcano_session: the HMAC cookie minted by the password login form
//     (src/app/lib/auth.ts#createSessionToken).
//   - elcano_auth:    the Ed25519 cookie the auth service mints for the
//     "Use Elcano email" magic-link path (src/app/lib/auth.ts#verifyElcanoToken).
//
// Both are verified natively by the Next middleware, so minting them here drives
// the REAL gate — no chat-server round-trip needed.

function b64url(buf: Buffer): string {
  return buf.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

// ── elcano_session: HMAC cookie (password path) ────────────────────────────
// Mirrors createSessionToken: base64url(JSON{email,exp}) + "." +
// base64url(HMAC-SHA256(secret, payload)).
export function mintSessionToken(email: string): string {
  const payload = JSON.stringify({
    email: email.toLowerCase(),
    exp: Math.floor(Date.now() / 1000) + 60 * 60 * 24,
  });
  const encodedPayload = b64url(Buffer.from(payload, "utf8"));
  const sig = crypto.createHmac("sha256", TEST_SESSION_SECRET).update(encodedPayload).digest();
  return `${encodedPayload}.${b64url(sig)}`;
}

// ── elcano_auth: Ed25519 cookie (magic-link path) ──────────────────────────
// The PRIVATE half of the throwaway keypair is GENERATED AT RUNTIME in
// playwright.config.ts (no key literal in the repo) and imported here; its
// matching PUBLIC half is exported to the server as AUTH_SIGNING_PUBKEY in the
// same config, so server + signer always agree. Signing here lets a spec mint a
// token the real verifier (verifyElcanoToken) accepts, exercising the Ed25519
// branch of the dual-login middleware. Token format mirrors the auth service:
// base64url(JSON{email,tenant,iat,exp}) + "." + base64url(ed25519Sig),
// signature over the base64url body STRING.
export function mintElcanoToken(email: string): string {
  const now = Math.floor(Date.now() / 1000);
  const payload = JSON.stringify({ email: email.toLowerCase(), tenant: "", iat: now, exp: now + 60 * 60 * 24 });
  const body = b64url(Buffer.from(payload, "utf8"));
  const key = crypto.createPrivateKey(TEST_AUTH_PRIVATE_KEY_PEM);
  const sig = crypto.sign(null, Buffer.from(body, "utf8"), key);
  return `${body}.${b64url(sig)}`;
}

// Installs the HMAC password-session cookie so every page load is authenticated.
// Host-only cookie on 127.0.0.1 to match the dev/prod server.
export async function loginViaCookie(context: BrowserContext, email = "e2e@example.com") {
  await context.addCookies([
    {
      name: "elcano_session",
      value: mintSessionToken(email),
      domain: "127.0.0.1",
      path: "/",
      httpOnly: true,
      sameSite: "Lax",
    },
  ]);
}

// Installs the Ed25519 elcano_auth cookie — the "Use Elcano email" session.
export async function loginViaElcanoCookie(context: BrowserContext, email = "e2e@example.com") {
  await context.addCookies([
    {
      name: "elcano_auth",
      value: mintElcanoToken(email),
      domain: "127.0.0.1",
      path: "/",
      httpOnly: true,
      sameSite: "Lax",
    },
  ]);
}
