import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

/**
 * GET /api/admin/stats — proxy to chat-server's /admin/stats. The upstream
 * handler enforces the ADMIN_EMAILS allowlist, so we don't duplicate that
 * check here; a non-admin user simply sees a 403 passed through.
 */
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const upstream = await chatServerFetch(session.email, "/admin/stats", { method: "GET" });
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
