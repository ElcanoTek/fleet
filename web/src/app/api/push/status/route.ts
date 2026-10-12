import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

// POST /api/push/status — whether the signed-in user holds the subscription
// for the given endpoint: {"subscribed": bool}. The settings switch asks this
// about the browser's own subscription, which may be another account's (a
// shared browser) or one the server has dropped. The endpoint is a
// capability URL, so it rides in the body. 501 when VAPID keys are unset.
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(session, "/push/status", {
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
