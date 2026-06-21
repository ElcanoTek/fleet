import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

/**
 * GET /api/conversations/:id/stream
 *
 * Reattach proxy. The client calls this after a page refresh or mobile
 * screen-lock to resume an in-flight turn's SSE stream. Forwards the
 * `Last-Event-ID` header and any `turn_id` query param to chat-server,
 * which replays buffered events after that id and then streams any new
 * ones live.
 *
 * No CSRF check — GETs are safe, and the handler reads nothing sensitive
 * beyond the session-scoped conversation.
 */
export async function GET(request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;

  // Preserve turn_id filter and any future passthrough params.
  const qs = request.nextUrl.searchParams.toString();
  const path = `/conversations/${encodeURIComponent(conversationId)}/stream${qs ? `?${qs}` : ""}`;

  // Forward Last-Event-ID so chat-server picks up after the client's
  // last-seen event. EventSource auto-attaches this header; since we
  // use fetch streams, the client sets it explicitly.
  const headers = new Headers();
  const lastEventID = request.headers.get("Last-Event-ID");
  if (lastEventID) {
    headers.set("Last-Event-ID", lastEventID);
  }

  let upstream: Response;
  try {
    upstream = await chatServerFetch(session.email, path, {
      method: "GET",
      headers,
      signal: request.signal,
    });
  } catch (err) {
    return NextResponse.json(
      { error: `chat-server unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  // 204 = no buffer to replay; client should fall back to /conversations/:id.
  if (upstream.status === 204) {
    return new NextResponse(null, { status: 204 });
  }
  if (!upstream.ok || !upstream.body) {
    const text = await upstream.text().catch(() => upstream.statusText);
    return new NextResponse(text, { status: upstream.status });
  }

  return new Response(upstream.body, {
    status: 200,
    headers: {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    },
  });
}
