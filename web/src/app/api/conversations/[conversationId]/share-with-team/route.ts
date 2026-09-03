import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ conversationId: string }> };

// POST /api/conversations/{id}/share-with-team → the owner opts this chat in
// or out of read-only visibility for their team (ADR-0013 / ADR-0057). Body:
// { visible: boolean }. Ownership is the Go handler's gate; this proxy only
// authenticates and forwards.
export async function POST(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { conversationId } = await params;
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/share-with-team`,
    { method: "POST", body },
  );
  if (error) return error;
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
