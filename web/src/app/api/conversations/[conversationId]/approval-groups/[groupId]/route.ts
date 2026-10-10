import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; groupId: string }>;
};

/**
 * POST /api/conversations/{id}/approval-groups/{groupId}
 *
 * One decision for a turn's grouped approval cards
 * (docs/GROUPED-APPROVALS.md). Body: { approve: string[], decline: string[] }.
 * The answer lists each card's outcome, as its own approval POST would have.
 */
export async function POST(req: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, groupId } = await context.params;
  const body = await req.text();
  const { upstream, error } = await chatServerProxy(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/approval-groups/${encodeURIComponent(groupId)}`,
    { method: "POST", body },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
