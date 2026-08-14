import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; childId: string }>;
};

/**
 * GET /api/conversations/:id/subagents/:childId (#1043)
 *
 * A chat-spawned sub-agent's own transcript (its sibling session log).
 * Ownership + history-linkage + id validation live in chat-server
 * (handleSubagentLog); this route is a thin authenticated proxy.
 */
export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, childId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/subagents/${encodeURIComponent(childId)}`,
    { method: "GET" },
  );
  if (error) return error;
  const body = await upstream.text();
  return new NextResponse(body, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("Content-Type") ?? "application/json",
    },
  });
}
