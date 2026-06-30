import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

/**
 * GET /api/admin/users — proxy to chat-server's /admin/users (#237). The
 * upstream handler enforces admin authorization (ADMIN_EMAILS env allowlist OR
 * a users.role='admin' account), so we don't duplicate that check here; a
 * non-admin simply sees a 403 passed through.
 */
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { upstream, error } = await chatServerProxy(session.email, "/admin/users", { method: "GET" });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
