import { NextRequest, NextResponse } from "next/server";
import { verifyOrigin } from "@/app/lib/csrf";
import { getOrchestratorBase } from "@/app/lib/orchestratorServer";

export const runtime = "nodejs";

// POST /api/orchestrator/auth/login → orchestrator POST /auth/login
//
// moc's username/password login path (API/CLI clients). The browser no
// longer stores the returned bearer (#1115); the web UI authenticates
// via the httpOnly chat/elcano session cookie. This route stays public
// so a bearer client can still obtain a token.
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
