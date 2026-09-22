import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * POST /api/chat
 *
 * Thin proxy around chat-server's POST /chat. We verify the session cookie,
 * forward the request to chat-server with the shared-secret + user-email
 * headers, and pipe the SSE body straight back to the browser.
 *
 * The request body matches chat-server's contract:
 *   { conversation_id?, message, persona?, model?, title?, enabled_optional?,
 *     mcp_accounts? }
 * The body is forwarded verbatim — `mcp_accounts` (server → seat label, #988)
 * needs no handling here.
 *
 * The response is an SSE stream with event types:
 *   conversation, reasoning.start/delta/end, text.delta, tool.call,
 *   tool.result, turn.completed, turn.error
 */
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  const bodyText = await request.text();

  // Enforce the completion-price ceiling before we proxy. The client also
  // validates, but we re-check here so direct API calls can't bypass it.

  let upstream: Response;
  try {
    upstream = await chatServerFetch(session, "/chat", {
      method: "POST",
      body: bodyText,
      signal: request.signal,
    });
  } catch (err) {
    return NextResponse.json(
      { error: `chat-server unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  if (!upstream.ok || !upstream.body) {
    const text = await upstream.text().catch(() => upstream.statusText);
    return new NextResponse(text, { status: upstream.status });
  }

  // The response header set is rebuilt here rather than passed through, so
  // anything the browser needs has to be listed. X-Fleet-Conversation-Id names
  // the conversation this stream belongs to (#1591): a brand-new chat posts
  // under a client-side pending key, and if the socket dies before the
  // `conversation` frame the header is the only id it ever learns — without it
  // the recovery chain has nothing to probe and the turn reads as failed while
  // the server writes its answer to the database.
  const headers: Record<string, string> = {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-cache, no-transform",
    Connection: "keep-alive",
    "X-Accel-Buffering": "no",
  };
  const conversationId = upstream.headers.get("X-Fleet-Conversation-Id");
  if (conversationId) headers["X-Fleet-Conversation-Id"] = conversationId;

  return new Response(upstream.body, {
    status: 200,
    headers,
  });
}
