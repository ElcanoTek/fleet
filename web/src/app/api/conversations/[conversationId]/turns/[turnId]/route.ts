import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; turnId: string }>;
};

/**
 * GET /api/conversations/:id/turns/:turnId
 *
 * What the server recorded about ONE turn (#1593). /inflight answers a
 * question about the conversation ("is anything running") and the history
 * answers another ("is there an answer"); a client holding an open assistant
 * slot needs neither — it needs to know what became of its own turn. Shape:
 *
 *   { state: "running", user_committed: false }
 *   { state: "failed", reason: "model_required", detail: {...},
 *     user_committed: true, finished_at: N }
 *
 * 404 means the server has no such turn for this caller's conversation, which
 * the client reads as "no answer available" rather than as a verdict.
 */
export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, turnId } = await context.params;

  let upstream: Response;
  try {
    upstream = await chatServerFetch(
      session,
      `/conversations/${encodeURIComponent(conversationId)}/turns/${encodeURIComponent(turnId)}`,
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
