import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

/**
 * POST /api/conversations/{id}/truncate
 *
 * Used by the Retry, Regenerate, and Edit UI flows. Drops every message
 * after the latest user message (default) or also drops the latest user
 * message itself (`?mode=edit_last`). No body required. The query string
 * is forwarded verbatim to chat-server, which owns the mode logic.
 */
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const query = request.nextUrl.search; // "?mode=edit_last" or ""
  const upstream = await chatServerFetch(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/truncate${query}`,
    { method: "POST" },
  );
  if (upstream.status === 204) {
    return NextResponse.json({ ok: true });
  }
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
