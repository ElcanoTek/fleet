import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

// POST /api/push/renew — after a VAPID key rotation the browser subscribes
// again; this moves the signed-in user's stored row from the old endpoint to
// the new subscription. Body: { old_endpoint, endpoint, keys }. Answers
// {"renewed": bool}: false (nothing changed) unless the user held the old
// row. 501 when the operator has not configured VAPID keys.
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(session, "/push/renew", {
    method: "POST",
    body,
  });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
