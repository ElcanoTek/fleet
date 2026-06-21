import { cookies } from "next/headers";
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
  const cookieStore = await cookies();

  cookieStore.set({
    name: getSessionCookieName(),
    value: "",
    httpOnly: true,
    sameSite: "lax",
    secure,
    maxAge: 0,
    path: "/",
  });

  // Cookie deletion matches on name + domain + path, so mirror how auth set it.
  // Omit domain entirely for host-only cookies (local dev).
  const elcanoDomain = getElcanoCookieDomain();
  cookieStore.set({
    name: getElcanoCookieName(),
    value: "",
    httpOnly: true,
    sameSite: "lax",
    secure,
    maxAge: 0,
    path: "/",
    ...(elcanoDomain ? { domain: elcanoDomain } : {}),
  });

  return NextResponse.redirect(getRedirectUrl(request, "/login"), { status: 303 });
}
