import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// POST /api/conversations/{id}/runtime — set the per-conversation runtime flavor
// (fleet's ACP runtime selection: native-inprocess | native-acp | ...). Pure
// passthrough to chat-server's member-gated endpoint, which validates the flavor
// against the client bundle's runtimes catalog. Body: { runtime: string }; an
// empty string clears the override (the bundle default applies next turn).
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const body = await request.text();

  const upstream = await chatServerFetch(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/runtime`,
    { method: "POST", body },
  );
  if (upstream.status === 204) {
    return NextResponse.json({ ok: true });
  }
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
