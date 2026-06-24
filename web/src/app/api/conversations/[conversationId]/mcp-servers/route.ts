import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// GET /api/conversations/{id}/mcp-servers
// Returns the catalog of Optional MCP servers the user may toggle, with
// each one's current opt-in state for this conversation. Non-optional
// servers are omitted — the UI only renders rows for togglable ones.
//
// Response shape (mirrors the chat-server handler):
//   { servers: [{ name, description, tools, tool_count, enabled }] }
export async function GET(request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/mcp-servers`,
    { method: "GET" },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

// POST /api/conversations/{id}/mcp-servers
// Body: { enabled_optional: ["gamma", ...] }  — the FULL list; the server
// replaces any prior state. The chat-server intersects the request list
// with its authoritative catalog so clients can't persist unknown names.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/mcp-servers`,
    { method: "POST", body },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
