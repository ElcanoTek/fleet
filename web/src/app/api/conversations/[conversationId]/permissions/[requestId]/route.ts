import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; requestId: string }>;
};

/**
 * POST /api/conversations/{id}/permissions/{requestId}
 *
 * Allow or deny an EXTERNAL ACP agent's session/request_permission. The agent
 * (Claude Code / Goose) self-executes in a locked sandbox and blocks its turn
 * until this decision arrives — or the server's default-deny timeout fires.
 * Body: { allowed: boolean, option_id?: string }. Pure passthrough to
 * chat-server's member-gated endpoint.
 */
export async function POST(req: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, requestId } = await context.params;
  const body = await req.text();
  const upstream = await chatServerFetch(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/permissions/${encodeURIComponent(requestId)}`,
    { method: "POST", body },
  );
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
