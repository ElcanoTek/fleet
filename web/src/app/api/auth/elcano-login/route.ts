import { NextRequest } from "next/server";
import { elcanoLoginRedirect } from "@/app/lib/auth";

/**
 * GET /api/auth/elcano-login
 *
 * Target of the "Use Elcano email" button on the chat login card. Bounces the
 * browser to the auth service's magic-link login (auth.elcanotek.com), signed
 * back to chat's home page. After the user clicks the emailed link, auth sets
 * the shared `elcano_auth` cookie and redirects here; the app then verifies that
 * cookie natively and chat-server gates on the user-list.
 *
 * The handoff itself lives in the shared elcanoLoginRedirect helper (the
 * orchestrator's elcano-login route is the same call with a different return
 * path), kept server-side so AUTH_LOGIN_URL stays config and `return_to` is
 * built from the real request host (works in dev and prod without a hardcoded
 * origin).
 */
export async function GET(request: NextRequest) {
  return elcanoLoginRedirect(request, "/");
}
