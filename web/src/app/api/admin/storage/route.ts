import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// Admin-only read-through to the chat server's storage accounting
// (uploads / temp files / workspaces byte totals, largest workspaces,
// reclaimable-conversation counts). The upstream admin middleware
// remains the authorization boundary.
export async function GET() {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  return chatServerPassthrough(session, "/admin/storage", { method: "GET" });
}
