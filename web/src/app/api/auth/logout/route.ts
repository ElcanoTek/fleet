import { NextRequest, NextResponse } from "next/server";
import {
  getElcanoCookieDomain,
  getElcanoCookieName,
  getRedirectUrl,
  getSessionCookieName,
  isSecureRequest,
} from "@/app/lib/auth";
import { verifyOrigin } from "@/app/lib/csrf";

/**
 * POST /api/auth/logout
 *
 * Clears BOTH session cookies and returns the user to chat's own /login page:
 *   - elcano_session — chat's HMAC password cookie (host-only).
 *   - elcano_auth     — the shared Ed25519 cookie minted by the auth service.
 *
 * We clear elcano_auth here (rather than bouncing through auth/logout) for two
 * reasons: the user should land back on chat's login, not auth's; and if we
 * left elcano_auth in place, an Elcano-email user would be logged straight back
 * in by the middleware and never see /login. chat can delete it because the
 * cookie lives on the shared parent domain (AUTH_COOKIE_DOMAIN) that chat's
 * host belongs to — and deleting the shared cookie signs the user out of the
 * other Elcano services too, which is the expected meaning of "log out".
 */
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const secure = isSecureRequest(request);
  const res = NextResponse.redirect(getRedirectUrl(request, "/login"), { status: 303 });

  const attrs = `Path=/; Max-Age=0; HttpOnly; SameSite=Lax${secure ? "; Secure" : ""}`;
  res.headers.append("Set-Cookie", `${getSessionCookieName()}=; ${attrs}`);

  // Cookie deletion matches on name + domain + path, so mirror how auth set
  // it. But AUTH_COOKIE_DOMAIN is config that can drift from how the auth
  // service actually minted the cookie (a fresh deployment with the env unset
  // deletes host-only while the live cookie carries Domain=…), and a deletion
  // that misses its shape silently no-ops: the user sees the login page, then
  // the next load is signed back in. Deleting a shape that doesn't exist is
  // harmless, so send BOTH variants — appended as raw headers because a
  // cookie store keyed by name would collapse them into one.
  const elcanoDomain = getElcanoCookieDomain();
  if (elcanoDomain) {
    res.headers.append(
      "Set-Cookie",
      `${getElcanoCookieName()}=; Domain=${elcanoDomain}; ${attrs}`,
    );
  }
  res.headers.append("Set-Cookie", `${getElcanoCookieName()}=; ${attrs}`);

  return res;
}
