import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string; memoryId: string }> };

// DELETE one shared project memory (#509).
export async function DELETE(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId, memoryId } = await params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/projects/${encodeURIComponent(projectId)}/memories/${encodeURIComponent(memoryId)}`,
    { method: "DELETE" },
  );
  if (error) return error;
  if (upstream.status === 204) return new NextResponse(null, { status: 204 });
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "text/plain" },
  });
}
