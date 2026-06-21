import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

/**
 * GET /api/conversations/:id/inflight
 *
 * Tiny JSON probe — the client calls this on mount / visibilitychange /
 * network-reconnect to decide whether to open a reattach stream. Shape:
 *
 *   { inflight: false }
 *   { inflight: true, turn_id: "...", last_event_id: N }
 *
 * Cheaper than blindly opening SSE just to discover "nothing's happening".
 */
export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;

  let upstream: Response;
  try {
    upstream = await chatServerFetch(
      session.email,
      `/conversations/${encodeURIComponent(conversationId)}/inflight`,
      { method: "GET" },
    );
  } catch (err) {
    return NextResponse.json(
      { error: `chat-server unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("Content-Type") ?? "application/json",
    },
  });
}
