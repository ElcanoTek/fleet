import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/notify-settings — proxy to chat-server's /admin/notify-settings
 * (admin-managed task notifications). Upstream enforces admin authorization
 * and validation. Secret values (SMTP password, webhook signing secret)
 * travel one way (browser → server) and never appear in any response.
 */

export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  return chatServerPassthrough(session, "/admin/notify-settings", { method: "GET" });
}

async function mutate(request: NextRequest, method: "PUT" | "DELETE"): Promise<NextResponse> {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  // Defense-in-depth on top of the SameSite=Lax session cookie — this config
  // holds write-only credentials and steers outbound notifications.
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const body = method === "PUT" ? await request.text() : undefined;
  return chatServerPassthrough(session, "/admin/notify-settings", { method, body });
}

export async function PUT(request: NextRequest) {
  return mutate(request, "PUT");
}

export async function DELETE(request: NextRequest) {
  return mutate(request, "DELETE");
}
