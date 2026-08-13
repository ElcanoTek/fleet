import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ id: string }> };

// POST /api/remote-mcp-servers/{id}/signout — end the authorization for an
// OAuth remote MCP connection without deleting it: the backend revokes the
// token (best effort) and drops it, keeping the registration + client
// credentials so Reconnect works without re-entering anything.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { id } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session,
    `/remote-mcp-servers/${encodeURIComponent(id)}/signout`,
    { method: "POST" },
  );
  if (error) return error;
  return new NextResponse(null, { status: upstream.status });
}
