import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// GET /api/oauth/mcp/callback — the registered OAuth redirect URI for per-user
// remote MCP login (#443). The authorization server redirects the browser here
// with ?code&state (or ?error). This route is an OPAQUE relay: it forwards
// code/state/error to chat-server (which validates the single-use state, checks
// the completing user matches the initiator, and exchanges the code with the
// PKCE verifier), then redirects the browser to the connections settings page.
// The authorization code is single-use and never logged.
//
// It is a top-level navigation (GET) from an external origin, so there is no
// CSRF token to check; the security comes from the unguessable, user-bound,
// single-use `state` validated server-side.
export async function GET(request: NextRequest) {
  const url = new URL(request.url);
  const code = url.searchParams.get("code") ?? "";
  const state = url.searchParams.get("state") ?? "";
  const oauthError = url.searchParams.get("error") ?? "";

  const settings = (params: string) => NextResponse.redirect(new URL(`/settings/connections${params}`, request.url));

  const session = await getServerSession();
  if (!session) {
    // Not logged in — bounce to login; the user can retry the connection after.
    return NextResponse.redirect(new URL("/login", request.url));
  }

  if (oauthError) {
    return settings(`?error=${encodeURIComponent(oauthError)}`);
  }
  if (!code || !state) {
    return settings("?error=missing_code_or_state");
  }

  const { upstream, error } = await chatServerProxy(session.email, "/oauth/mcp/callback", {
    method: "POST",
    body: JSON.stringify({ code, state }),
  });
  if (error) {
    return settings("?error=backend_unreachable");
  }
  if (upstream.status >= 200 && upstream.status < 300) {
    let name = "";
    try {
      const data = (await upstream.json()) as { name?: string };
      name = data.name ?? "";
    } catch {
      // ignore body parse issues — the connection still succeeded
    }
    return settings(`?connected=${encodeURIComponent(name || "1")}`);
  }
  // Surface a non-secret failure reason.
  let detail = "authorization_failed";
  try {
    const data = (await upstream.json()) as { detail?: string; error?: string };
    detail = data.detail || data.error || detail;
  } catch {
    // keep the generic reason
  }
  return settings(`?error=${encodeURIComponent(detail)}`);
}
