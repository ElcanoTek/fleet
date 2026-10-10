import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; approvalId: string }>;
};

/**
 * GET /api/conversations/{id}/approvals/{approvalId}
 *
 * One approval card's current state (the chat server's one-card read,
 * /conversations/{id}?approval_id=…): a running card of a tool that reports
 * progress polls it for the latest progress and its outcome
 * (docs/APPROVAL-PROGRESS.md).
 */
export async function GET(_: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, approvalId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session,
    `/conversations/${encodeURIComponent(conversationId)}?approval_id=${encodeURIComponent(approvalId)}`,
    { method: "GET" },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

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
    session,
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
