import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ id: string }> };

// POST /api/remote-mcp-servers/{id}/authorize — begin the OAuth login flow for a
// remote MCP server (#443). The backend stores a single-use PKCE/CSRF state bound
// to this user and returns { redirect_url }; the browser then navigates there to
// authorize. The AS redirects back to /api/oauth/mcp/callback.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { id } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/remote-mcp-servers/${encodeURIComponent(id)}/authorize`,
    { method: "POST" },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
