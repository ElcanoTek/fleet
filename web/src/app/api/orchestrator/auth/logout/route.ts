import { NextRequest, NextResponse } from "next/server";
import { verifyOrigin } from "@/app/lib/csrf";
import { getOrchestratorBase } from "@/app/lib/mocServer";

export const runtime = "nodejs";

// POST /api/orchestrator/auth/logout → orchestrator POST /auth/logout
//
// Clears the shared httpOnly elcano_auth cookie (JS can't) so the user is
// signed out of all Elcano services. POST so the Origin CSRF check applies.
// The browser separately drops its stored moc bearer token.
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const auth = request.headers.get("authorization") ?? "";
  let upstream: Response;
  try {
    upstream = await fetch(`${getOrchestratorBase()}/auth/logout`, {
      method: "POST",
      headers: auth ? { Authorization: auth } : {},
      cache: "no-store",
    });
  } catch (err) {
    return NextResponse.json(
      { detail: `orchestrator unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  // Forward any Set-Cookie (cookie deletion) the orchestrator emits.
  const headers = new Headers({
    "Content-Type": upstream.headers.get("Content-Type") ?? "application/json",
  });
  const setCookie = upstream.headers.get("set-cookie");
  if (setCookie) headers.set("set-cookie", setCookie);

  const text = await upstream.text();
  return new NextResponse(text, { status: upstream.status, headers });
}
