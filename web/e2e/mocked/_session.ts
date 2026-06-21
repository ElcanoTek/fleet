import crypto from "node:crypto";
import type { BrowserContext } from "@playwright/test";
import { TEST_SESSION_SECRET } from "../../playwright.config";

// Mints the elcano_session HMAC cookie the unified middleware accepts, exactly
// as src/app/lib/auth.ts#createSessionToken does:
//   base64url(JSON{email,exp}) + "." + base64url(HMAC-SHA256(secret, payload))
// Used by the mocked specs to start "already logged in" without standing up a
// Go backend. This is the password-cookie login path; the elcano_auth Ed25519
// path is exercised separately by the dual-login middleware unit tests.

function b64url(buf: Buffer): string {
  return buf.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

export function mintSessionToken(email: string): string {
  const payload = JSON.stringify({
    email: email.toLowerCase(),
    exp: Math.floor(Date.now() / 1000) + 60 * 60 * 24,
  });
  const encodedPayload = b64url(Buffer.from(payload, "utf8"));
  const sig = crypto.createHmac("sha256", TEST_SESSION_SECRET).update(encodedPayload).digest();
  return `${encodedPayload}.${b64url(sig)}`;
}

// Installs the session cookie on the browser context so every page load is
// authenticated. Host-only cookie on 127.0.0.1 to match the dev server.
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
