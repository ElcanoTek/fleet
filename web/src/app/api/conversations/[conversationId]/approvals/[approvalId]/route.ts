import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; approvalId: string }>;
};

/**
 * POST /api/conversations/{id}/approvals/{approvalId}
 *
 * Approve or reject a staged high-risk tool call (currently send_email).
 * Body: { approved: boolean }.
 */
export async function POST(req: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, approvalId } = await context.params;
  const body = await req.text();
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/approvals/${encodeURIComponent(approvalId)}`,
    { method: "POST", body },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
