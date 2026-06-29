import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * PATCH /api/conversations/bulk — apply the same additive mutation (pinned /
 * folder / labels) to multiple conversations in a single transaction (#279).
 * Forwards the JSON body { conversation_ids, changes } to the backend.
 */
export async function PATCH(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(session.email, "/conversations/bulk", {
    method: "PATCH",
    body,
  });
  if (error) return error;
  return passthrough(upstream);
}

async function passthrough(upstream: Response) {
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
