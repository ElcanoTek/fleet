import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// GET /api/mcp-servers
// Returns the Optional MCP server catalog with no per-conversation state.
// `enabled` is seeded from each server's `enabled_by_default` (so default-on
// connectors like gamma start toggled on for a fresh chat). The Tools picker
// calls this on startup so it can render for new chats before a conversation
// row exists. Once a conversation is open, /api/conversations/{id}/mcp-servers
// returns the same catalog merged with that conversation's opt-in list.
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const upstream = await chatServerFetch(session.email, "/mcp-servers", {
    method: "GET",
  });
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
