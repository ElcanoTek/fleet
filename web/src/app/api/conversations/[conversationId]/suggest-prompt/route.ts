import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// POST /api/conversations/{id}/suggest-prompt → distills the conversation into
// a prompt-library draft ({name, description, content}). Proxies to the chat
// server's /conversations/{id}/suggest-prompt; nothing is persisted — the
// client shows the draft in an editable review dialog and saves the approved
// version through the orchestrator's existing POST /prompts.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/suggest-prompt`,
    { method: "POST" },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: {
      "Content-Type":
        upstream.headers.get("Content-Type") ?? "application/json",
    },
  });
}
