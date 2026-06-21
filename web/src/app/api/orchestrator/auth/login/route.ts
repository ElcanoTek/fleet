import { NextRequest, NextResponse } from "next/server";
import { verifyOrigin } from "@/app/lib/csrf";
import { getOrchestratorBase } from "@/app/lib/mocServer";

export const runtime = "nodejs";

// POST /api/orchestrator/auth/login → orchestrator POST /auth/login
//
// moc's username/password login path. The form posts {username, password};
// the orchestrator returns {token, user}. The browser stores the bearer token
// and sends it on subsequent /api/orchestrator/* calls. This route is public
// (no session required) so the user can obtain the bearer in the first place.
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const body = await request.text();
  let upstream: Response;
  try {
    upstream = await fetch(`${getOrchestratorBase()}/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body,
      cache: "no-store",
    });
  } catch (err) {
    return NextResponse.json(
      { detail: `orchestrator unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
