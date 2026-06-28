import { NextRequest, NextResponse } from "next/server";
import { buildElcanoLoginUrl, getAuthSigningPubkey, getRedirectUrl } from "@/app/lib/auth";

/**
 * GET /api/auth/elcano-login
 *
 * Target of the "Use Elcano email" button on the login card. Bounces the
 * browser to the auth service's magic-link login (auth.elcanotek.com),
 * signed back to chat's home page. After the user clicks the emailed link,
 * auth sets the shared `elcano_auth` cookie and redirects here; chat then
 * verifies that cookie natively and chat-server gates on the user-list.
 *
 * Kept as a server route (rather than a bare <a> to the auth host) so
 * AUTH_LOGIN_URL stays server-side config and `return_to` is built from the
 * real request host (works in dev and prod without a hardcoded origin).
 */
export async function GET(request: NextRequest) {
  // Without a configured public key, chat can never verify the elcano_auth
  // cookie auth would mint — sending the user there would trap them in a
  // redirect loop (auth sets the cookie, chat can't read it, back to /login,
  // click again, repeat). Bounce to the password login with a message instead.
  if (!getAuthSigningPubkey()) {
    return NextResponse.redirect(getRedirectUrl(request, "/login?e=elcano_unavailable"), { status: 303 });
  }
  const returnTo = getRedirectUrl(request, "/").toString();
  return NextResponse.redirect(buildElcanoLoginUrl(returnTo), { status: 303 });
}
