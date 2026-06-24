import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

/**
 * POST /api/conversations/{id}/summarize
 *
 * Run the user-initiated "summarize and continue" flow on this
 * conversation. Body: `{ "model": "<openrouter-slug>" }` (optional —
 * chat-server falls back to the conversation's stored slug).
 *
 * Response is text/event-stream so the summary text streams to the
 * caller as it generates (a 30-60s blocking spinner reads as
 * broken). Events:
 *
 *   event: summary.delta       data: {"text": "<chunk>"}
 *   event: summary.completed   data: {type, role, text, model, ...}
 *   event: summary.error       data: {"message": "..."}
 *
 * Pre-stream errors (validation, in-flight conflict, missing model)
 * still come back as plain HTTP error codes — the UI distinguishes
 * those from mid-stream model failures.
 */
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/summarize`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: body.length > 0 ? body : "{}",
    },
  );
  if (error) return error;
  // Stream upstream body straight through. text/event-stream needs the
  // body to flow chunk-by-chunk; reading it via .text() would buffer
  // the whole summary and defeat the streaming UX.
  return new NextResponse(upstream.body, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("Content-Type") ?? "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
    },
  });
}
