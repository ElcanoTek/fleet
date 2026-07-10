import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ id: string }> };

// POST /api/remote-mcp-servers/{id}/shares — grant another user (or "*",
// everyone on the box) use of an owned remote MCP connection. The Connections
// page has posted here since the share UI shipped, but the proxy route was
// missing — the browser hit a Next 404 while the chat-server handler
// (internal/httpapi/remote_mcp.go) sat unreachable. Owner-scoping and grantee
// validation are enforced upstream.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { id } = await context.params;
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/remote-mcp-servers/${encodeURIComponent(id)}/shares`,
    { method: "POST", body, headers: { "Content-Type": "application/json" } },
  );
  if (error) return error;
  if (upstream.status === 204) {
    return NextResponse.json({ ok: true });
  }
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
