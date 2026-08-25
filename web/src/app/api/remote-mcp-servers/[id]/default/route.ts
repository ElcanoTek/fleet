import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ id: string }> };

// POST /api/remote-mcp-servers/{id}/default — make this seat (login) the
// default among the caller's seats with the same connection name (#988).
// Chats and tasks mount the default seat unless a conversation or task picks
// another. No body; the backend answers 204.
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
    `/remote-mcp-servers/${encodeURIComponent(id)}/default`,
    { method: "POST" },
  );
  if (error) return error;
  if (upstream.status === 204) return new NextResponse(null, { status: 204 });
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
