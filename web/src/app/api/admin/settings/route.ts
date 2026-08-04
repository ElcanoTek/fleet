import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

/**
 * /api/admin/settings — proxy to chat-server's GET /admin/settings (the admin
 * Features panel: every registered workspace feature setting with its
 * effective value, provenance, and default). Upstream enforces admin
 * authorization; a non-admin sees the 403 passed through. No secret material
 * flows here — the registry is feature toggles and numeric bounds only.
 */

export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  return chatServerPassthrough(session, "/admin/settings", { method: "GET" });
}
