import { NextRequest, NextResponse } from "next/server";
import {
  getElcanoCookieDomain,
  getElcanoCookieName,
  getRedirectUrl,
  getSessionCookieName,
  isSecureRequest,
  verifySessionToken,
} from "@/app/lib/auth";
import { verifyOrigin } from "@/app/lib/csrf";
import { getOidcConfig } from "@/app/lib/oidc";

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
 *
 * A session minted through central OIDC (source "oidc") also has a live
 * session on the identity provider behind it. Clearing Fleet's cookie alone
 * would leave that in place, and with FLEET_OIDC_AUTO_START the next visit
 * would silently sign the user straight back in, so "log out" would appear to
 * do nothing. For those sessions the browser is sent to the provider's
 * RP-initiated logout (`<issuer>/logout?client_id=<ours>`), which on Elcano
 * Auth ends the central session, fans a back-channel logout out to every
 * application, and lands on Auth's login page. Password sessions have no
 * central session and keep landing on /login.
 */
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await verifySessionToken(request.cookies.get(getSessionCookieName())?.value);
  const oidc = getOidcConfig();
  const landing =
    session?.source === "oidc" && oidc
      ? `${oidc.issuer}/logout?client_id=${encodeURIComponent(oidc.clientId)}`
      : getRedirectUrl(request, "/login");

  const secure = isSecureRequest(request);
  const res = NextResponse.redirect(landing, { status: 303 });

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
