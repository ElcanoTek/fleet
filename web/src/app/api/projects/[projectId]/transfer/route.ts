import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// POST /api/projects/{id}/transfer → hand the project to another member
// (ADR-0057). Body: { to_email }. Authorization is the Go handler's — the
// owner, or an admin cleaning up after an owner who left — and a caller who is
// neither gets the same 404 a non-member gets for any project subresource.
export async function POST(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  const { upstream, error } = await chatServerProxy(
    session,
    `/projects/${encodeURIComponent(projectId)}/transfer`,
    { method: "POST", body: await request.text() },
  );
  if (error) return error;
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
