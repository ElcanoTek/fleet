import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; approvalId: string }>;
};

/**
 * POST /api/conversations/{id}/approvals/{approvalId}/stop
 *
 * Stop a running approved call of a tool the bundle lists in
 * agent_policy.critical_tool_stop (docs/APPROVAL-STOP.md): the chat server
 * records the stop and asks the tool to halt after its current item. The
 * answer is the card's state (still executing and stopping, or settled).
 */
export async function POST(req: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, approvalId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/approvals/${encodeURIComponent(approvalId)}/stop`,
    { method: "POST" },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
