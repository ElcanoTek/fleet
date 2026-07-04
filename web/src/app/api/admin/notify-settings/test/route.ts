import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/notify-settings/test — proxy to chat-server's
 * POST /admin/notify-settings/test: one real delivery attempt of a synthetic
 * event over the requested channel. The response is a key-free
 * {ok, detail, latency_ms}.
 */

export async function POST(request: NextRequest) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const body = await request.text();
  return chatServerPassthrough(session.email, "/admin/notify-settings/test", {
    method: "POST",
    body,
  });
}
