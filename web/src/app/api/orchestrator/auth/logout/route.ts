import { NextRequest, NextResponse } from "next/server";
import { verifyOrigin } from "@/app/lib/csrf";
import { getOrchestratorBase } from "@/app/lib/orchestratorServer";

export const runtime = "nodejs";

// POST /api/orchestrator/auth/logout → orchestrator POST /auth/logout
//
// Ends the orchestrator's own server-side state for this user. It does NOT
// sign the user out of Fleet or of central Auth: that is /api/auth/logout,
// which every surface (the Operations Center included) submits as a
// top-level form after this best-effort call (shared/signOut.ts). POST so
// the Origin CSRF check applies.
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
