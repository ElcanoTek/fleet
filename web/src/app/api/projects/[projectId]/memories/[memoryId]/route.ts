import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string; memoryId: string }> };

// PATCH one team learning — edit / pin / retire (ADR-0057). "Retire" is the
// default remove: the entry stops being injected but the record, and who wrote
// it, survives. The author-or-project-owner permission is the Go handler's
// gate; this proxy only authenticates and forwards.
export async function PATCH(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId, memoryId } = await params;
  const { upstream, error } = await chatServerProxy(
    session,
    `/projects/${encodeURIComponent(projectId)}/memories/${encodeURIComponent(memoryId)}`,
    { method: "PATCH", body: await request.text() },
  );
  if (error) return error;
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

// DELETE one shared project memory (#509).
export async function DELETE(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId, memoryId } = await params;
  const { upstream, error } = await chatServerProxy(
    session,
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
