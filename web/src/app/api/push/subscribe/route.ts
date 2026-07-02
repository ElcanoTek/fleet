import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

// POST /api/push/subscribe — store the browser's PushSubscription for the
// signed-in user (#292). The body is the subscription's toJSON() shape
// (endpoint + keys); the chat server upserts it keyed on the endpoint.
// Returns 204, or 501 when the operator has not configured VAPID keys.
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(session.email, "/push/subscribe", {
    method: "POST",
    body,
  });
  if (error) return error;
  if (upstream.status === 204) return new NextResponse(null, { status: 204 });
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
